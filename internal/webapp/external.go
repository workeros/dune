package webapp

import (
	"context"
	"net/http"
	"strings"
	"time"

	"github.com/aiomni/dune/internal/identity"
)

const loginCookie = "dune_login_proof"

func (s *Server) setSession(w http.ResponseWriter, token string, lifetime time.Duration) {
	http.SetCookie(w, &http.Cookie{Name: cookieName, Value: token, HttpOnly: true, Secure: strings.HasPrefix(s.urls.PublicURL, "https://"), SameSite: http.SameSiteStrictMode, Path: s.urls.CookiePath, MaxAge: int(lifetime.Seconds())})
}

func (s *Server) loginProof(w http.ResponseWriter, proof string, age int) {
	http.SetCookie(w, &http.Cookie{Name: loginCookie, Value: proof, HttpOnly: true, Secure: strings.HasPrefix(s.urls.PublicURL, "https://"), SameSite: http.SameSiteLaxMode, Path: s.urls.Path + "api/auth/external/callback", MaxAge: age})
}

func (s *Server) externalStart(w http.ResponseWriter, r *http.Request) {
	if !s.authAllowed(w, r) {
		return
	}
	defer func() { <-s.hashSlots }()
	ctx, cancel := context.WithTimeout(r.Context(), 15*time.Second)
	defer cancel()
	location, proof, err := s.options.External.Begin(ctx, s.urls.PublicURL+"api/auth/external/callback")
	if err != nil {
		http.Redirect(w, r, s.urls.PublicURL+"?login_error=1", http.StatusSeeOther)
		return
	}
	s.loginProof(w, proof, int(identity.LoginLifetime.Seconds()))
	http.Redirect(w, r, location, http.StatusSeeOther)
}

func (s *Server) externalCallback(w http.ResponseWriter, r *http.Request) {
	if !s.authAllowed(w, r) {
		return
	}
	defer func() { <-s.hashSlots }()
	ctx, cancel := context.WithTimeout(r.Context(), 15*time.Second)
	defer cancel()
	proof, err := r.Cookie(loginCookie)
	query := r.URL.Query()
	s.loginProof(w, "", -1)
	if err != nil || len(query["state"]) != 1 || len(query["code"]) != 1 || query.Has("error") {
		http.Redirect(w, r, s.urls.PublicURL+"?login_error=1", http.StatusSeeOther)
		return
	}
	_, token, err := s.options.External.Finish(ctx, query.Get("state"), proof.Value, query.Get("code"), s.urls.PublicURL+"api/auth/external/callback")
	if err != nil {
		http.Redirect(w, r, s.urls.PublicURL+"?login_error=1", http.StatusSeeOther)
		return
	}
	s.setSession(w, token, s.options.External.SessionLifetime())
	http.Redirect(w, r, s.urls.PublicURL, http.StatusSeeOther)
}
