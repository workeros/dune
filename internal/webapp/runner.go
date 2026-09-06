package webapp

import (
	"net/http"

	"github.com/aiomni/dune/pkg/login"
	"github.com/aiomni/dune/pkg/runner"
)

func (s *Server) runners(w http.ResponseWriter, r *http.Request) {
	user, _, ok := s.user(w, r)
	if !ok {
		return
	}
	rows, err := s.store.Runners(r.Context(), user.ID)
	if err != nil {
		writeMetadataError(w, err)
		return
	}
	writeJSON(w, 200, rows)
}
func (s *Server) runner(w http.ResponseWriter, r *http.Request) {
	user, _, ok := s.user(w, r)
	if !ok {
		return
	}
	row, err := s.store.Runner(r.Context(), user.ID, r.PathValue("runner"))
	if err != nil {
		writeMetadataError(w, err)
		return
	}
	writeJSON(w, 200, row)
}
func (s *Server) cliRunners(w http.ResponseWriter, r *http.Request) {
	user, _, ok := s.cliUser(w, r)
	if !ok {
		return
	}
	rows, err := s.store.Runners(r.Context(), user.ID)
	if err != nil {
		writeMetadataError(w, err)
		return
	}
	writeJSON(w, 200, rows)
}
func (s *Server) cliRunner(w http.ResponseWriter, r *http.Request) {
	user, _, ok := s.cliUser(w, r)
	if !ok {
		return
	}
	row, err := s.store.Runner(r.Context(), user.ID, r.PathValue("runner"))
	if err != nil {
		writeMetadataError(w, err)
		return
	}
	writeJSON(w, 200, row)
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
