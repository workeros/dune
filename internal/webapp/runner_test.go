package webapp

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"strings"
	"testing"

	"github.com/aiomni/dune/internal/identity"
	"github.com/aiomni/dune/pkg/runner"
)

func TestBrowserRunnerBindingIsFixed(t *testing.T) {
	ctx := context.Background()
	dir := filepath.Join(t.TempDir(), "metadata")
	store, err := OpenStore(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	local := identity.NewLocal(store, true)
	user, cookie, err := local.Register(ctx, "runner-ui@example.test", "runner-ui-test-password")
	if err != nil {
		t.Fatal(err)
	}
	token, _, err := store.IssueEnrollment(ctx, user.ID, "selected environment")
	if err != nil {
		t.Fatal(err)
	}
	machine, credential, err := store.Enroll(ctx, token, "linux", "amd64")
	if err != nil {
		t.Fatal(err)
	}
	dials := 0
	app, err := newTestServer(ctx, Options{PublicURL: "http://dune.example.test/tools/dune/", DialGateway: func(context.Context, string) (net.Conn, error) {
		dials++
		return nil, fmt.Errorf("offline review machine")
	}}, store, local)
	if err != nil {
		t.Fatal(err)
	}
	defer app.Close()
	request := func(method, route, session string) *httptest.ResponseRecorder {
		t.Helper()
		if method == "GET" && strings.Contains(route, "/events") {
			separator := "?"
			if strings.Contains(route, "?") {
				separator = "&"
			}
			route += separator + "incarnation=selected&generation=1"
		}
		r := httptest.NewRequest(method, "/tools/dune/api/"+route, bytes.NewBufferString(`{"operation":"runtime.list"}`))
		r.AddCookie(&http.Cookie{Name: cookieName, Value: session})
		r.Header.Set("Origin", app.urls.Origin)
		r.Header.Set("X-Dune-Request", "1")
		r.Header.Set("Content-Type", "application/json")
		out := httptest.NewRecorder()
		app.ServeHTTP(out, r)
		return out
	}
	response := request("GET", "runners", cookie)
	var page struct{ Items []runnerView }
	if err := json.Unmarshal(response.Body.Bytes(), &page); err != nil || response.Code != 200 || len(page.Items) != 1 {
		t.Fatal("Runner discovery", response.Code, err)
	}
	selected := page.Items[0]
	if selected.ID != machine.RunnerID || selected.Binding == nil || selected.Binding.MachineID != machine.ID || selected.Online || selected.OS != "linux" || selected.Arch != "amd64" {
		t.Fatal("incorrect current binding facts", selected)
	}
	route := func(binding runner.Binding, suffix string) string {
		query := url.Values{"machine_id": {binding.MachineID}, "fabric_id": {binding.FabricID}, "revision": {fmt.Sprint(binding.Revision)}}
		return "runners/" + binding.RunnerID + "/" + suffix + "?" + query.Encode()
	}
	for _, suffix := range []string{"call", "sessions", "sessions/guessed/events", "binding"} {
		method := "POST"
		if suffix == "binding" {
			method = "DELETE"
		}
		if suffix == "sessions/guessed/events" {
			method = "GET"
		}
		for _, bad := range []runner.Binding{
			{RunnerID: selected.ID, MachineID: machine.ID, FabricID: selected.Binding.FabricID, Revision: 2},
			{RunnerID: selected.ID, MachineID: machine.ID, FabricID: "another-fabric", Revision: 1},
			{RunnerID: "another-runner", MachineID: machine.ID, FabricID: selected.Binding.FabricID, Revision: 1},
		} {
			out := request(method, route(bad, suffix), cookie)
			if out.Code != 409 || !bytes.Contains(out.Body.Bytes(), []byte("BINDING_CHANGED")) {
				t.Fatal("stale selector reached execution", suffix, out.Code, out.Body.String())
			}
		}
		for _, bad := range []string{"runners/" + selected.ID + "/" + suffix, route(*selected.Binding, suffix) + "&revision=2"} {
			if out := request(method, bad, cookie); out.Code != 400 {
				t.Fatal("missing or ambiguous selector accepted", suffix, out.Code)
			}
		}
		if out := request(method, route(*selected.Binding, suffix), ""); out.Code != 401 {
			t.Fatal("binding acted as a credential", suffix, out.Code)
		}
	}
	if dials != 0 {
		t.Fatal("rejected request contacted Gateway")
	}
	if out := request("POST", route(*selected.Binding, "call"), cookie); out.Code != 503 || dials != 1 {
		t.Fatal("valid selection did not reach Gateway", out.Code, dials)
	}
	// Simulate the lifecycle module committing a new revision. This is a SQL
	// fixture, not a product endpoint for replacing Attached/Managed resources.
	db, err := sql.Open("sqlite", filepath.Join(dir, "metadata.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if _, err := db.Exec(`UPDATE dune_runners SET binding_revision=2 WHERE id=?`, selected.ID); err != nil {
		t.Fatal(err)
	}
	for _, step := range []struct{ method, suffix string }{{"POST", "call"}, {"POST", "sessions"}, {"GET", "sessions/old/events"}, {"DELETE", "binding"}} {
		if out := request(step.method, route(*selected.Binding, step.suffix), cookie); out.Code != 409 {
			t.Fatal("old request followed binding revision", step, out.Code)
		}
	}
	if dials != 1 {
		t.Fatal("old snapshot was retried against replacement")
	}
	current := *selected.Binding
	current.Revision = 2
	if out := request("DELETE", route(current, "binding"), cookie); out.Code != 200 {
		t.Fatal("explicit current binding could not be revoked", out.Code)
	}
	if _, err := store.MachineCredential(ctx, credential); err == nil {
		t.Fatal("Runner unbind retained machine credential")
	}
}
