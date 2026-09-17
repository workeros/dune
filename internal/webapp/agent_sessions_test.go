package webapp

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/aiomni/dune/pkg/agents"
	"github.com/aiomni/dune/pkg/api"
	"github.com/aiomni/dune/pkg/profiles"
	"github.com/aiomni/dune/pkg/runner"
)

type launchFunc func(context.Context, agents.Scope, agents.StartRequest) (agents.LaunchResult, error)

func (f launchFunc) Start(ctx context.Context, scope agents.Scope, request agents.StartRequest) (agents.LaunchResult, error) {
	return f(ctx, scope, request)
}

func TestAgentSessionHTTPScopesAndOmitsPrivateSnapshots(t *testing.T) {
	for _, tenant := range []bool{false, true} {
		t.Run(map[bool]string{false: "personal", true: "tenant"}[tenant], func(t *testing.T) {
			f := newWorkbenchFixture(t, tenant)
			session, err := f.store.CreateAgentSession(t.Context(), f.owner, agents.LaunchSnapshot{
				Binding: runner.Binding{RunnerID: "r", FabricID: "f", MachineID: "m", Revision: 1}, AgentType: "fixture", Storage: "uid:1000:home:/home/fixture",
				Profile: api.Profile{Version: 1, Kind: "agent", Adapter: "pty", WorkingDirectory: "/workspace", Env: map[string]string{"SECRET": "private-snapshot-value"}, Start: api.Command{Argv: []string{"/private/launch-command"}}},
			})
			if err != nil {
				t.Fatal(err)
			}
			reader := 0
			if tenant {
				reader = 1
			}
			for _, suffix := range []string{"/agent-sessions", "/agent-sessions/" + session.ID} {
				out := f.request(t, "GET", suffix, reader, nil)
				if out.Code != http.StatusOK || !bytes.Contains(out.Body.Bytes(), []byte(session.ID)) {
					t.Fatal(out.Code, out.Body.String())
				}
				for _, secret := range []string{"private-snapshot-value", "SECRET", "/private/launch-command", "uid:1000"} {
					if bytes.Contains(out.Body.Bytes(), []byte(secret)) {
						t.Fatal("private launch data exposed", secret)
					}
				}
				out = f.request(t, "GET", suffix, 2, nil)
				if bytes.Contains(out.Body.Bytes(), []byte(session.ID)) {
					t.Fatal("cross-owner read exposed session")
				}
				if out := f.request(t, "GET", suffix, -1, nil); out.Code != http.StatusUnauthorized {
					t.Fatal("anonymous read", out.Code)
				}
			}
		})
	}
}

func TestAgentStartHTTPPinsScopeAndPreservesPartialResult(t *testing.T) {
	f := newWorkbenchFixture(t, true)
	_, token, _, err := f.store.IssueEnrollment(t.Context(), f.owner, "development")
	if err != nil {
		t.Fatal(err)
	}
	machine, _, err := f.store.Enroll(t.Context(), token, "linux", "amd64")
	if err != nil {
		t.Fatal(err)
	}
	binding := runner.Binding{RunnerID: machine.RunnerID, MachineID: machine.ID, FabricID: "attached", Revision: 1}
	calls := 0
	f.server.options.AgentLauncher = launchFunc(func(_ context.Context, scope agents.Scope, request agents.StartRequest) (agents.LaunchResult, error) {
		calls++
		if scope.Principal.ID != f.users[0].ID || scope.OwnerID != f.owner || request.Binding != binding || request.Profile.ID != "chosen" || request.Profile.Revision != 3 || request.Worktree.Path != "/new-tree" {
			t.Fatal("HTTP changed trusted scope or launch input")
		}
		return agents.LaunchResult{Worktree: &api.Worktree{Path: "/new-tree", Branch: "feat/helper"}, Session: &agents.Summary{ID: "saved-attempt"}}, &api.Error{Code: "RESULT_UNKNOWN", Detail: "startup result unknown"}
	})
	send := func(user int, body any) *httptest.ResponseRecorder {
		payload, _ := json.Marshal(body)
		r := httptest.NewRequest("POST", "/api/v1/runners/"+machine.RunnerID+"/sessions?machine_id="+machine.ID+"&fabric_id=attached&revision=1", bytes.NewReader(payload))
		r.Header.Set("Content-Type", "application/json")
		r.Header.Set("X-Dune-Request", "1")
		r.Header.Set("Origin", "http://dune.test")
		r.AddCookie(&http.Cookie{Name: cookieName, Value: f.tokens[user]})
		out := httptest.NewRecorder()
		f.server.ServeHTTP(out, r)
		return out
	}
	request := agents.StartRequest{Profile: &profiles.Selection{ID: "chosen", Revision: 3}, Worktree: &agents.WorktreeLocation{Path: "/new-tree", Branch: "feat/helper"}}
	out := send(0, request)
	if out.Code != http.StatusServiceUnavailable || !bytes.Contains(out.Body.Bytes(), []byte("RESULT_UNKNOWN")) || !bytes.Contains(out.Body.Bytes(), []byte("saved-attempt")) || !bytes.Contains(out.Body.Bytes(), []byte("/new-tree")) || calls != 1 {
		t.Fatal("partial result lost or replayed", out.Code, out.Body.String(), calls)
	}
	request.Binding = binding
	request.Binding.Revision++
	if out := send(0, request); out.Code != http.StatusConflict || calls != 1 {
		t.Fatal("body overrode URL binding", out.Code, calls)
	}
	request.Binding = runner.Binding{}
	if out := send(2, request); out.Code != http.StatusNotFound || calls != 1 {
		t.Fatal("cross-owner startup", out.Code, calls)
	}
	if out := send(0, api.Profile{Version: 1, Kind: "agent"}); out.Code != http.StatusBadRequest || calls != 1 {
		t.Fatal("obsolete raw Profile contract accepted", out.Code, calls)
	}
}
