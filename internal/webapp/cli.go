package webapp

import (
	"errors"
	"net/http"
	"strings"

	"github.com/aiomni/dune/internal/authorization"
	"github.com/aiomni/dune/internal/identity"
	"github.com/aiomni/dune/pkg/login"
)

func (s *Server) cliStart(w http.ResponseWriter, r *http.Request) {
	if !s.authAllowed(w, r) {
		return
	}
	defer func() { <-s.hashSlots }()
	var body struct {
		Challenge string `json:"challenge"`
	}
	if !readJSON(w, r, &body) {
		return
	}
	request, err := s.store.BeginCLI(r.Context(), body.Challenge, s.urls.PublicURL, s.identity.Namespace())
	if err != nil {
		writeMetadataError(w, err)
		return
	}
	writeJSON(w, 200, request)
}

func (s *Server) cliConsume(w http.ResponseWriter, r *http.Request) {
	if !s.authAllowedFor(w, r, "cli-poll", 120) {
		return
	}
	defer func() { <-s.hashSlots }()
	var body struct {
		ID       string `json:"id"`
		Verifier string `json:"verifier"`
	}
	if !readJSON(w, r, &body) {
		return
	}
	session, err := s.store.ConsumeCLI(r.Context(), body.ID, s.urls.PublicURL, s.identity.Namespace(), body.Verifier)
	if errors.Is(err, identity.ErrPending) {
		writeJSON(w, 202, map[string]string{"status": "pending"})
		return
	}
	if err != nil {
		writeMetadataError(w, err)
		return
	}
	writeJSON(w, 200, session)
}

func (s *Server) cliReview(w http.ResponseWriter, r *http.Request) {
	if _, _, ok := s.user(w, r); !ok {
		return
	}
	review, err := s.store.ReviewCLI(r.Context(), r.PathValue("request"), s.urls.PublicURL, s.identity.Namespace())
	if err != nil {
		writeMetadataError(w, err)
		return
	}
	writeJSON(w, 200, review)
}

func (s *Server) cliConfirm(w http.ResponseWriter, r *http.Request) {
	user, cookie, ok := s.user(w, r)
	if !ok {
		return
	}
	var body struct {
		Code    string `json:"code"`
		Approve bool   `json:"approve"`
	}
	if !readJSON(w, r, &body) {
		return
	}
	if err := s.store.ConfirmCLI(r.Context(), r.PathValue("request"), s.urls.PublicURL, s.identity.Namespace(), body.Code, cookie, user.ID, body.Approve); err != nil {
		writeMetadataError(w, err)
		return
	}
	writeJSON(w, 200, map[string]bool{"ok": true})
}

func cliBearer(r *http.Request) string {
	if len(r.Header.Values("Authorization")) != 1 {
		return ""
	}
	value := r.Header.Get("Authorization")
	if !strings.HasPrefix(value, "Bearer ") {
		return ""
	}
	token := strings.TrimPrefix(value, "Bearer ")
	if !identity.ValidCLIToken(token) {
		return ""
	}
	return token
}

func (s *Server) cliUser(w http.ResponseWriter, r *http.Request) (identity.User, string, bool) {
	token := cliBearer(r)
	user, err := s.identity.AuthenticateCLI(r.Context(), token)
	if err != nil {
		writeMetadataError(w, err)
		return identity.User{}, "", false
	}
	return user, token, true
}

func (s *Server) cliMachines(w http.ResponseWriter, r *http.Request) {
	user, _, ok := s.cliUser(w, r)
	if !ok {
		return
	}
	query, ok := pageQuery(w, r)
	if !ok {
		return
	}
	page, err := s.access.Discover(r.Context(), user, query, true)
	if err != nil {
		writeMetadataError(w, err)
		return
	}
	out := login.MachinePage{Items: []login.Machine{}, NextCursor: page.NextCursor}
	for _, resource := range page.Items {
		out.Items = append(out.Items, login.Machine{ID: resource.Runner.Binding.MachineID, Name: resource.Runner.Name, Online: s.gateway.Online(resource.Runner.Binding.MachineID)})
	}
	writeJSON(w, 200, out)
}

func (s *Server) cliAccess(w http.ResponseWriter, r *http.Request) {
	_, token, ok := s.cliUser(w, r)
	if !ok {
		return
	}
	var body struct {
		Target string `json:"target"`
	}
	if !readJSON(w, r, &body) {
		return
	}
	grant, err := s.access.ClientCLI(r.Context(), token, body.Target)
	if errors.Is(err, authorization.ErrNotFound) {
		writeError(w, 404, "NOT_FOUND", "machine not found")
		return
	}
	if err != nil {
		writeMetadataError(w, err)
		return
	}
	// The CLI consumes this ticket after the HTTP response, possibly elsewhere.
	// Its lifetime and session limits bound abandoned exchanges.
	writeJSON(w, 200, login.Access{Credential: grant.Token(), Gateway: s.urls.GatewayURL, Target: body.Target, ExpiresAt: grant.ExpiresAt()})
}

func (s *Server) cliLogout(w http.ResponseWriter, r *http.Request) {
	token := cliBearer(r)
	if token == "" {
		writeMetadataError(w, identity.ErrUnauthorized)
		return
	}
	// Revocation by possession remains idempotent after expiry or a namespace
	// change. CLI tokens cannot name browser or machine credentials.
	if err := s.identity.Logout(r.Context(), token); err != nil {
		writeMetadataError(w, err)
		return
	}
	writeJSON(w, 200, map[string]bool{"ok": true})
}
