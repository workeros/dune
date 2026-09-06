package webapp

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/aiomni/dune/internal/config"
)

func TestDeploymentPrefixRoutesCookiesAndEnrollment(t *testing.T) {
	for _, prefix := range []string{"/", "/tools/dune/", "/a%20b/"} {
		t.Run(prefix, func(t *testing.T) {
			store, err := OpenStore(filepath.Join(t.TempDir(), "accounts"))
			if err != nil {
				t.Fatal(err)
			}
			defer store.Close()
			assets, binaries := t.TempDir(), t.TempDir()
			for _, name := range []string{"index.html", "main.js", "install.sh"} {
				if err := os.WriteFile(filepath.Join(assets, name), []byte(name), 0600); err != nil {
					t.Fatal(err)
				}
			}
			if err := os.WriteFile(filepath.Join(binaries, "dune-linux-amd64.tar.gz"), []byte("package"), 0600); err != nil {
				t.Fatal(err)
			}
			origin := "https://example.test"
			app, err := NewServer(context.Background(), config.Config{Gateway: "ws://127.0.0.1:1/tunnel"}, Options{PublicURL: origin + strings.TrimSuffix(prefix, "/"), GatewayURL: "wss://machines.test/private/connect", Assets: assets, Binaries: binaries}, store)
			if err != nil {
				t.Fatal(err)
			}
			defer app.Close()
			do := func(method, route, body, requestOrigin string, cookie *http.Cookie) *httptest.ResponseRecorder {
				request := httptest.NewRequest(method, route, strings.NewReader(body))
				request.Header.Set("Origin", requestOrigin)
				request.Header.Set("Content-Type", "application/json")
				request.Header.Set("X-Dune-Request", "1")
				request.Header.Set("X-Forwarded-Prefix", "/forged/")
				if cookie != nil {
					request.AddCookie(cookie)
				}
				response := httptest.NewRecorder()
				app.ServeHTTP(response, request)
				return response
			}
			for _, route := range []string{"", "main.js", "install.sh", "downloads/dune-linux-amd64.tar.gz"} {
				if response := do("GET", prefix+route, "", "", nil); response.Code != 200 {
					t.Fatalf("static %s = %d", route, response.Code)
				}
			}
			if prefix != "/" {
				if response := do("GET", strings.TrimSuffix(prefix, "/"), "", "", nil); response.Code != 308 || response.Header().Get("Location") != prefix {
					t.Fatalf("canonical redirect = %d %s", response.Code, response.Header().Get("Location"))
				}
				if response := do("GET", "/api/me", "", "", nil); response.Code != 404 {
					t.Fatal("root route bypassed prefix")
				}
			}
			response := do("POST", prefix+"api/auth/register", `{"email":"owner@example.test","password":"a-strong-test-password"}`, origin, nil)
			if response.Code != 200 {
				t.Fatalf("register: %d %s", response.Code, response.Body.String())
			}
			cookie := response.Result().Cookies()[0]
			if cookie.Path != prefix || !cookie.Secure || !cookie.HttpOnly {
				t.Fatalf("cookie scope = %+v", cookie)
			}
			if response := do("GET", prefix+"api/me", "", "", cookie); response.Code != 200 {
				t.Fatal("cookie could not authenticate prefixed API")
			}
			if response := do("POST", prefix+"api/enrollments", `{"name":"machine"}`, origin+prefix, cookie); response.Code != 403 {
				t.Fatal("accepted an Origin containing a path")
			}
			response = do("POST", prefix+"api/enrollments", `{"name":"machine"}`, origin, cookie)
			var enrollment struct{ Token, Command, Endpoint string }
			if err := json.Unmarshal(response.Body.Bytes(), &enrollment); err != nil || response.Code != 200 {
				t.Fatalf("enrollment: %d %v", response.Code, err)
			}
			if enrollment.Endpoint != origin+strings.TrimSuffix(prefix, "/") || !strings.Contains(enrollment.Command, origin+prefix+"install.sh") || strings.Contains(enrollment.Command, "machines.test") || strings.Contains(enrollment.Command, "forged") {
				t.Fatalf("incorrect installation address: %+v", enrollment)
			}
			body, _ := json.Marshal(map[string]string{"token": enrollment.Token, "os": "linux", "arch": "amd64"})
			response = do("POST", prefix+"api/enroll", string(body), "", nil)
			if response.Code != 200 || !bytes.Contains(response.Body.Bytes(), []byte(`"gateway":"wss://machines.test/private/connect"`)) {
				t.Fatalf("gateway override: %d %s", response.Code, response.Body.String())
			}
			response = do("POST", prefix+"api/auth/logout", `{}`, origin, cookie)
			if response.Code != 200 || response.Result().Cookies()[0].Path != prefix || response.Result().Cookies()[0].MaxAge != -1 {
				t.Fatal("logout did not clear the same cookie path")
			}
		})
	}
}
