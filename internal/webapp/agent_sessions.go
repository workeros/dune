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
			if code == "RESULT_UNKNOWN" || code == "RECOVERY_INDEX_FAILED" {
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

func (s *Server) agentSessionRoutes(prefix string) {
	s.mux.HandleFunc("GET "+prefix+"/agent-sessions", s.listAgentSessions)
	s.mux.HandleFunc("GET "+prefix+"/agent-sessions/{session}", s.getAgentSession)
	s.mux.HandleFunc("POST "+prefix+"/agent-sessions/{session}/resume", s.resumeAgentSession)
}

func (s *Server) resumeAgentSession(w http.ResponseWriter, r *http.Request) {
	user, owner, ok := s.workbenchOwner(w, r, "workspace.write")
	if !ok {
		return
	}
	var request agents.ResumeRequest
	if !readJSON(w, r, &request) {
		return
	}
	if request.SessionID != "" && request.SessionID != r.PathValue("session") {
		writeError(w, http.StatusBadRequest, "INVALID_ARGUMENT", "session record must match the selected path")
		return
	}
	request.SessionID = r.PathValue("session")
	if s.options.AgentRestorer == nil {
		writeError(w, http.StatusServiceUnavailable, "UNAVAILABLE", "Agent recovery is not configured")
		return
	}
	result, err := s.options.AgentRestorer.Resume(r.Context(), agents.Scope{Principal: user, OwnerID: owner}, request)
	writeAgentResult(w, result, err)
}

func (s *Server) listAgentSessions(w http.ResponseWriter, r *http.Request) {
	_, owner, ok := s.workbenchOwner(w, r, "workspace.read")
	if !ok {
		return
	}
	query, ok := pageQuery(w, r)
	if !ok {
		return
	}
	if query.Limit == 0 {
		query.Limit = 32
	}
	items, err := s.store.AgentSessions(r.Context(), owner, query.Cursor, query.Limit+1)
	if err != nil {
		writeMetadataError(w, err)
		return
	}
	page := struct {
		Items      []agents.Summary `json:"items"`
		NextCursor string           `json:"next_cursor,omitempty"`
	}{Items: []agents.Summary{}}
	if len(items) > query.Limit {
		items = items[:query.Limit]
		page.NextCursor = items[len(items)-1].ID
	}
	for _, item := range items {
		page.Items = append(page.Items, item.Summary())
	}
	writeJSON(w, http.StatusOK, page)
}

func (s *Server) getAgentSession(w http.ResponseWriter, r *http.Request) {
	_, owner, ok := s.workbenchOwner(w, r, "workspace.read")
	if !ok {
		return
	}
	item, err := s.store.AgentSession(r.Context(), owner, r.PathValue("session"))
	if err != nil {
		writeMetadataError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, item.Summary())
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
