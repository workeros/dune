package tests

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"strings"
	"testing"

	"github.com/aiomni/dune/pkg/host"
	"github.com/aiomni/dune/pkg/identity"
)

func TestLinkedBrowserIdentityMigration(t *testing.T) {
	for _, backend := range []string{"sqlite", "postgres"} {
		t.Run(backend, func(t *testing.T) {
			ctx := context.Background()
			const site = "https://dune.example.test/tools/dune/"
			options := host.Options{PublicURL: site, DataDir: filepath.Join(t.TempDir(), "metadata")}
			if backend == "postgres" {
				database := postgresWorkbenchConfig(t)
				options.DataDir, options.Database = "", &database
			}
			app, err := host.Open(ctx, options)
			must(t, err)
			defer func() { app.Close() }()
			request := func(method, route, body string, cookies ...*http.Cookie) *httptest.ResponseRecorder {
				r := httptest.NewRequest(method, route, strings.NewReader(body))
				r.Header.Set("Origin", "https://dune.example.test")
				r.Header.Set("X-Dune-Request", "1")
				r.Header.Set("Content-Type", "application/json")
				for _, cookie := range cookies {
					r.AddCookie(cookie)
				}
				out := httptest.NewRecorder()
				app.ServeHTTP(out, r)
				return out
			}
			out := request("POST", site+"api/auth/register", `{"email":"original@example.test","password":"original-test-password"}`)
			if out.Code != 200 {
				t.Fatalf("register: %d", out.Code)
			}
			var oldUser struct{ ID string }
			must(t, json.Unmarshal(out.Body.Bytes(), &oldUser))
			oldCookie := out.Result().Cookies()[0]
			decision := identity.LinkRequest{RequestID: "approved-migration", Actor: "admin:review", PrincipalID: oldUser.ID, Namespace: (&browserIdentityFixture{}).Namespace(), Subject: "browser-subject", Reason: "verified both accounts; approval migration-42"}
			record, err := app.LinkIdentity(ctx, decision)
			must(t, err)
			if record.LinkRequest != decision {
				t.Fatal("link decision lost")
			}
			if out := request("GET", site+"api/me", "", oldCookie); out.Code != 401 {
				t.Fatal("old cookie survived migration")
			}
			must(t, app.Close())
			if _, err := app.LinkIdentity(ctx, decision); err == nil {
				t.Fatal("closed host admitted link")
			}
			if _, err := app.IdentityLink(ctx, decision.RequestID); err == nil {
				t.Fatal("closed host admitted audit read")
			}
			options.Identity = &identity.Options{Provider: &browserIdentityFixture{}}
			app, err = host.Open(ctx, options)
			must(t, err)
			stored, err := app.IdentityLink(ctx, decision.RequestID)
			must(t, err)
			if stored != record {
				t.Fatal("reopened host lost audit record")
			}
			out = request("GET", site+"api/auth/external/start", "")
			if out.Code != 303 {
				t.Fatalf("begin: %d", out.Code)
			}
			location, err := url.Parse(out.Header().Get("Location"))
			must(t, err)
			out = request("GET", site+"api/auth/external/callback?code=valid&state="+location.Query().Get("state"), "", out.Result().Cookies()[0])
			if out.Code != 303 || out.Header().Get("Location") != site {
				t.Fatal("linked callback failed")
			}
			var session *http.Cookie
			for _, cookie := range out.Result().Cookies() {
				if cookie.Name == "dune_session" {
					session = cookie
				}
			}
			if session == nil {
				t.Fatal("linked login has no session")
			}
			out = request("GET", site+"api/me", "", session)
			var current struct{ ID, Email string }
			must(t, json.Unmarshal(out.Body.Bytes(), &current))
			if out.Code != 200 || current.ID != oldUser.ID || current.Email != "browser@example.test" {
				t.Fatal("SSO did not preserve explicitly linked principal")
			}
			if _, err := app.LinkIdentity(ctx, decision); err != nil {
				t.Fatal(err)
			}
			if out := request("GET", site+"api/me", "", session); out.Code != 200 {
				t.Fatal("repeated decision revoked new SSO session")
			}
		})
	}
}
