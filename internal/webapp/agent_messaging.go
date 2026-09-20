package webapp

import (
	"context"
	"errors"
	"net/http"

	"github.com/aiomni/dune/pkg/agents"
	"github.com/aiomni/dune/pkg/api"
)

func (s *Server) agentMessagingRoutes(prefix string) {
	s.mux.HandleFunc("POST "+prefix+"/agents/submit", agentMessageHandler(s, "workspace.write", func(ctx context.Context, scope agents.Scope, request agents.SubmissionRequest) (api.SubmissionReceipt, error) {
		return s.options.AgentMessenger.Submit(ctx, scope, request)
	}))
	s.mux.HandleFunc("POST "+prefix+"/agents/submission", agentMessageHandler(s, "workspace.read", func(ctx context.Context, scope agents.Scope, request agents.SubmissionQuery) (api.SubmissionReceipt, error) {
		return s.options.AgentMessenger.QuerySubmission(ctx, scope, request)
	}))
	s.mux.HandleFunc("POST "+prefix+"/agents/prompt", agentMessageHandler(s, "workspace.write", func(ctx context.Context, scope agents.Scope, request agents.PromptRequest) (agents.Operation, error) {
		return s.options.AgentMessenger.Prompt(ctx, scope, request)
	}))
	s.mux.HandleFunc("POST "+prefix+"/agents/send-keys", agentMessageHandler(s, "workspace.write", func(ctx context.Context, scope agents.Scope, request agents.KeysRequest) (agents.Operation, error) {
		return s.options.AgentMessenger.SendKeys(ctx, scope, request)
	}))
	s.mux.HandleFunc("POST "+prefix+"/agents/wait", agentMessageHandler(s, "workspace.read", func(ctx context.Context, scope agents.Scope, request agents.WaitRequest) (agents.WaitResult, error) {
		return s.options.AgentMessenger.Wait(ctx, scope, request)
	}))
	s.mux.HandleFunc("POST "+prefix+"/agents/read", agentMessageHandler(s, "workspace.read", func(ctx context.Context, scope agents.Scope, request agents.ReadRequest) (agents.ReadResult, error) {
		return s.options.AgentMessenger.Read(ctx, scope, request)
	}))
}

func agentMessageHandler[Request, Result any](s *Server, access string, call func(context.Context, agents.Scope, Request) (Result, error)) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		user, owner, ok := s.workbenchOwner(w, r, access)
		if !ok {
			return
		}
		var request Request
		if !readJSON(w, r, &request) {
			return
		}
		if s.options.AgentMessenger == nil {
			writeError(w, http.StatusServiceUnavailable, "UNAVAILABLE", "Agent messaging is not configured")
			return
		}
		result, err := call(r.Context(), agents.Scope{Principal: user, OwnerID: owner}, request)
		writeAgentResult(w, result, err)
	}
}

func writeAgentResult[Result any](w http.ResponseWriter, result Result, err error) {
	if err != nil {
		status, code, message := metadataError(err)
		var failure *api.Error
		if errors.As(err, &failure) {
			status, code, message = http.StatusUnprocessableEntity, failure.Code, failure.Detail
			switch code {
			case "INVALID_ARGUMENT":
				status = http.StatusBadRequest
			case "STALE_SESSION", "STALE_RUNTIME", "OPERATION_EXPIRED", "RUNTIME_ALIVE":
				status = http.StatusConflict
			case "RESULT_UNKNOWN", "OFFLINE", "RECOVERY_INDEX_FAILED":
				status = http.StatusServiceUnavailable
			case "ACCESS_DENIED":
				status = http.StatusForbidden
			}
		}
		// Submission or recovery may have progressed before a later failure.
		// Preserve confirmed references instead of encouraging another start.
		writeJSON(w, status, struct {
			Code   string `json:"code"`
			Error  string `json:"error"`
			Result Result `json:"result"`
		}{Code: code, Error: message, Result: result})
		return
	}
	writeJSON(w, http.StatusOK, result)
}
