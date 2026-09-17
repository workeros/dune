package webapp

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"

	"github.com/aiomni/dune/internal/identity"
	"github.com/aiomni/dune/pkg/api"
	"github.com/aiomni/dune/pkg/profiles"
)

func TestPersonalProfilesPersistWithoutRunnerAndEnforceOwner(t *testing.T) {
	store, err := OpenStore(filepath.Join(t.TempDir(), "metadata"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	identities := identity.NewLocal(store, true)
	user, cookie, err := identities.Register(t.Context(), "owner@example.test", "strong-test-password")
	if err != nil {
		t.Fatal(err)
	}
	_, otherCookie, err := identities.Register(t.Context(), "other@example.test", "strong-test-password")
	if err != nil {
		t.Fatal(err)
	}
	app, err := newTestServer(t.Context(), Options{PublicURL: "http://dune.example.test/nested/", DialGateway: noGateway}, store, identities)
	if err != nil {
		t.Fatal(err)
	}
	defer app.Close()
	invoke := func(method, path, token string, body any) *httptest.ResponseRecorder {
		t.Helper()
		encoded, err := json.Marshal(body)
		if err != nil {
			t.Fatal(err)
		}
		request := httptest.NewRequest(method, "/nested/api/v1/profiles"+path, bytes.NewReader(encoded))
		request.Header.Set("X-Dune-Request", "1")
		request.Header.Set("Content-Type", "application/json")
		request.Header.Set("Origin", "http://dune.example.test")
		if token != "" {
			request.AddCookie(&http.Cookie{Name: cookieName, Value: token})
		}
		response := httptest.NewRecorder()
		app.ServeHTTP(response, request)
		return response
	}
	profile := api.Profile{Version: 1, Kind: "agent", WorkingDirectory: "/workspace", Adapter: "acp", Start: api.Command{Argv: []string{"/not-installed/agent"}, TimeoutSeconds: 19}, Env: map[string]string{"API_KEY": "private-value"}}
	profile.Setup.Steps = []api.Command{{Run: "prepare", Shell: "/bin/sh", TimeoutSeconds: 11}}
	body := map[string]any{"name": "Offline agent", "description": "server-owned", "profile": profile}
	if response := invoke("POST", "", "", body); response.Code != 401 {
		t.Fatal("anonymous save", response.Code)
	}
	response := invoke("POST", "", cookie, body)
	if response.Code != 201 {
		t.Fatal(response.Code, response.Body.String())
	}
	var first profiles.Record
	if err := json.Unmarshal(response.Body.Bytes(), &first); err != nil {
		t.Fatal(err)
	}
	if first.OwnerID != user.ID || first.Revision != 1 {
		t.Fatal("wrong server owner or revision")
	}
	if first.Profile.Start.TimeoutSeconds != 19 || first.Profile.Setup.Steps[0].TimeoutSeconds != 11 {
		t.Fatal("full Profile was reduced")
	}
	for _, method := range []string{"GET", "PUT", "DELETE"} {
		response := invoke(method, "/"+first.ID+"?revision=1", otherCookie, map[string]any{"name": "overwrite", "revision": 1, "profile": profile})
		if response.Code != 404 {
			t.Fatal("cross-owner access", method, response.Code, response.Body.String())
		}
	}
	response = invoke("GET", "", otherCookie, nil)
	if response.Code != 200 || bytes.Contains(response.Body.Bytes(), []byte("private-value")) || bytes.Contains(response.Body.Bytes(), []byte(first.ID)) {
		t.Fatal("cross-owner list leaked input")
	}
	body["revision"] = 1
	body["name"] = "Updated"
	if response := invoke("PUT", "/"+first.ID, cookie, body); response.Code != 200 {
		t.Fatal(response.Code, response.Body.String())
	}
	if response := invoke("PUT", "/"+first.ID, cookie, body); response.Code != 409 {
		t.Fatal("stale save accepted", response.Code)
	}
	response = invoke("GET", "/"+first.ID+"?revision=1", cookie, nil)
	if response.Code != 200 || !bytes.Contains(response.Body.Bytes(), []byte("Offline agent")) {
		t.Fatal("pinned revision changed")
	}
	for _, query := range []string{"", "?revision=0", "?revision=-1", "?revision=bad"} {
		if response := invoke("DELETE", "/"+first.ID+query, cookie, nil); response.Code != 400 {
			t.Fatal("unversioned delete", query, response.Code)
		}
	}
	if response := invoke("DELETE", "/"+first.ID+"?revision=1", cookie, nil); response.Code != 409 {
		t.Fatal("stale delete accepted")
	}
	if response := invoke("DELETE", "/"+first.ID+"?revision=2", cookie, nil); response.Code != 204 {
		t.Fatal("delete failed", response.Code, response.Body.String())
	}
	if response := invoke("GET", "/"+first.ID, cookie, nil); response.Code != 404 {
		t.Fatal("deleted Profile still available")
	}
}

func TestTenantHostDoesNotExposePersonalProfileRoutes(t *testing.T) {
	store, err := OpenStore(filepath.Join(t.TempDir(), "metadata"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	app, err := newTestServer(t.Context(), Options{PublicURL: "http://dune.example.test/", DialGateway: noGateway, TenantScoped: true}, store, identity.NewLocal(store, true))
	if err != nil {
		t.Fatal(err)
	}
	defer app.Close()
	response := httptest.NewRecorder()
	app.ServeHTTP(response, httptest.NewRequest("GET", "/api/v1/profiles", nil))
	if response.Code != 404 {
		t.Fatal("tenant host exposed personal Profile API", response.Code)
	}
}
