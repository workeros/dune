package webapp

import (
	"context"
	"encoding/json"
	"net/http/httptest"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/aiomni/dune/internal/config"
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
			if _, _, err := store.Register("existing@example.test", "a-strong-test-password"); err != nil {
				t.Fatal(err)
			}
			app, err := NewServer(context.Background(), config.Config{}, Options{
				PublicURL: "https://example.test/tools/dune/", GatewayURL: "wss://machines.test/private/connect", DisableRegistration: disabled,
			}, store)
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
				"local_registration": !disabled, "attached": true, "managed": false,
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
				if _, _, err := store.Login("new@example.test", "a-strong-test-password"); err == nil {
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
