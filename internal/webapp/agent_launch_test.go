package webapp

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/aiomni/dune/internal/wire"
	"github.com/aiomni/dune/pkg/agents"
	"github.com/aiomni/dune/pkg/api"
	"github.com/aiomni/dune/pkg/profiles"
	"github.com/aiomni/dune/pkg/runner"
)

type launchFunc func(context.Context, agents.Scope, agents.StartRequest) (agents.LaunchResult, error)

func (f launchFunc) Start(ctx context.Context, scope agents.Scope, request agents.StartRequest) (agents.LaunchResult, error) {
	return f(ctx, scope, request)
}

func (f launchFunc) QueryLaunch(context.Context, agents.Scope, agents.LaunchQuery) (api.SubmissionReceipt, error) {
	return api.SubmissionReceipt{}, &api.Error{Code: "UNSUPPORTED"}
}

type directoryFixture struct {
	list func(agents.Scope, runner.Query) (agents.DirectoryPage, error)
	get  func(agents.Scope, string) (agents.Agent, error)
}

func (f directoryFixture) List(_ context.Context, scope agents.Scope, query runner.Query) (agents.DirectoryPage, error) {
	return f.list(scope, query)
}
func (f directoryFixture) Get(_ context.Context, scope agents.Scope, ref string) (agents.Agent, error) {
	return f.get(scope, ref)
}

func TestAgentDirectoryHTTPAuthenticatesBeforeDiscovery(t *testing.T) {
	for _, tenant := range []bool{false, true} {
		t.Run(map[bool]string{false: "personal", true: "tenant"}[tenant], func(t *testing.T) {
			f := newWorkbenchFixture(t, tenant)
			calls := 0
			f.server.options.AgentDirectory = directoryFixture{
				list: func(scope agents.Scope, query runner.Query) (agents.DirectoryPage, error) {
					calls++
					if scope.OwnerID != f.owner || scope.Principal.ID != f.users[0].ID || query.Limit != 1 {
						t.Fatal("untrusted directory scope")
					}
					return agents.DirectoryPage{Items: []agents.Agent{{Ref: "known-agent"}}}, nil
				},
				get: func(scope agents.Scope, ref string) (agents.Agent, error) {
					calls++
					if scope.OwnerID != f.owner || scope.Principal.ID != f.users[0].ID || ref != "known-agent" {
						t.Fatal("untrusted Agent scope")
					}
					return agents.Agent{}, &api.Error{Code: "STALE_SESSION", Detail: "native session changed"}
				},
			}
			if out := f.request(t, "GET", "/agents?limit=1", 0, nil); out.Code != http.StatusOK || !bytes.Contains(out.Body.Bytes(), []byte("known-agent")) {
				t.Fatal(out.Code, out.Body.String())
			}
			if out := f.request(t, "POST", "/agents/get", 0, map[string]string{"agent_ref": "known-agent"}); out.Code != http.StatusUnprocessableEntity || !bytes.Contains(out.Body.Bytes(), []byte("STALE_SESSION")) {
				t.Fatal(out.Code, out.Body.String())
			}
			if out := f.request(t, "GET", "/agents", -1, nil); out.Code != http.StatusUnauthorized || calls != 2 {
				t.Fatal("anonymous discovery reached the service", out.Code, calls)
			}
			if tenant {
				if out := f.request(t, "POST", "/agents/get", 2, map[string]string{"agent_ref": "known-agent"}); out.Code != http.StatusForbidden || calls != 2 {
					t.Fatal("cross-Tenant discovery reached the service", out.Code, calls)
				}
			}
			if out := f.request(t, "POST", "/agents/get", 0, map[string]string{"agent_ref": "known-agent", "owner_id": "other"}); out.Code != http.StatusBadRequest || calls != 2 {
				t.Fatal("payload overrode directory scope", out.Code, calls)
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
		return agents.LaunchResult{Worktree: &api.Worktree{Path: "/new-tree", Branch: "feat/helper"}}, &api.Error{Code: "RESULT_UNKNOWN", Detail: "startup result unknown"}
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
	request := agents.StartRequest{SubmissionID: wire.ID(), Profile: &profiles.Selection{ID: "chosen", Revision: 3}, Worktree: &agents.WorktreeLocation{Path: "/new-tree", Branch: "feat/helper"}}
	out := send(0, request)
	if out.Code != http.StatusServiceUnavailable || !bytes.Contains(out.Body.Bytes(), []byte("RESULT_UNKNOWN")) || !bytes.Contains(out.Body.Bytes(), []byte("/new-tree")) || calls != 1 {
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
}

type launchQueryFunc func(context.Context, agents.Scope, agents.LaunchQuery) (api.SubmissionReceipt, error)

func (f launchQueryFunc) Start(context.Context, agents.Scope, agents.StartRequest) (agents.LaunchResult, error) {
	panic("launch query must not start an Agent")
}
func (f launchQueryFunc) QueryLaunch(ctx context.Context, scope agents.Scope, query agents.LaunchQuery) (api.SubmissionReceipt, error) {
	return f(ctx, scope, query)
}

func TestLaunchQueryHTTPPinsOwnerAndOriginalBinding(t *testing.T) {
	for _, tenant := range []bool{false, true} {
		t.Run(map[bool]string{false: "personal", true: "tenant"}[tenant], func(t *testing.T) {
			f := newWorkbenchFixture(t, tenant)
			query := agents.LaunchQuery{SubmissionID: "caller-saved", Binding: runner.Binding{RunnerID: "original", FabricID: "fabric", MachineID: "machine", Revision: 7}}
			calls := 0
			f.server.options.AgentLauncher = launchQueryFunc(func(_ context.Context, scope agents.Scope, got agents.LaunchQuery) (api.SubmissionReceipt, error) {
				calls++
				if scope.OwnerID != f.owner || scope.Principal.ID != f.users[0].ID || got != query {
					t.Fatal("query changed original identity", scope, got)
				}
				key := api.SubmissionKey{SubmissionID: got.SubmissionID, Target: api.SubmissionTarget{OwnerID: scope.OwnerID, RunnerID: got.Binding.RunnerID, FabricID: got.Binding.FabricID, MachineID: got.Binding.MachineID, BindingRevision: got.Binding.Revision}}
				return api.SubmissionReceipt{SubmissionKey: key, Admission: api.SubmissionUnknown}, nil
			})
			out := f.request(t, "POST", "/agents/launch-submission", 0, query)
			var receipt api.SubmissionReceipt
			if out.Code != http.StatusOK || json.Unmarshal(out.Body.Bytes(), &receipt) != nil || receipt.SubmissionID != query.SubmissionID || receipt.Admission != api.SubmissionUnknown || receipt.Target.OwnerID != f.owner || calls != 1 {
				t.Fatal("launch query response", out.Code, out.Body.String(), calls)
			}
			if out := f.request(t, "POST", "/agents/launch-submission", -1, query); out.Code != http.StatusUnauthorized || calls != 1 {
				t.Fatal("anonymous query reached service", out.Code, calls)
			}
			if tenant {
				if out := f.request(t, "POST", "/agents/launch-submission", 2, query); out.Code != http.StatusForbidden || calls != 1 {
					t.Fatal("cross-Tenant query reached service", out.Code, calls)
				}
			}
			if out := f.request(t, "POST", "/agents/launch-submission", 0, map[string]any{"submission_id": query.SubmissionID, "binding": query.Binding, "owner_id": "foreign"}); out.Code != http.StatusBadRequest || calls != 1 {
				t.Fatal("body overrode trusted owner", out.Code, calls)
			}
		})
	}
}
