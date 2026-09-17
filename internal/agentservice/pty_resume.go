package agentservice

import (
	"context"
	"errors"
	"time"

	"github.com/aiomni/dune/pkg/agents"
	"github.com/aiomni/dune/pkg/api"
	"github.com/aiomni/dune/pkg/client"
)

// PTY recovery is the native CLI's startup command, not a prompt operation.
// Only a matching SessionStart confirms it. A CLI waiting for native trust or
// login remains a known running attempt that discovery can later reconcile.
func (s *Service) awaitPTYResume(ctx context.Context, scope agents.Scope, connection *client.Client, result agents.ResumeResult, session agents.Session) (agents.ResumeResult, error) {
	waitCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	for {
		runtime, err := connection.Get(waitCtx, *result.Runtime)
		if err != nil {
			if waitCtx.Err() != nil {
				return result, nil // Bounded wait ended; never launch another CLI.
			}
			outcome := "unknown"
			var failure *api.Error
			if errors.As(err, &failure) && failure.Code == "STALE_RUNTIME" {
				outcome = "failed"
			}
			return s.failResume(ctx, scope, result, session, outcome, "native PTY recovery Runtime could not be observed", false, connection)
		}
		result.Runtime = &runtime
		if runtime.NativeSession != nil {
			if runtime.NativeSession.ID != session.Native.ID || runtime.NativeSession.Cwd != session.Native.Cwd {
				return s.failResume(ctx, scope, result, session, "failed", "native CLI selected a different session; recovery was not confirmed", true, connection)
			}
			saveCtx, saveCancel := context.WithTimeout(context.WithoutCancel(ctx), 3*time.Second)
			confirmed, err := s.Store.ObserveAgentSession(saveCtx, scope.OwnerID, targetFor(session.Launch.Binding, runtime), *runtime.NativeSession)
			saveCancel()
			if err != nil {
				return result, &api.Error{Code: "RECOVERY_INDEX_FAILED", Detail: "native session was confirmed but its recovery index could not be saved; do not start again"}
			}
			summary := confirmed.Summary()
			result.Session = &summary
			return result, nil
		}
		if runtime.State == "exited" {
			return s.failResume(ctx, scope, result, session, "failed", "native CLI exited without confirming the saved session", false, connection)
		}
		select {
		case <-waitCtx.Done():
			return result, nil
		case <-time.After(50 * time.Millisecond):
		}
	}
}
