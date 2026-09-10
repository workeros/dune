package tests

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/aiomni/dune/pkg/access"
	"github.com/aiomni/dune/pkg/host"
	"github.com/aiomni/dune/pkg/identity"
)

type browserIdentityFixture struct {
	mu      sync.Mutex
	valid   map[string]identity.User
	checked atomic.Int32
}

func newBrowserIdentityFixture() *browserIdentityFixture {
	return &browserIdentityFixture{valid: map[string]identity.User{
		"sanddance-session": {ID: "enterprise-uid-42", Email: "browser@example.test", Namespace: "sanddance", Subject: "enterprise-uid-42"},
	}}
}

func (*browserIdentityFixture) Namespace() string { return "sanddance" }
func (*browserIdentityFixture) LoginMethods(publicURL string) []identity.LoginMethod {
	return []identity.LoginMethod{{Kind: "enterprise", URL: "https://identity.example.test/login?return=" + publicURL}}
}
func (p *browserIdentityFixture) Authenticate(ctx context.Context, token string) (identity.User, error) {
	if err := ctx.Err(); err != nil {
		return identity.User{}, err
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	user, ok := p.valid[token]
	if !ok {
		return identity.User{}, identity.ErrUnauthorized
	}
	return user, nil
}
func (p *browserIdentityFixture) Logout(ctx context.Context, token string) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	p.mu.Lock()
	delete(p.valid, token)
	p.mu.Unlock()
	return nil
}
func (p *browserIdentityFixture) Check(_ context.Context, request access.Request) (access.Decision, error) {
	p.checked.Add(1)
	return access.Decision{Allowed: request.PrincipalID == "enterprise-uid-42" && request.Namespace == "sanddance", ID: request.RequestID, Reason: "VERIFIED_ENTERPRISE_UID", ValidUntil: time.Now().Add(access.MaxLease)}, nil
}

func TestEnterpriseSessionBoundary(t *testing.T) {
	for _, backend := range []string{"sqlite", "postgres"} {
		t.Run(backend, func(t *testing.T) {
			const site = "https://dune.example.test/tools/dune/"
			provider := newBrowserIdentityFixture()
			options := host.Options{PublicURL: site, DataDir: filepath.Join(t.TempDir(), "metadata"), Identity: provider, AccessChecker: provider}
			if backend == "postgres" {
				database := postgresWorkbenchConfig(t)
				options.DataDir = ""
				options.Database = &database
			}
			app, err := host.Open(context.Background(), options)
			must(t, err)
			defer app.Close()
			request := func(method, route, body string, cookie bool) *httptest.ResponseRecorder {
				r := httptest.NewRequest(method, route, strings.NewReader(body))
				if method != http.MethodGet {
					r.Header.Set("Content-Type", "application/json")
					r.Header.Set("X-Dune-Request", "1")
					r.Header.Set("Origin", "https://dune.example.test")
				}
				if cookie {
					r.AddCookie(&http.Cookie{Name: "dune_session", Value: "sanddance-session"})
				}
				out := httptest.NewRecorder()
				app.ServeHTTP(out, r)
				return out
			}
			bootstrap := request("GET", site+"api/bootstrap", "", false)
			if bootstrap.Code != 200 || !strings.Contains(bootstrap.Body.String(), `"kind":"enterprise"`) || strings.Contains(bootstrap.Body.String(), `"kind":"password"`) || !strings.Contains(bootstrap.Body.String(), `"local_registration":false`) {
				t.Fatal("bootstrap did not advertise host login", bootstrap.Code, bootstrap.Body.String())
			}
			if out := request("GET", site+"api/me", "", true); out.Code != 200 || !strings.Contains(out.Body.String(), `"id":"enterprise-uid-42"`) {
				t.Fatal("host-verified session rejected", out.Code, out.Body.String())
			}
			for _, route := range []string{"login", "register"} {
				if out := request("POST", site+"api/auth/"+route, `{"email":"browser@example.test","password":"browser-test-password"}`, false); out.Code != 404 {
					t.Fatal("Dune exposed local password route with enterprise identity", route, out.Code)
				}
			}
			created := request("POST", site+"api/enrollments", `{"name":"subject machine"}`, true)
			if created.Code != 200 || provider.checked.Load() != 1 {
				t.Fatal("enterprise UID did not reach policy", created.Code, provider.checked.Load())
			}
			var enrollment struct {
				Token string `json:"token"`
			}
			if err := json.Unmarshal(created.Body.Bytes(), &enrollment); err != nil || enrollment.Token == "" {
				t.Fatal("enterprise enrollment token missing", err, created.Body.String())
			}
			consumed := request("POST", site+"api/enroll", `{"token":"`+enrollment.Token+`","os":"linux","arch":"amd64"}`, false)
			if consumed.Code != 200 || provider.checked.Load() != 2 {
				t.Fatal("enterprise enrollment lost identity scope", consumed.Code, provider.checked.Load(), consumed.Body.String())
			}
			if err := app.SetUserEnabled(context.Background(), "enterprise-uid-42", false); err == nil {
				t.Fatal("Dune tried to persist enterprise user state")
			}
			logout := request("POST", site+"api/auth/logout", `{}`, true)
			if logout.Code != 200 {
				t.Fatal("host logout failed", logout.Code)
			}
			if out := request("GET", site+"api/me", "", true); out.Code != 401 {
				t.Fatal("revoked enterprise session survived", out.Code)
			}
			if _, err := provider.Authenticate(context.Background(), "sanddance-session"); !errors.Is(err, identity.ErrUnauthorized) {
				t.Fatal("logout was not delegated to host identity")
			}
		})
	}
}
