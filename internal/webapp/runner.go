package webapp

import (
	"net/http"
	"strconv"

	"github.com/aiomni/dune/internal/authorization"
	"github.com/aiomni/dune/pkg/runner"
)

// Browser discovery includes facts about the current binding. These fields are
// display information; execution always verifies the selected binding again.
type runnerView struct {
	runner.Runner
	Online bool   `json:"online"`
	OS     string `json:"os,omitempty"`
	Arch   string `json:"arch,omitempty"`
}

func runnerResponse(resource authorization.Resource, online map[string]bool) runnerView {
	view := runnerView{Runner: resource.Runner, OS: resource.OS, Arch: resource.Arch}
	if b := resource.Runner.Binding; b != nil {
		view.Online = online[b.MachineID]
	}
	return view
}

func (s *Server) runners(w http.ResponseWriter, r *http.Request) {
	user, _, ok := s.user(w, r)
	if !ok {
		return
	}
	query, ok := pageQuery(w, r)
	if !ok {
		return
	}
	page, err := s.access.Discover(r.Context(), user, query, false)
	if err != nil {
		writeMetadataError(w, err)
		return
	}
	online, err := s.online(r.Context(), page.Items)
	if err != nil {
		writeMetadataError(w, err)
		return
	}
	out := struct {
		Items      []runnerView `json:"items"`
		NextCursor string       `json:"next_cursor,omitempty"`
	}{Items: []runnerView{}, NextCursor: page.NextCursor}
	for _, resource := range page.Items {
		out.Items = append(out.Items, runnerResponse(resource, online))
	}
	writeJSON(w, 200, out)
}
func (s *Server) runner(w http.ResponseWriter, r *http.Request) {
	user, _, ok := s.user(w, r)
	if !ok {
		return
	}
	resource, _, err := s.access.Resource(r.Context(), user, r.PathValue("runner"), false, "runner.get")
	if err != nil {
		writeMetadataError(w, err)
		return
	}
	online, err := s.online(r.Context(), []authorization.Resource{resource})
	if err != nil {
		writeMetadataError(w, err)
		return
	}
	writeJSON(w, 200, runnerResponse(resource, online))
}

// Binding selectors are not credentials or authoritative scope. They prevent a
// stale browser request from resolving the same logical Runner to a new target.
func selectedBinding(w http.ResponseWriter, r *http.Request) (runner.Binding, bool) {
	query := r.URL.Query()
	revision, err := strconv.ParseInt(query.Get("revision"), 10, 64)
	b := runner.Binding{RunnerID: r.PathValue("runner"), MachineID: query.Get("machine_id"), FabricID: query.Get("fabric_id"), Revision: revision}
	if err != nil || !b.Valid() || len(b.RunnerID) > 256 || len(b.MachineID) > 256 || len(b.FabricID) > 256 || len(query["revision"]) != 1 || len(query["machine_id"]) != 1 || len(query["fabric_id"]) != 1 {
		writeError(w, 400, "INVALID_BINDING", "a complete selected Runner binding is required")
		return b, false
	}
	return b, true
}

func (s *Server) revokeRunner(w http.ResponseWriter, r *http.Request) {
	_, cookie, ok := s.user(w, r)
	if !ok {
		return
	}
	b, ok := selectedBinding(w, r)
	if !ok {
		return
	}
	if err := s.access.RevokeRunner(r.Context(), cookie, b); err != nil {
		writeMetadataError(w, err)
		return
	}
	s.gateway.Disconnect(b.MachineID)
	writeJSON(w, 200, map[string]bool{"ok": true})
}
func pageQuery(w http.ResponseWriter, r *http.Request) (runner.Query, bool) {
	query := runner.Query{Cursor: r.URL.Query().Get("cursor")}
	var err error
	if value := r.URL.Query().Get("limit"); value != "" {
		query.Limit, err = strconv.Atoi(value)
	}
	if err != nil || query.Limit < 0 || query.Limit > 100 || len(query.Cursor) > 128 {
		writeError(w, 400, "INVALID_ARGUMENT", "invalid page query")
		return query, false
	}
	return query, true
}
