package host_test

import (
	"context"
	"encoding/json"
	"errors"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	"github.com/aiomni/dune/pkg/host"
)

func TestPrincipalAdministrationHonorsApplicationLifecycle(t *testing.T) {
	app, err := host.Open(context.Background(), host.Options{DataDir: filepath.Join(t.TempDir(), "metadata"), PublicURL: "http://example.test/dune/"})
	if err != nil {
		t.Fatal(err)
	}
	defer app.Close()
	request := httptest.NewRequest("POST", "/dune/api/auth/register", strings.NewReader(`{"email":"admin-test@example.test","password":"admin-test-password"}`))
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("X-Dune-Request", "1")
	request.Header.Set("Origin", "http://example.test")
	out := httptest.NewRecorder()
	app.ServeHTTP(out, request)
	if out.Code != 200 {
		t.Fatalf("registration: %d %s", out.Code, out.Body)
	}
	var user struct{ ID string }
	if err := json.Unmarshal(out.Body.Bytes(), &user); err != nil {
		t.Fatal(err)
	}
	cookie := out.Result().Cookies()[0]
	canceled, cancel := context.WithCancel(context.Background())
	cancel()
	if err := app.SetPrincipalEnabled(canceled, user.ID, false); !errors.Is(err, context.Canceled) {
		t.Fatal("canceled administration accepted", err)
	}
	me := httptest.NewRequest("GET", "/dune/api/me", nil)
	me.AddCookie(cookie)
	out = httptest.NewRecorder()
	app.ServeHTTP(out, me)
	if out.Code != 200 {
		t.Fatal("canceled administration revoked session")
	}
	if err := app.Close(); err != nil {
		t.Fatal(err)
	}
	if err := app.SetPrincipalEnabled(context.Background(), user.ID, false); err == nil {
		t.Fatal("closed host admitted administration")
	}
}
