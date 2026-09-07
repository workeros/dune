package tests

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/aiomni/dune/pkg/access"
	"github.com/aiomni/dune/pkg/host"
	"github.com/aiomni/dune/pkg/identity"
)

type browserIdentityFixture struct{ exchanged, checked atomic.Int32 }

func (*browserIdentityFixture) Namespace() string { return "https://identity.example.test" }
func (*browserIdentityFixture) Begin(ctx context.Context, c identity.Challenge) (string, error) {
	return "https://identity.example.test/login?state=" + c.State, nil
}
func (p *browserIdentityFixture) Verify(ctx context.Context, c identity.Challenge, code string) (identity.Subject, error) {
	p.exchanged.Add(1)
	if len(c.Nonce) != 64 || len(c.Verifier) != 64 || c.RedirectURL != "https://dune.example.test/tools/dune/api/auth/external/callback" || code != "valid" {
		return identity.Subject{}, fmt.Errorf("challenge changed")
	}
	return identity.Subject{ID: "browser-subject", Email: "browser@example.test"}, nil
}

func (p *browserIdentityFixture) Check(ctx context.Context, request access.Request) (access.Decision, error) {
	p.checked.Add(1)
	return access.Decision{Allowed: request.PrincipalID != "" && request.Namespace == p.Namespace() && request.Subject == "browser-subject", ID: request.RequestID, Reason: "VERIFIED_SUBJECT", ValidUntil: time.Now().Add(access.MaxLease)}, nil
}

func TestExternalBrowserLoginCallbacks(t *testing.T) {
	for _, backend := range []string{"sqlite-restart", "postgres-two-apps"} {
		t.Run(backend, func(t *testing.T) {
			const site = "https://dune.example.test/tools/dune/"
			const callback = site + "api/auth/external/callback"
			providerA, providerB := &browserIdentityFixture{}, &browserIdentityFixture{}
			options := host.Options{PublicURL: site, ConfigurationVersion: "browser-identity-test-v1", DataDir: filepath.Join(t.TempDir(), "metadata"), Identity: &identity.Options{Provider: providerA}, AccessChecker: providerA}
			if backend == "postgres-two-apps" {
				database := postgresWorkbenchConfig(t)
				options.DataDir = ""
				options.Database = &database
			}
			a, err := host.Open(context.Background(), options)
			must(t, err)
			defer a.Close()
			begin := httptest.NewRecorder()
			a.ServeHTTP(begin, httptest.NewRequest("GET", site+"api/auth/external/start", nil))
			if begin.Code != 303 {
				t.Fatalf("begin: %d", begin.Code)
			}
			location, err := url.Parse(begin.Header().Get("Location"))
			must(t, err)
			state := location.Query().Get("state")
			proof := begin.Result().Cookies()[0]
			if !proof.HttpOnly || !proof.Secure || proof.SameSite != http.SameSiteLaxMode || proof.Path != "/tools/dune/api/auth/external/callback" || proof.MaxAge != 600 {
				t.Fatal("login proof cookie lost its callback binding")
			}
			if backend == "sqlite-restart" {
				must(t, a.Close())
			}
			options.Identity = &identity.Options{Provider: providerB}
			options.AccessChecker = providerB
			b, err := host.Open(context.Background(), options)
			must(t, err)
			defer b.Close()
			request := func(route string, cookies ...*http.Cookie) *httptest.ResponseRecorder {
				r := httptest.NewRequest("GET", route, nil)
				for _, cookie := range cookies {
					r.AddCookie(cookie)
				}
				out := httptest.NewRecorder()
				b.ServeHTTP(out, r)
				return out
			}
			if out := request(callback + "?code=valid&state=" + state); out.Header().Get("Location") != site+"?login_error=1" {
				t.Fatal("callback accepted missing browser proof")
			}
			if out := request(callback+"?code=valid&code=duplicate&state="+state, proof); out.Header().Get("Location") != site+"?login_error=1" {
				t.Fatal("callback accepted ambiguous code")
			}
			out := request(callback+"?code=valid&state="+state, proof)
			if out.Code != 303 || out.Header().Get("Location") != site {
				t.Fatalf("cross-application callback failed: %d %s", out.Code, out.Body)
			}
			var session *http.Cookie
			for _, cookie := range out.Result().Cookies() {
				if cookie.Name == "dune_session" {
					session = cookie
				}
			}
			if session == nil || !session.HttpOnly || !session.Secure || session.Path != "/tools/dune/" || session.SameSite != http.SameSiteStrictMode || session.MaxAge != 8*3600 {
				t.Fatal("external Dune session cookie invalid")
			}
			var user struct{ ID string }
			out = request(site+"api/me", session)
			must(t, json.Unmarshal(out.Body.Bytes(), &user))
			if out.Code != 200 || user.ID == "" {
				t.Fatal("external browser session rejected")
			}
			if out := request(callback+"?code=valid&state="+state, proof); out.Header().Get("Location") != site+"?login_error=1" {
				t.Fatal("callback replay succeeded")
			}
			if providerA.exchanged.Load() != 0 || providerB.exchanged.Load() != 1 {
				t.Fatal("callback did not exchange exactly once on the other application")
			}
			out = request(site + "api/bootstrap")
			if !strings.Contains(out.Body.String(), `"kind":"external"`) || strings.Contains(out.Body.String(), `"kind":"password"`) || !strings.Contains(out.Body.String(), `"local_registration":false`) {
				t.Fatal("bootstrap did not describe exclusive external login")
			}
			for _, route := range []string{"login", "register"} {
				r := httptest.NewRequest("POST", site+"api/auth/"+route, strings.NewReader(`{"email":"browser@example.test","password":"browser-test-password"}`))
				r.Header.Set("Content-Type", "application/json")
				r.Header.Set("X-Dune-Request", "1")
				r.Header.Set("Origin", "https://dune.example.test")
				out := httptest.NewRecorder()
				b.ServeHTTP(out, r)
				if out.Code != 403 {
					t.Fatal("local password route bypassed external identity")
				}
			}
			if strings.Contains(out.Body.String(), "browser-subject") {
				t.Fatal("subject exposed in bootstrap")
			}
			enrollment := httptest.NewRequest("POST", site+"api/enrollments", strings.NewReader(`{"name":"subject machine"}`))
			enrollment.Header.Set("Content-Type", "application/json")
			enrollment.Header.Set("X-Dune-Request", "1")
			enrollment.Header.Set("Origin", "https://dune.example.test")
			enrollment.AddCookie(session)
			created := httptest.NewRecorder()
			b.ServeHTTP(created, enrollment)
			if created.Code != 200 || providerB.checked.Load() != 1 {
				t.Fatal("verified callback subject did not reach host checker", created.Code, providerB.checked.Load())
			}
			must(t, b.SetPrincipalEnabled(context.Background(), user.ID, false))
			if out := request(site+"api/me", session); out.Code != 401 {
				t.Fatal("external session survived Dune suspension")
			}
		})
	}
}
