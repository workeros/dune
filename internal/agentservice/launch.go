// Package agentservice coordinates application metadata with authorized Dune
// execution. It owns neither Runtime queues nor durable background jobs.
package agentservice

import (
	"context"
	"errors"
	"maps"
	"path"
	"slices"
	"time"

	"github.com/aiomni/dune/internal/authorization"
	"github.com/aiomni/dune/internal/metadata"
	"github.com/aiomni/dune/pkg/agents"
	"github.com/aiomni/dune/pkg/api"
	"github.com/aiomni/dune/pkg/client"
	"github.com/aiomni/dune/pkg/runner"
	"github.com/aiomni/dune/pkg/workbench"
)

type Dial func(context.Context, agents.Scope, runner.Binding, string) (*client.Client, func(), error)

type Service struct {
	Store       *metadata.Store
	Access      *authorization.Service
	Dial        Dial
	Environment agents.EnvironmentResolver
	Online      func(context.Context, []string) (map[string]bool, error)
	MCPURL      string
}

func invalid(detail string) error { return &api.Error{Code: "INVALID_ARGUMENT", Detail: detail} }

// Start resolves the fixed Profile/project revisions before any mutation. The
// optional worktree is created once, followed by a committed launch snapshot,
// then a single profile.start over SDK/Gateway. Errors never trigger a replay.
func (s *Service) Start(ctx context.Context, scope agents.Scope, request agents.StartRequest) (result agents.LaunchResult, err error) {
	defer func() {
		if result.Runtime != nil {
			result.AgentRef = agentRef(targetFor(request.Binding, *result.Runtime), result.Runtime.NativeSession)
		}
	}()
	ctx, cancel := context.WithTimeout(ctx, 5*time.Minute)
	defer cancel()
	resource, _, err := s.Access.Resource(ctx, scope.Principal, request.Binding.RunnerID, false, "profile.start")
	if err != nil {
		return result, err
	}
	if scope.OwnerID == "" || resource.OwnerID != scope.OwnerID {
		return result, metadata.ErrNotFound
	}
	if resource.Runner.Binding == nil || *resource.Runner.Binding != request.Binding {
		return result, runner.ErrBindingChanged
	}
	launch, err := s.resolveLaunch(ctx, scope.OwnerID, request)
	if err != nil {
		return result, err
	}
	if s.Environment != nil {
		launch.Profile.Env, err = s.Environment(ctx, scope, request.Binding, maps.Clone(launch.Profile.Env))
		if err != nil {
			return result, err
		}
	}
	connection, closeConnection, err := s.Dial(ctx, scope, request.Binding, "profile.start")
	if err != nil {
		return result, err
	}
	defer closeConnection()
	if !slices.Contains(connection.Binding.Capabilities, "profile.start") {
		return result, &api.Error{Code: "UNSUPPORTED", Detail: "Runner does not support Agent startup"}
	}
	if launch.Profile.RequireAgentMCP && !slices.Contains(connection.Binding.Capabilities, "acp.mcp.configure") {
		return result, &api.Error{Code: "UNSUPPORTED", Detail: "Runner does not support Agent MCP configuration"}
	}
	var machine api.MachineInfo
	if err := connection.Call(ctx, "machine.info", struct{}{}, &machine); err != nil {
		return result, err
	}
	if machine.UserID == "" || machine.Home == "" {
		return result, &api.Error{Code: "UNSUPPORTED", Detail: "Runner does not report its execution user/storage"}
	}
	launch.Storage = storageIdentity(machine, launch.Profile)
	if err := launch.Validate(); err != nil {
		return result, invalid(err.Error())
	}
	if request.Worktree != nil {
		if !slices.Contains(connection.Binding.Capabilities, "worktree.create") {
			return result, &api.Error{Code: "UNSUPPORTED", Detail: "Runner does not support worktree creation"}
		}
		location := request.Worktree
		created, err := connection.CreateWorktree(ctx, api.WorktreeCreate{Directory: launch.Profile.WorkingDirectory, Path: location.Path, Branch: location.Branch, Ref: location.Ref})
		if err != nil {
			return result, err
		}
		result.Worktree = &created
		launch.Profile.WorkingDirectory = created.Path
		// The project still owns this session, but a new worktree is not the
		// source directory. Do not rewrite the shared project behind the user.
		launch.DirectoryID = ""
	}
	session, err := s.Store.CreateAgentSession(ctx, scope.OwnerID, launch)
	if err != nil {
		return result, err
	}
	setSession := func(value agents.Session) { summary := value.Summary(); result.Session = &summary }
	setSession(session)
	runtime, stream, startErr := connection.Start(ctx, session.Launch.Profile)
	if stream != nil {
		_ = stream.Close()
	}
	// Save a confirmed result even if the browser disconnected meanwhile.
	saveCtx, saveCancel := context.WithTimeout(context.WithoutCancel(ctx), 3*time.Second)
	defer saveCancel()
	if startErr != nil {
		outcome := startOutcome(startErr)
		failed, saveErr := s.Store.FailAgentAttempt(saveCtx, scope.OwnerID, session.ID, session.Attempt.ID, outcome, "Agent startup did not return a confirmed Runtime; inspect the saved attempt before another start")
		if saveErr == nil {
			setSession(failed)
		}
		if outcome == "unknown" {
			return result, &api.Error{Code: "RESULT_UNKNOWN", Detail: "Agent startup result is unknown; it was not replayed"}
		}
		return result, &api.Error{Code: "START_FAILED", Detail: "Runner rejected Agent startup; inspect the configured command and Runner prerequisites"}
	}
	result.Runtime = &runtime
	ref := workbench.RuntimeRef{ID: runtime.ID, Incarnation: runtime.Incarnation, Generation: runtime.Generation, Adapter: runtime.Adapter}
	session, err = s.Store.RecordAgentRuntime(saveCtx, scope.OwnerID, session.ID, session.Attempt.ID, ref)
	if err != nil {
		return result, &api.Error{Code: "RECOVERY_INDEX_FAILED", Detail: "Agent is running, but its Runtime could not be saved in the recovery index; do not start it again"}
	}
	setSession(session)
	if launch.Profile.Adapter == "pty" && launch.Recovery.ID == "" {
		session, err = s.Store.AgentCaptureUnavailable(saveCtx, scope.OwnerID, session.ID, session.Attempt.ID, "This launch has no native session capture adapter")
		if err != nil {
			return result, &api.Error{Code: "RECOVERY_INDEX_FAILED", Detail: "Agent is running, but recovery availability could not be saved"}
		}
		setSession(session)
	}
	if runtime.Adapter == "acp" {
		return s.initializeACP(ctx, scope, connection, result)
	}
	return result, nil
}

func storageIdentity(machine api.MachineInfo, profile api.Profile) string {
	home := machine.Home
	if configured, ok := profile.Env["HOME"]; ok {
		home = configured
	}
	return "uid:" + machine.UserID + ":home:" + home
}

func startOutcome(err error) string {
	var failure *api.Error
	if errors.As(err, &failure) {
		switch failure.Code {
		case "INVALID_ARGUMENT", "UNSUPPORTED", "RESOURCE_EXHAUSTED", "START_FAILED", "PROFILE_SETUP_FAILED", "ACCESS_DENIED":
			return "failed"
		}
	}
	return "unknown"
}

func (s *Service) resolveLaunch(ctx context.Context, owner string, request agents.StartRequest) (agents.LaunchSnapshot, error) {
	launch := agents.LaunchSnapshot{Binding: request.Binding}
	if request.Profile != nil && request.Custom != nil {
		return launch, invalid("choose a saved Profile or a custom launch")
	}
	selection := request.Profile
	directory := request.WorkingDirectory
	if request.Project != nil {
		selected := request.Project
		if selected.ID == "" || selected.Revision < 1 {
			return launch, invalid("project requires a fixed revision")
		}
		project, err := s.Store.Project(ctx, owner, selected.ID)
		if err != nil {
			return launch, err
		}
		if project.Revision != selected.Revision {
			return launch, metadata.ErrConflict
		}
		launch.ProjectID = project.ID
		if selection == nil && request.Custom == nil {
			selection = project.DefaultProfile
		}
		if request.DirectoryID != "" {
			index := slices.IndexFunc(project.Directories, func(item workbench.Directory) bool { return item.ID == request.DirectoryID })
			if index < 0 {
				return launch, metadata.ErrNotFound
			}
			selectedDirectory := project.Directories[index]
			if selectedDirectory.Binding != request.Binding {
				return launch, runner.ErrBindingChanged
			}
			if directory != "" && directory != selectedDirectory.Path {
				return launch, invalid("selected directory differs from the requested working directory")
			}
			directory, launch.DirectoryID = selectedDirectory.Path, selectedDirectory.ID
		}
	} else if request.DirectoryID != "" {
		return launch, invalid("a saved directory requires a project")
	}
	if request.Custom != nil {
		launch.Profile = *request.Custom
	} else {
		if selection == nil || selection.Revision < 1 {
			return launch, invalid("select a fixed Agent Profile revision")
		}
		record, err := s.Store.Profiles().Get(ctx, owner, *selection)
		if err != nil {
			return launch, err
		}
		launch.Profile, launch.SourceProfile = record.Profile, selection
	}
	if directory != "" {
		launch.Profile.WorkingDirectory = directory
	}
	launch.Profile.ManagedACP = launch.Profile.Adapter == "acp"
	launch.Profile.RequireAgentMCP = launch.Profile.ManagedACP
	if err := launch.Profile.Validate(); err != nil {
		return launch, invalid(err.Error())
	}
	if launch.Profile.Kind != "agent" {
		return launch, invalid("an Agent Profile is required")
	}
	launch.AgentType = "custom"
	if len(launch.Profile.Start.Argv) > 0 {
		launch.AgentType = path.Base(launch.Profile.Start.Argv[0])
	}
	launch.Recovery = recoveryAdapter(launch.Profile)
	return launch, nil
}

// Opaque shell commands or arbitrary extra CLI arguments may include a one-shot
// task. Only recognized transport-only launches can be resumed automatically.
func recoveryAdapter(profile api.Profile) agents.RecoveryAdapter {
	argv := profile.Start.Argv
	if profile.Adapter != "acp" || len(argv) == 0 || profile.Start.Run != "" {
		return agents.RecoveryAdapter{}
	}
	executable := path.Base(argv[0])
	if (len(argv) == 2 && (argv[1] == "--acp" || executable == "opencode" && argv[1] == "acp" || executable == "gemini" && argv[1] == "--experimental-acp")) || (len(argv) == 1 && (executable == "codex-acp" || executable == "claude-agent-acp")) {
		return agents.RecoveryAdapter{ID: "acp-load", Version: 1}
	}
	return agents.RecoveryAdapter{}
}
