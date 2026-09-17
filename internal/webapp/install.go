package webapp

import (
	_ "embed"
	"fmt"
	"net/http"
)

//go:embed install.sh
var installScript string

func installCommand(site, token, runnerID string) string {
	return fmt.Sprintf("curl --fail --show-error --proto '=http,https' %s -o dune-install.sh && sh dune-install.sh enroll %s %s %s", shellQuote(site+"/api/v1/install.sh"), shellQuote(site), shellQuote(token), shellQuote(runnerID))
}
func (s *Server) cancelEnrollment(w http.ResponseWriter, r *http.Request) {
	_, cookie, ok := s.user(w, r)
	if !ok {
		return
	}
	if err := s.access.CancelEnrollment(r.Context(), cookie, r.PathValue("runner")); err != nil {
		writeMetadataError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
}
