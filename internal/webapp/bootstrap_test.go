package webapp

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/aiomni/dune/internal/identity"
)

func TestStartupCapabilitiesAndRegistration(t *testing.T) {
	for _, disabled := range []bool{false, true} {
		name := "open"
		if disabled {
			name = "closed"
		}
		t.Run(name, func(t *testing.T) {
			store, err := OpenStore(filepath.Join(t.TempDir(), "accounts"))
			if err != nil {
				t.Fatal(err)
			}
			defer store.Close()
			if _, _, err := identity.NewLocal(store, true).Register(context.Background(), "existing@example.test", "a-strong-test-password"); err != nil {
				t.Fatal(err)
			}
			app, err := newTestServer(context.Background(), Options{DialGateway: noGateway,
				PublicURL: "https://example.test/tools/dune/", GatewayURL: "wss://machines.test/private/connect",
			}, store, identity.NewLocal(store, !disabled))
			if err != nil {
				t.Fatal(err)
			}
			defer app.Close()
			do := func(method, route, body string) *httptest.ResponseRecorder {
				r := httptest.NewRequest(method, route, strings.NewReader(body))
				r.Header.Set("Content-Type", "application/json")
				r.Header.Set("X-Dune-Request", "1")
				// Untrusted forwarding and Host fields cannot alter advertised URLs.
				r.Host = "forged.test"
				r.Header.Set("X-Forwarded-Host", "forged.test")
				w := httptest.NewRecorder()
				app.ServeHTTP(w, r)
				return w
			}
			response := do("GET", "/tools/dune/api/bootstrap", "")
			var actual map[string]any
			if err := json.Unmarshal(response.Body.Bytes(), &actual); err != nil {
				t.Fatal(err)
			}
			want := map[string]any{
				"login_methods":      []any{map[string]any{"kind": "password", "url": "https://example.test/tools/dune/api/auth/login"}},
				"local_registration": !disabled, "attached": true, "managed": false, "tenant_scoped": false,
				"public_url": "https://example.test/tools/dune/", "gateway_url": "wss://machines.test/private/connect",
			}
			if response.Code != 200 || !reflect.DeepEqual(actual, want) || response.Header().Get("Cache-Control") != "no-store" || len(response.Result().Cookies()) != 0 {
				t.Fatalf("anonymous startup information: %d %v", response.Code, actual)
			}
			response = do("POST", "/tools/dune/api/auth/register", `{"email":"new@example.test","password":"a-strong-test-password"}`)
			if disabled {
				if response.Code != 403 || !strings.Contains(response.Body.String(), `"code":"REGISTRATION_DISABLED"`) || len(response.Result().Cookies()) != 0 {
					t.Fatalf("disabled registration: %d %s", response.Code, response.Body.String())
				}
				if _, _, err := identity.NewLocal(store, true).Login(context.Background(), "new@example.test", "a-strong-test-password"); err == nil {
					t.Fatal("disabled registration created an account")
				}
			} else if response.Code != 200 {
				t.Fatalf("enabled registration: %d %s", response.Code, response.Body.String())
			}
			response = do("POST", actual["login_methods"].([]any)[0].(map[string]any)["url"].(string), `{"email":"existing@example.test","password":"a-strong-test-password"}`)
			if response.Code != 200 || len(response.Result().Cookies()) != 1 {
				t.Fatalf("existing account login: %d %s", response.Code, response.Body.String())
			}
		})
	}
}

func TestDisableAttachedHidesCapabilityAndIssuanceRoute(t *testing.T) {
	store, err := OpenStore(filepath.Join(t.TempDir(), "accounts"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	local := identity.NewLocal(store, true)
	_, session, err := local.Register(context.Background(), "owner@example.test", "a-strong-test-password")
	if err != nil {
		t.Fatal(err)
	}
	app, err := newTestServer(context.Background(), Options{DialGateway: noGateway, PublicURL: "https://example.test/", DisableAttached: true}, store, local)
	if err != nil {
		t.Fatal(err)
	}
	defer app.Close()

	bootstrap := httptest.NewRecorder()
	app.ServeHTTP(bootstrap, httptest.NewRequest(http.MethodGet, "/api/bootstrap", nil))
	if bootstrap.Code != http.StatusOK || !strings.Contains(bootstrap.Body.String(), `"attached":false`) {
		t.Fatalf("attached capability remained visible: %d %s", bootstrap.Code, bootstrap.Body.String())
	}

	request := httptest.NewRequest(http.MethodPost, "/api/enrollments", strings.NewReader(`{"name":"hidden"}`))
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("X-Dune-Request", "1")
	request.Header.Set("Origin", "https://example.test")
	request.AddCookie(&http.Cookie{Name: cookieName, Value: session})
	response := httptest.NewRecorder()
	app.ServeHTTP(response, request)
	if response.Code != http.StatusNotFound {
		t.Fatalf("disabled Attached enrollment route returned %d: %s", response.Code, response.Body.String())
	}
}
