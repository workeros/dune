package webapp

import (
	"bytes"
	"context"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"

	"github.com/aiomni/dune/internal/config"
)

func TestHTTPRoutesWithStaticAssets(t *testing.T) {
	store, err := OpenStore(filepath.Join(t.TempDir(), "accounts"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	app, err := NewServer(context.Background(), config.Config{Gateway: "ws://127.0.0.1:1/tunnel"}, Options{PublicURL: "http://dune.example.test:17443", Assets: t.TempDir()}, store)
	if err != nil {
		t.Fatal(err)
	}
	defer app.Close()
	for _, path := range []string{"/api/me", "/api/machines"} {
		response := httptest.NewRecorder()
		app.ServeHTTP(response, httptest.NewRequest("GET", path, nil))
		if response.Code != http.StatusUnauthorized {
			t.Fatalf("unauthenticated %s: %d", path, response.Code)
		}
	}
	response := httptest.NewRecorder()
	request := httptest.NewRequest("POST", "/api/auth/register", bytes.NewBufferString(`{"email":"test@example.com","password":"a-strong-test-password"}`))
	request.Header.Set("Content-Type", "application/json")
	app.ServeHTTP(response, request)
	if response.Code != http.StatusForbidden {
		t.Fatal("accepted missing request guard")
	}
	response = httptest.NewRecorder()
	request = httptest.NewRequest("POST", "/api/auth/register", bytes.NewBufferString(`{"email":"test@example.com","password":"a-strong-test-password"}`))
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("X-Dune-Request", "1")
	request.Header.Set("Origin", "http://dune.example.test:17443")
	app.ServeHTTP(response, request)
	if response.Code != http.StatusOK {
		t.Fatalf("register: %d %s", response.Code, response.Body.String())
	}
	cookies := response.Result().Cookies()
	if len(cookies) != 1 || cookies[0].Secure || !cookies[0].HttpOnly || cookies[0].SameSite != http.SameSiteStrictMode {
		t.Fatal("incorrect login cookie")
	}
	response = httptest.NewRecorder()
	request = httptest.NewRequest("GET", "/api/machines", nil)
	request.AddCookie(cookies[0])
	app.ServeHTTP(response, request)
	if response.Code != http.StatusOK {
		t.Fatal("signed-in machine listing failed")
	}
	response = httptest.NewRecorder()
	request = httptest.NewRequest("DELETE", "/api/machines/guessed-id", nil)
	request.AddCookie(cookies[0])
	request.Header.Set("X-Dune-Request", "1")
	request.Header.Set("Origin", "https://attacker.example")
	app.ServeHTTP(response, request)
	if response.Code != http.StatusForbidden {
		t.Fatal("cross-origin state change accepted")
	}
}

// Exercise ownership at every public HTTP/WebSocket entry, before routing to
// a machine. A guessed runtime ID must not reveal whether it exists.
func TestHTTPOwnershipAndRevocation(t *testing.T) {
	store, err := OpenStore(filepath.Join(t.TempDir(), "accounts"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	app, err := NewServer(context.Background(), config.Config{Gateway: "ws://127.0.0.1:1/tunnel"}, Options{PublicURL: "http://dune.example.test:17443"}, store)
	if err != nil {
		t.Fatal(err)
	}
	defer app.Close()
	a, at, err := store.Register("a@example.test", "a-strong-test-password")
	if err != nil {
		t.Fatal(err)
	}
	b, bt, err := store.Register("b@example.test", "b-strong-test-password")
	if err != nil {
		t.Fatal(err)
	}
	bind, _, err := store.IssueEnrollment(a.ID, "machine-a")
	if err != nil {
		t.Fatal(err)
	}
	ma, _, err := store.Enroll(bind, "linux", "amd64")
	if err != nil {
		t.Fatal(err)
	}
	bind, _, err = store.IssueEnrollment(b.ID, "machine-b")
	if err != nil {
		t.Fatal(err)
	}
	mb, _, err := store.Enroll(bind, "darwin", "arm64")
	if err != nil {
		t.Fatal(err)
	}
	for _, pair := range []struct{ token, own, other string }{{at, ma.ID, mb.ID}, {bt, mb.ID, ma.ID}} {
		req := httptest.NewRequest("GET", "/api/machines", nil)
		req.AddCookie(&http.Cookie{Name: cookieName, Value: pair.token})
		out := httptest.NewRecorder()
		app.ServeHTTP(out, req)
		if out.Code != 200 || !bytes.Contains(out.Body.Bytes(), []byte(pair.own)) || bytes.Contains(out.Body.Bytes(), []byte(pair.other)) {
			t.Fatal("machine listing leaked ownership")
		}
		for _, route := range []struct{ method, suffix, body string }{{"POST", "/call", `{"operation":"runtime.list"}`}, {"POST", "/call", `{"operation":"git","payload":{"action":"diff"}}`}, {"POST", "/sessions", `{}`}, {"GET", "/sessions/guessed/events", ""}, {"DELETE", "", ""}} {
			req := httptest.NewRequest(route.method, "/api/machines/"+pair.other+route.suffix, bytes.NewBufferString(route.body))
			req.AddCookie(&http.Cookie{Name: cookieName, Value: pair.token})
			req.Header.Set("Origin", app.options.PublicURL)
			req.Header.Set("X-Dune-Request", "1")
			req.Header.Set("Content-Type", "application/json")
			out := httptest.NewRecorder()
			app.ServeHTTP(out, req)
			if out.Code != 404 {
				t.Fatalf("cross-account %s %s: %d", route.method, route.suffix, out.Code)
			}
		}
	}
	if err = store.Logout(at); err != nil {
		t.Fatal(err)
	}
	req := httptest.NewRequest("GET", "/api/machines", nil)
	req.AddCookie(&http.Cookie{Name: cookieName, Value: at})
	out := httptest.NewRecorder()
	app.ServeHTTP(out, req)
	if out.Code != 401 {
		t.Fatal("revoked browser session still works")
	}
}
