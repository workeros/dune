package webapp

import (
	"errors"
	"github.com/aiomni/dune/pkg/agents"
	"github.com/aiomni/dune/pkg/api"
	"net/http"
)

func (s *Server) agentDirectoryRoutes(prefix string) {
	s.mux.HandleFunc("GET "+prefix+"/agents", s.listAgents)
	s.mux.HandleFunc("POST "+prefix+"/agents/get", s.getAgent)
}

func (s *Server) listAgents(w http.ResponseWriter, r *http.Request) {
	user, owner, ok := s.workbenchOwner(w, r, "workspace.read")
	if !ok {
		return
	}
	query, ok := pageQuery(w, r)
	if !ok {
		return
	}
	if s.options.AgentDirectory == nil {
		writeError(w, http.StatusServiceUnavailable, "UNAVAILABLE", "Agent discovery is not configured")
		return
	}
	page, err := s.options.AgentDirectory.List(r.Context(), agents.Scope{Principal: user, OwnerID: owner}, query)
	if err != nil {
		writeMetadataError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, page)
}

func (s *Server) getAgent(w http.ResponseWriter, r *http.Request) {
	user, owner, ok := s.workbenchOwner(w, r, "workspace.read")
	if !ok {
		return
	}
	var request struct {
		Ref string `json:"agent_ref"`
	}
	if !readJSON(w, r, &request) {
		return
	}
	if s.options.AgentDirectory == nil {
		writeError(w, http.StatusServiceUnavailable, "UNAVAILABLE", "Agent discovery is not configured")
		return
	}
	item, err := s.options.AgentDirectory.Get(r.Context(), agents.Scope{Principal: user, OwnerID: owner}, request.Ref)
	if err != nil {
		var failure *api.Error
		if errors.As(err, &failure) {
			operationError(w, err)
		} else {
			writeMetadataError(w, err)
		}
		return
	}
	writeJSON(w, http.StatusOK, item)
}
