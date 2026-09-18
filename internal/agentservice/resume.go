package agentservice

import (
	"context"
	"errors"
	"slices"
	"time"

	"github.com/aiomni/dune/internal/agentintegration"
	"github.com/aiomni/dune/internal/authorization"
	"github.com/aiomni/dune/internal/metadata"
	"github.com/aiomni/dune/pkg/agents"
	"github.com/aiomni/dune/pkg/api"
	"github.com/aiomni/dune/pkg/client"
	"github.com/aiomni/dune/pkg/workbench"
)

// Resume uses only the saved actual launch. No current Profile, environment
// defaults, setup steps, session/list, or "resume latest" enter this path.
func (s *Service) Resume(ctx context.Context, scope agents.Scope, request agents.ResumeRequest) (result agents.ResumeResult, err error) {
	if request.SessionID == "" || len(request.SessionID) > 256 || request.Revision < 1 || scope.OwnerID == "" {
		return result, invalid("session_record_id and positive revision are required")
	}
	ctx, cancel := context.WithTimeout(ctx, 90*time.Second)
	defer cancel()
	if _, err := s.Access.Check(ctx, scope.Principal, authorization.Resource{OwnerID: scope.OwnerID}, "workspace.write", ""); err != nil {
		return result, err
	}
	session, err := s.Store.AgentSession(ctx, scope.OwnerID, request.SessionID)
	if err != nil {
		return result, err
	}
	if session.LastRuntime == nil {
		return result, &api.Error{Code: "RECOVERY_UNAVAILABLE", Detail: "session has no confirmed previous Runtime"}
	}
	target := workbench.AgentTarget{Binding: session.Launch.Binding, Runtime: *session.LastRuntime}
	_, connection, closeConnection, err := s.connect(ctx, scope, target, "profile.start")
	if err != nil {
		return result, err
	}
	defer closeConnection()
	setSession := func(session agents.Session) { summary := session.Summary(); result.Session = &summary }
	setSession(session)
	if session.Attempt.Kind == "resume" && session.Attempt.BaseRevision == request.Revision {
		return s.existingResume(ctx, scope, connection, session)
	}
	if session.Revision != request.Revision {
		return result, metadata.ErrConflict
	}
	if session.Status != "available" || session.Native == nil || !session.Native.ResumeSupported || (session.Attempt.State != "ready" && session.Attempt.State != "failed") {
		return result, &api.Error{Code: "RECOVERY_UNAVAILABLE", Detail: "native session is unavailable or its previous attempt is unconfirmed"}
	}
	profile, err := resumeProfile(session)
	if err != nil {
		return result, err
	}
	requiredCapabilities := []string{"runtime.get", "runtime.stop", "machine.info", "agent.mcp.configure"}
	if profile.Adapter == "acp" {
		requiredCapabilities = append(requiredCapabilities, "acp.state", "acp.action", "agent.operation.wait")
	}
	for _, required := range requiredCapabilities {
		if !slices.Contains(connection.Binding.Capabilities, required) {
			return result, &api.Error{Code: "UNSUPPORTED", Detail: "Runner does not support native session recovery"}
		}
	}
	checked := map[workbench.RuntimeRef]bool{}
	for _, previous := range []*workbench.RuntimeRef{session.LastRuntime, session.Attempt.Runtime} {
		if previous == nil || checked[*previous] {
			continue
		}
		checked[*previous] = true
		previousTarget := workbench.AgentTarget{Binding: session.Launch.Binding, Runtime: *previous}
		runtime, lookupErr := connection.Get(ctx, runtimeFor(previousTarget))
		if lookupErr != nil {
			var failure *api.Error
			if errors.As(lookupErr, &failure) && failure.Code == "STALE_RUNTIME" {
				continue
			}
			return result, lookupErr
		}
		if runtime.State != "exited" {
			result.Runtime = &runtime
			return result, &api.Error{Code: "RUNTIME_ALIVE", Detail: "the existing Runtime is still alive; reconnect or stop it before explicit recovery"}
		}
	}
	var machine api.MachineInfo
	if err := connection.Call(ctx, "machine.info", struct{}{}, &machine); err != nil {
		return result, err
	}
	if machine.UserID == "" || machine.Home == "" || storageIdentity(machine, profile) != session.Launch.Storage {
		return result, &api.Error{Code: "RECOVERY_STORAGE_CHANGED", Detail: "the original execution user or native session storage is unavailable"}
	}
	session, claimed, err := s.Store.BeginAgentResume(ctx, scope.OwnerID, session.ID, request.Revision)
	if err != nil {
		return result, err
	}
	if !claimed {
		return s.existingResume(ctx, scope, connection, session)
	}
	setSession(session)
	runtime, stream, startErr := connection.Start(ctx, profile)
	if stream != nil {
		_ = stream.Close()
	}
	if startErr != nil {
		return s.failResume(ctx, scope, result, session, startOutcome(startErr), "recovery startup did not return a confirmed Runtime", false, connection)
	}
	result.Runtime = &runtime
	target = targetFor(session.Launch.Binding, runtime)
	saveCtx, saveCancel := context.WithTimeout(context.WithoutCancel(ctx), 3*time.Second)
	session, err = s.Store.RecordAgentRuntime(saveCtx, scope.OwnerID, session.ID, session.Attempt.ID, target.Runtime)
	saveCancel()
	if err != nil {
		return result, &api.Error{Code: "RECOVERY_INDEX_FAILED", Detail: "recovery Runtime started but its index write was not confirmed; do not start it again"}
	}
	setSession(session)
	if runtime.Adapter == "pty" {
		if err := s.configureMCP(ctx, scope, connection, runtime, result.Session.Binding); err != nil {
			return s.failResume(ctx, scope, result, session, "failed", "MCP configuration was not confirmed for the recovery Runtime", true, connection)
		}
		return s.awaitPTYResume(ctx, scope, connection, result, session)
	}
	ready, err := awaitACPReady(ctx, connection, runtime)
	if err != nil {
		return s.failResume(ctx, scope, result, session, "failed", "recovery Agent did not become ready for native loading", true, connection)
	}
	if !ready.CanLoad || session.Native.AgentVersion != "" && ready.Agent.Version != "" && session.Native.AgentVersion != ready.Agent.Version {
		return s.failResume(ctx, scope, result, session, "failed", "recovery Agent no longer supports the saved native session version or session/load", true, connection)
	}
	if err := s.configureMCP(ctx, scope, connection, runtime, result.Session.Binding); err != nil {
		return s.failResume(ctx, scope, result, session, "failed", "MCP configuration was not confirmed; no native recovery was submitted", true, connection)
	}
	accepted, err := connection.ACPSubmit(ctx, runtime, api.ACPAction{Action: "load", SessionID: session.Native.ID, Cwd: session.Native.Cwd})
	if err != nil {
		outcome := "failed"
		var failure *api.Error
		if !errors.As(submissionError(err), &failure) || failure.Code == "RESULT_UNKNOWN" {
			outcome = "unknown"
		}
		return s.failResume(ctx, scope, result, session, outcome, "native load admission did not return a confirmed operation", outcome == "failed", connection)
	}
	op := s.describeOperation(ctx, scope, target, accepted)
	result.Operation = &op
	for !accepted.Terminal() {
		accepted, err = connection.WaitAgentOperation(ctx, runtime, api.AgentOperationWait{Ref: accepted.Ref, TimeoutMS: 30000})
		if err != nil {
			return s.failResume(ctx, scope, result, session, "unknown", "native load completion is unknown; query the accepted operation before another recovery", false, connection)
		}
		op = s.describeOperation(ctx, scope, target, accepted)
		result.Operation = &op
	}
	if accepted.State != "completed" {
		outcome := "failed"
		if accepted.State == "unknown" {
			outcome = "unknown"
		}
		return s.failResume(ctx, scope, result, session, outcome, "Agent did not confirm loading the saved native session", outcome == "failed", connection)
	}
	if accepted.NativeSession == nil || accepted.NativeSession.ID != session.Native.ID || accepted.NativeSession.Cwd != session.Native.Cwd {
		return s.failResume(ctx, scope, result, session, "unknown", "Agent confirmation did not match the saved native session", false, connection)
	}
	saveCtx, saveCancel = context.WithTimeout(context.WithoutCancel(ctx), 3*time.Second)
	defer saveCancel()
	session, err = s.Store.ObserveAgentSession(saveCtx, scope.OwnerID, target, *accepted.NativeSession)
	if err != nil {
		return result, &api.Error{Code: "RECOVERY_INDEX_FAILED", Detail: "native session was loaded but saving its recovery confirmation failed; do not start again"}
	}
	setSession(session)
	// Native confirmation is immutable and belongs to this load even if another
	// caller has already switched the Runtime again. Discovery finds current state.
	runtime.NativeSession = accepted.NativeSession
	result.Runtime = &runtime
	result.Operation.RecoveryError = ""
	return result, nil
}

func resumeProfile(session agents.Session) (api.Profile, error) {
	profile := session.Launch.Profile
	if session.Launch.Recovery.ID == "" || recoveryAdapter(profile) != session.Launch.Recovery {
		return api.Profile{}, &api.Error{Code: "RECOVERY_UNAVAILABLE", Detail: "the saved launch has no supported native recovery adapter"}
	}
	profile.Setup.Steps = nil
	switch session.Launch.Recovery.ID {
	case "acp-load":
		profile.RequireAgentMCP = true
	case "pty-claude", "pty-codex":
		profile.RequireAgentMCP = true
		if session.Native == nil {
			return api.Profile{}, &api.Error{Code: "RECOVERY_UNAVAILABLE", Detail: "native PTY identity is missing"}
		}
		argument := "resume"
		if session.Launch.Recovery.ID == "pty-claude" {
			argument = "--resume"
		}
		profile.Start.Argv = []string{profile.Start.Argv[0], argument, session.Native.ID}
		if agentintegration.Agent(profile.Start.Argv) == "" {
			return api.Profile{}, &api.Error{Code: "RECOVERY_UNAVAILABLE", Detail: "native PTY resume requires an exact session UUID"}
		}
		// PTY CLIs have one process working directory; resume in the confirmed
		// native directory, which may differ from the initial process launch.
		profile.WorkingDirectory = session.Native.Cwd
	default:
		return api.Profile{}, &api.Error{Code: "RECOVERY_UNAVAILABLE", Detail: "native recovery adapter is unsupported"}
	}
	return profile, nil
}

func (s *Service) existingResume(ctx context.Context, scope agents.Scope, connection *client.Client, session agents.Session) (agents.ResumeResult, error) {
	summary := session.Summary()
	result := agents.ResumeResult{Session: &summary}
	if session.Attempt.Runtime == nil {
		return result, nil
	}
	target := workbench.AgentTarget{Binding: session.Launch.Binding, Runtime: *session.Attempt.Runtime}
	runtime, err := connection.Get(ctx, runtimeFor(target))
	if err != nil {
		return result, err
	}
	result.Runtime = &runtime
	if runtime.NativeSession != nil {
		observed, err := s.Store.ObserveAgentSession(ctx, scope.OwnerID, target, *runtime.NativeSession)
		if err != nil {
			return result, &api.Error{Code: "RECOVERY_INDEX_FAILED", Detail: "the existing recovery confirmation could not be saved"}
		}
		if observed.ID != session.ID {
			current, err := s.Store.AgentSession(ctx, scope.OwnerID, session.ID)
			if err != nil {
				return result, err
			}
			summary = current.Summary()
			result.Session = &summary
			return result, &api.Error{Code: "STALE_SESSION", Detail: "the recovered Runtime has since selected another native session; discover its current state"}
		}
		summary = observed.Summary()
		result.Session = &summary
	}
	return result, nil
}

func (s *Service) failResume(ctx context.Context, scope agents.Scope, result agents.ResumeResult, session agents.Session, outcome, detail string, stop bool, connection *client.Client) (agents.ResumeResult, error) {
	saveCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 3*time.Second)
	failed, err := s.Store.FailAgentAttempt(saveCtx, scope.OwnerID, session.ID, session.Attempt.ID, outcome, detail)
	cancel()
	if err == nil {
		summary := failed.Summary()
		result.Session = &summary
	}
	if stop && result.Runtime != nil {
		stopCtx, stopCancel := context.WithTimeout(context.WithoutCancel(ctx), 3*time.Second)
		_ = connection.Stop(stopCtx, *result.Runtime)
		stopCancel()
	}
	code := "RECOVERY_FAILED"
	if outcome == "unknown" {
		code = "RESULT_UNKNOWN"
	}
	if err != nil {
		return result, &api.Error{Code: "RECOVERY_INDEX_FAILED", Detail: "recovery failed and saving its outcome was not confirmed; do not start again"}
	}
	return result, &api.Error{Code: code, Detail: detail}
}
