package webapp

import (
	"net/http"
	"strconv"

	"github.com/aiomni/dune/pkg/login"
	"github.com/aiomni/dune/pkg/runner"
)

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
	out := runner.Page{Items: []runner.Runner{}, NextCursor: page.NextCursor}
	for _, resource := range page.Items {
		out.Items = append(out.Items, resource.Runner)
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
	writeJSON(w, 200, resource.Runner)
}
func (s *Server) cliRunners(w http.ResponseWriter, r *http.Request) {
	user, _, ok := s.cliUser(w, r)
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
	out := runner.Page{Items: []runner.Runner{}, NextCursor: page.NextCursor}
	for _, resource := range page.Items {
		out.Items = append(out.Items, resource.Runner)
	}
	writeJSON(w, 200, out)
}
func (s *Server) cliRunner(w http.ResponseWriter, r *http.Request) {
	user, _, ok := s.cliUser(w, r)
	if !ok {
		return
	}
	resource, _, err := s.access.Resource(r.Context(), user, r.PathValue("runner"), false, "runner.get")
	if err != nil {
		writeMetadataError(w, err)
		return
	}
	writeJSON(w, 200, resource.Runner)
}
func (s *Server) cliRunnerAccess(w http.ResponseWriter, r *http.Request) {
	_, token, ok := s.cliUser(w, r)
	if !ok {
		return
	}
	var binding runner.Binding
	if !readJSON(w, r, &binding) {
		return
	}
	grant, err := s.access.ClientRunnerCLI(r.Context(), token, binding)
	if err != nil {
		writeMetadataError(w, err)
		return
	}
	writeJSON(w, 200, login.Access{Credential: grant.Token(), Gateway: s.urls.GatewayURL, Target: binding.MachineID, ExpiresAt: grant.ExpiresAt()})
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
