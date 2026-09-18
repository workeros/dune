// Package agentservice coordinates application metadata with authorized Dune
// execution. It owns neither Runtime queues nor durable background jobs.
package agentservice

import (
	"context"
	"errors"
	"maps"
	"slices"
	"time"

	"github.com/aiomni/dune/internal/agentintegration"
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
// optional worktree is created once, then a single profile.start is sent over
// SDK/Gateway. Errors never trigger a replay.
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
	profile, err := s.resolveLaunch(ctx, scope.OwnerID, request)
	if err != nil {
		return result, err
	}
	if s.Environment != nil {
		profile.Env, err = s.Environment(ctx, scope, request.Binding, maps.Clone(profile.Env))
		if err != nil {
			return result, err
		}
	}
	if err := profile.Validate(); err != nil {
		return result, invalid(err.Error())
	}
	connection, closeConnection, err := s.Dial(ctx, scope, request.Binding, "profile.start")
	if err != nil {
		return result, err
	}
	defer closeConnection()
	if !slices.Contains(connection.Binding.Capabilities, "profile.start") {
		return result, &api.Error{Code: "UNSUPPORTED", Detail: "Runner does not support Agent startup"}
	}
	if profile.RequireAgentMCP && !slices.Contains(connection.Binding.Capabilities, "agent.mcp.configure") {
		return result, &api.Error{Code: "UNSUPPORTED", Detail: "Runner does not support Agent MCP configuration"}
	}
	if request.Worktree != nil {
		if !slices.Contains(connection.Binding.Capabilities, "worktree.create") {
			return result, &api.Error{Code: "UNSUPPORTED", Detail: "Runner does not support worktree creation"}
		}
		location := request.Worktree
		created, err := connection.CreateWorktree(ctx, api.WorktreeCreate{Directory: profile.WorkingDirectory, Path: location.Path, Branch: location.Branch, Ref: location.Ref})
		if err != nil {
			return result, err
		}
		result.Worktree = &created
		profile.WorkingDirectory = created.Path
		// The project still owns this session, but a new worktree is not the
		// source directory. Do not rewrite the shared project behind the user.
		profile.DirectoryID = ""
	}
	runtime, stream, startErr := connection.Start(ctx, profile)
	if stream != nil {
		_ = stream.Close()
	}
	if startErr != nil {
		if startOutcome(startErr) == "unknown" {
			return result, &api.Error{Code: "RESULT_UNKNOWN", Detail: "Agent startup result is unknown; inspect current Runtimes before another start; the request was not replayed"}
		}
		return result, &api.Error{Code: "START_FAILED", Detail: "Runner rejected Agent startup; inspect the configured command and Runner prerequisites"}
	}
	result.Runtime = &runtime
	if runtime.Adapter == "acp" {
		return s.initializeACP(ctx, scope, connection, request.Binding, result)
	}
	if profile.RequireAgentMCP {
		if err := s.configureMCP(ctx, scope, connection, runtime, request.Binding); err != nil {
			return result, err
		}
	}
	return result, nil
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

func (s *Service) resolveLaunch(ctx context.Context, owner string, request agents.StartRequest) (api.Profile, error) {
	profile := api.Profile{}
	projectID, directoryID := "", ""
	if request.Profile != nil && request.Custom != nil {
		return profile, invalid("choose a saved Profile or a custom launch")
	}
	selection := request.Profile
	directory := request.WorkingDirectory
	if request.Project != nil {
		selected := request.Project
		if selected.ID == "" || selected.Revision < 1 {
			return profile, invalid("project requires a fixed revision")
		}
		project, err := s.Store.Project(ctx, owner, selected.ID)
		if err != nil {
			return profile, err
		}
		if project.Revision != selected.Revision {
			return profile, metadata.ErrConflict
		}
		projectID = project.ID
		if selection == nil && request.Custom == nil {
			selection = project.DefaultProfile
		}
		if request.DirectoryID != "" {
			index := slices.IndexFunc(project.Directories, func(item workbench.Directory) bool { return item.ID == request.DirectoryID })
			if index < 0 {
				return profile, metadata.ErrNotFound
			}
			selectedDirectory := project.Directories[index]
			if selectedDirectory.Binding != request.Binding {
				return profile, runner.ErrBindingChanged
			}
			if directory != "" && directory != selectedDirectory.Path {
				return profile, invalid("selected directory differs from the requested working directory")
			}
			directory, directoryID = selectedDirectory.Path, selectedDirectory.ID
		}
	} else if request.DirectoryID != "" {
		return profile, invalid("a saved directory requires a project")
	}
	if request.Custom != nil {
		profile = *request.Custom
	} else {
		if selection == nil || selection.Revision < 1 {
			return profile, invalid("select a fixed Agent Profile revision")
		}
		record, err := s.Store.Profiles().Get(ctx, owner, *selection)
		if err != nil {
			return profile, err
		}
		profile = record.Profile
	}
	if directory != "" {
		profile.WorkingDirectory = directory
	}
	// Project labels are resolved by the host, never inherited from a Profile.
	profile.ProjectID, profile.DirectoryID = projectID, directoryID
	profile.ManagedACP = profile.Adapter == "acp"
	profile.RequireAgentMCP = profile.ManagedACP || profile.Adapter == "pty" && agentintegration.Agent(profile.Start.Argv) != ""
	if err := profile.Validate(); err != nil {
		return profile, invalid(err.Error())
	}
	if profile.Kind != "agent" {
		return profile, invalid("an Agent Profile is required")
	}
	return profile, nil
}
