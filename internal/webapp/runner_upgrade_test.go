package webapp

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/aiomni/dune/pkg/api"
	"github.com/aiomni/dune/pkg/runner"
	"github.com/aiomni/dune/pkg/upgrade"
)

type upgradeHTTPFixture struct {
	calls        []string
	check        func(upgrade.Scope)
	loseResponse bool
}

func (f *upgradeHTTPFixture) called(scope upgrade.Scope, name string) {
	f.check(scope)
	f.calls = append(f.calls, name)
}
func (f *upgradeHTTPFixture) InspectRunner(ctx context.Context, scope upgrade.Scope) (upgrade.Inspection, error) {
	f.called(scope, "inspect")
	return upgrade.Inspection{Binding: scope.Binding}, nil
}
func (f *upgradeHTTPFixture) PreviewUpgrade(ctx context.Context, scope upgrade.Scope, r upgrade.PreviewRequest) (upgrade.Preview, error) {
	f.called(scope, "preview")
	return upgrade.Preview{Allowed: true}, nil
}
func (f *upgradeHTTPFixture) StartUpgrade(ctx context.Context, scope upgrade.Scope, r upgrade.Request) (upgrade.Observation, error) {
	f.called(scope, "start")
	result := upgrade.Observation{Operation: upgrade.Operation{Request: r, Admission: api.SubmissionUnknown}, Freshness: "unreachable"}
	if f.loseResponse {
		return result, &api.Error{Code: "RESULT_UNKNOWN", Detail: "query the original submission"}
	}
	result.Operation.Admission = api.SubmissionAccepted
	result.Operation.ID = "operation"
	return result, nil
}
func (f *upgradeHTTPFixture) GetUpgrade(ctx context.Context, scope upgrade.Scope, q upgrade.Query) (upgrade.Observation, error) {
	f.called(scope, "get")
	return upgrade.Observation{Operation: upgrade.Operation{ID: q.OperationID}, Freshness: "last_known"}, nil
}
func (f *upgradeHTTPFixture) ListUpgrades(ctx context.Context, scope upgrade.Scope, r upgrade.ListRequest) (upgrade.History, error) {
	f.called(scope, "list")
	return upgrade.History{Freshness: "last_known", Page: upgrade.Page{Items: []upgrade.Operation{}}}, nil
}

func TestRunnerUpgradeHTTPPinsScopeAndPreservesUnknownAdmission(t *testing.T) {
	f := newWorkbenchFixture(t, true)
	_, token, _, err := f.store.IssueEnrollment(t.Context(), f.owner, "Runner")
	if err != nil {
		t.Fatal(err)
	}
	machine, _, err := f.store.Enroll(t.Context(), token, "linux", "amd64")
	if err != nil {
		t.Fatal(err)
	}
	binding := runner.Binding{RunnerID: machine.RunnerID, MachineID: machine.ID, FabricID: "attached", Revision: 1}
	service := &upgradeHTTPFixture{check: func(scope upgrade.Scope) {
		if scope.Principal.ID != f.users[0].ID || scope.OwnerID != f.owner || scope.Binding != binding {
			t.Fatal("HTTP supplied untrusted scope", scope)
		}
	}}
	f.server.options.RunnerUpgrades = service
	send := func(action string, user int, body any) *httptest.ResponseRecorder {
		data, _ := json.Marshal(body)
		request := httptest.NewRequest("POST", "/api/v1/runners/"+binding.RunnerID+"/upgrade/"+action+"?machine_id="+binding.MachineID+"&fabric_id=attached&revision=1", bytes.NewReader(data))
		request.Header.Set("Content-Type", "application/json")
		request.Header.Set("X-Dune-Request", "1")
		request.Header.Set("Origin", "http://dune.test")
		request.AddCookie(&http.Cookie{Name: cookieName, Value: f.tokens[user]})
		result := httptest.NewRecorder()
		f.server.ServeHTTP(result, request)
		return result
	}
	ref := upgrade.ReleaseRef{ID: "release", ManifestSHA256: strings.Repeat("a", 64)}
	submission := upgrade.Request{SubmissionID: "submission", Binding: binding, InstallationID: "installation", ExpectedInstallationRevision: "1", ExpectedRunningSHA256: strings.Repeat("b", 64), Release: ref}
	for _, call := range []struct {
		action string
		body   any
	}{
		{"inspect", struct{}{}}, {"preview", upgrade.PreviewRequest{Binding: binding, Release: ref}}, {"start", submission}, {"get", upgrade.Query{Binding: binding, InstallationID: "installation", OperationID: "operation"}}, {"list", upgrade.ListRequest{Binding: binding, InstallationID: "installation"}},
	} {
		out := send(call.action, 0, call.body)
		want := http.StatusOK
		if call.action == "start" {
			want = http.StatusAccepted
		}
		if out.Code != want {
			t.Fatal(call.action, out.Code, out.Body.String())
		}
	}
	service.loseResponse = true
	out := send("start", 0, submission)
	if out.Code != http.StatusServiceUnavailable || !bytes.Contains(out.Body.Bytes(), []byte(`"admission":"unknown"`)) || !bytes.Contains(out.Body.Bytes(), []byte(`"submission_id":"submission"`)) {
		t.Fatal("lost unknown admission identity", out.Code, out.Body.String())
	}
	before := len(service.calls)
	submission.Binding.Revision++
	if out := send("start", 0, submission); out.Code != http.StatusBadRequest || len(service.calls) != before {
		t.Fatal("HTTP followed mismatched binding", out.Code)
	}
	if out := send("list", 2, upgrade.ListRequest{Binding: binding, InstallationID: "installation"}); out.Code == http.StatusOK || len(service.calls) != before {
		t.Fatal("cross-owner history reached service", out.Code)
	}
}
