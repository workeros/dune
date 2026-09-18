package webapp

import (
	"errors"
	"net/http"

	"github.com/aiomni/dune/pkg/agents"
	"github.com/aiomni/dune/pkg/api"
	"github.com/aiomni/dune/pkg/runner"
)

func (s *Server) start(w http.ResponseWriter, r *http.Request) {
	user, _, ok := s.user(w, r)
	if !ok {
		return
	}
	binding, ok := selectedBinding(w, r)
	if !ok {
		return
	}
	resource, _, err := s.access.Resource(r.Context(), user, binding.MachineID, true, "profile.start")
	if err != nil {
		writeMetadataError(w, err)
		return
	}
	if resource.Runner.Binding == nil || *resource.Runner.Binding != binding {
		writeMetadataError(w, runner.ErrBindingChanged)
		return
	}
	var request agents.StartRequest
	if !readJSON(w, r, &request) {
		return
	}
	if request.Binding != (runner.Binding{}) && request.Binding != binding {
		writeMetadataError(w, runner.ErrBindingChanged)
		return
	}
	request.Binding = binding
	if s.options.AgentLauncher == nil {
		writeError(w, http.StatusServiceUnavailable, "UNAVAILABLE", "Agent launcher is not configured")
		return
	}
	result, err := s.options.AgentLauncher.Start(r.Context(), agents.Scope{Principal: user, OwnerID: resource.OwnerID}, request)
	if err != nil {
		status, code, message := metadataError(err)
		var failure *api.Error
		if errors.As(err, &failure) {
			status, code, message = http.StatusUnprocessableEntity, failure.Code, failure.Detail
			if code == "INVALID_ARGUMENT" {
				status = http.StatusBadRequest
			}
			if code == "RESULT_UNKNOWN" {
				status = http.StatusServiceUnavailable
			}
		}
		writeJSON(w, status, struct {
			Code   string              `json:"code"`
			Error  string              `json:"error"`
			Result agents.LaunchResult `json:"result"`
		}{Code: code, Error: message, Result: result})
		return
	}
	writeJSON(w, http.StatusCreated, result)
}

func (s *Server) openAgentSession(w http.ResponseWriter, r *http.Request) {
	user, owner, ok := s.workbenchOwner(w, r, "workspace.write")
	if !ok {
		return
	}
	var request agents.OpenSessionRequest
	if !readJSON(w, r, &request) {
		return
	}
	if s.options.AgentNativeSessions == nil {
		writeError(w, http.StatusServiceUnavailable, "UNAVAILABLE", "native session service is not configured")
		return
	}
	result, err := s.options.AgentNativeSessions.OpenSession(r.Context(), agents.Scope{Principal: user, OwnerID: owner}, request)
	writeAgentResult(w, result, err)
}
