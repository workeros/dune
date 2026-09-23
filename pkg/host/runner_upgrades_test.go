package host

import (
	"context"
	"fmt"
	"net/http/httptest"
	"os"
	"testing"
	"time"

	"github.com/aiomni/dune/pkg/api"
	"github.com/aiomni/dune/pkg/identity"
	"github.com/aiomni/dune/pkg/upgrade"
)

func TestRunnerUpgradePublicAdmissionQueriesAndOfflineObservations(t *testing.T) {
	f := openInstalledRunnerFixture(t)
	server := httptest.NewServer(f.app)
	defer server.Close()
	config := fmt.Sprintf("gateway: ws://127.0.0.1:7443/api/v1/ws/tunnel\nupgrade_control_url: %s/api/v1/runner-upgrade-control\ntoken: %s\ntarget: %s\nsession_dir: %s\n", server.URL, f.credential, f.binding.MachineID, f.stateDir)
	if err := os.WriteFile(f.configPath, []byte(config), 0600); err != nil {
		t.Fatal(err)
	}
	selected := f.manifest
	calls := 0
	f.app.upgradeSource = releaseSourceFunc(func(ctx context.Context, ref upgrade.ReleaseRef, p upgrade.Platform) (upgrade.Manifest, error) {
		calls++
		return selected, nil
	})
	service := f.app.RunnerUpgrader()
	scope := upgrade.Scope{Principal: f.principal, OwnerID: f.owner, Binding: f.binding}
	inspected, err := service.InspectRunner(t.Context(), scope)
	if err != nil || inspected.Installation == nil || inspected.Running.SHA256 != f.program.SHA256 || inspected.RunningFromSelectedRelease {
		t.Fatal(inspected, err)
	}
	digest, _ := selected.Digest()
	// The in-process fixture executes outside the installation. Equal bytes on
	// disk cannot make it current or bypass the stale installation precondition.
	request := upgrade.Request{SubmissionID: "old-running-directory", Binding: f.binding, InstallationID: f.observed.ID, ExpectedInstallationRevision: "999", ExpectedRunningSHA256: f.program.SHA256, Release: upgrade.ReleaseRef{ID: selected.ID, ManifestSHA256: digest}}
	current, err := service.StartUpgrade(t.Context(), scope, request)
	if err != nil || current.Operation.Admission != api.SubmissionNotAccepted || current.Operation.Failure == nil || current.Operation.Failure.Code != "INSTALLATION_CHANGED" || !current.Operation.Confirmed || current.Freshness != "live" {
		t.Fatal(current, err)
	}
	again, err := service.StartUpgrade(t.Context(), scope, request)
	if err != nil || again.Operation.ID != current.Operation.ID || calls != 1 {
		t.Fatal("duplicate was dispatched", again, calls, err)
	}
	changed := request
	changed.ExpectedInstallationRevision = "1"
	if _, err := service.StartUpgrade(t.Context(), scope, changed); err == nil {
		t.Fatal("changed original host submission was accepted")
	}
	selected.ID = "next"
	selected.Components = append([]upgrade.Component(nil), selected.Components...)
	selected.Components[len(selected.Components)-1].SHA256 = selected.ProgramSHA256()
	digest, _ = selected.Digest()
	request.SubmissionID = "queued-upgrade"
	request.ExpectedInstallationRevision = f.observed.Revision
	request.Release = upgrade.ReleaseRef{ID: selected.ID, ManifestSHA256: digest}
	accepted, err := service.StartUpgrade(t.Context(), scope, request)
	if err != nil || accepted.Operation.Admission != api.SubmissionAccepted || accepted.Operation.Phase != upgrade.Queued || accepted.Operation.Confirmed {
		t.Fatal("did not return durable immediate admission", accepted, err)
	}
	query := upgrade.Query{Binding: f.binding, InstallationID: f.observed.ID, SubmissionID: request.SubmissionID}
	// A new service client discovers the operation without browser storage.
	observed, err := f.app.RunnerUpgrader().GetUpgrade(t.Context(), scope, query)
	if err != nil || observed.Operation.ID != accepted.Operation.ID || observed.Freshness != "live" {
		t.Fatal(observed, err)
	}
	history, err := service.ListUpgrades(t.Context(), scope, upgrade.ListRequest{Binding: f.binding, InstallationID: f.observed.ID})
	if err != nil || history.Page.Active == nil || history.Page.Active.ID != accepted.Operation.ID || len(history.Page.Items) != 1 {
		t.Fatal(history, err)
	}
	f.disconnect()
	deadline := time.Now().Add(time.Second)
	for f.app.core.Online(f.binding.MachineID) && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	offline, err := service.GetUpgrade(t.Context(), scope, query)
	if err != nil || offline.Freshness != "last_known" || offline.Operation.ID != accepted.Operation.ID || offline.ObservationIssue == nil {
		t.Fatal("lost durable offline observation", offline, err)
	}
	history, err = service.ListUpgrades(t.Context(), scope, upgrade.ListRequest{Binding: f.binding, InstallationID: f.observed.ID})
	if err != nil || history.Freshness != "last_known" || history.Page.Active == nil {
		t.Fatal(history, err)
	}
	denied := scope
	denied.Principal = identity.User{ID: "another-user"}
	if _, err := service.GetUpgrade(t.Context(), denied, query); err == nil {
		t.Fatal("offline cache bypassed current permission")
	}
}

func TestRunnerUpgradeLiveHistoryFindsUndispatchedSubmissions(t *testing.T) {
	f := openInstalledRunnerFixture(t)
	digest, _ := f.manifest.Digest()
	request := upgrade.Request{SubmissionID: "reserved-before-host-crash", Binding: f.binding, InstallationID: f.observed.ID, ExpectedInstallationRevision: f.observed.Revision, ExpectedRunningSHA256: f.program.SHA256, Release: upgrade.ReleaseRef{ID: f.manifest.ID, ManifestSHA256: digest}}
	reserved, created, err := f.app.upgradeObservations.Reserve(t.Context(), f.owner, request)
	if err != nil || !created {
		t.Fatal(reserved, created, err)
	}
	scope := upgrade.Scope{Principal: f.principal, OwnerID: f.owner, Binding: f.binding}
	// Only the shared store survived the hypothetical host crash. A new client
	// has no submission key and must discover it even while the Runner is live.
	history, err := f.app.RunnerUpgrader().ListUpgrades(t.Context(), scope, upgrade.ListRequest{Binding: f.binding, InstallationID: f.observed.ID})
	if err != nil || history.Freshness != "live" || history.Page.Active != nil || len(history.Page.Items) != 1 {
		t.Fatal("live history lost undispatched submission", history, err)
	}
	item := history.Page.Items[0]
	if item.Request != request || item.Admission != api.SubmissionUnknown || item.ID != "" || item.Confirmed {
		t.Fatal("host reservation became executor evidence", item)
	}
}

func TestRunnerUpgradeHistoryKeepsPaginationAcrossAdmissionAndDisconnect(t *testing.T) {
	f := openInstalledRunnerFixture(t)
	server := httptest.NewServer(f.app)
	defer server.Close()
	config := fmt.Sprintf("gateway: ws://127.0.0.1:7443/api/v1/ws/tunnel\nupgrade_control_url: %s/api/v1/runner-upgrade-control\ntoken: %s\ntarget: %s\nsession_dir: %s\n", server.URL, f.credential, f.binding.MachineID, f.stateDir)
	if err := os.WriteFile(f.configPath, []byte(config), 0600); err != nil {
		t.Fatal(err)
	}
	f.app.upgradeSource = releaseSourceFunc(func(context.Context, upgrade.ReleaseRef, upgrade.Platform) (upgrade.Manifest, error) {
		return f.manifest, nil
	})
	digest, _ := f.manifest.Digest()
	request := upgrade.Request{Binding: f.binding, InstallationID: f.observed.ID, ExpectedInstallationRevision: "999", ExpectedRunningSHA256: f.program.SHA256, Release: upgrade.ReleaseRef{ID: f.manifest.ID, ManifestSHA256: digest}}
	var first upgrade.Observation
	for _, id := range []string{"first", "second", "third"} {
		request.SubmissionID = id
		reserved, created, err := f.app.upgradeObservations.Reserve(t.Context(), f.owner, request)
		if err != nil || !created {
			t.Fatal(reserved, created, err)
		}
		if id == "first" {
			first = reserved
		}
	}
	scope := upgrade.Scope{Principal: f.principal, OwnerID: f.owner, Binding: f.binding}
	query := upgrade.ListRequest{Binding: f.binding, InstallationID: f.observed.ID, Limit: 1}
	page, err := f.app.RunnerUpgrader().ListUpgrades(t.Context(), scope, query)
	if err != nil || len(page.Page.Items) != 1 || page.Page.Items[0].Request.SubmissionID != "third" || page.Page.NextCursor == "" {
		t.Fatal(page, err)
	}
	query.Cursor = page.Page.NextCursor
	// The original dispatcher completes its single send after pagination has
	// begun. A fresh client only observes it; it must never redispatch the key.
	var admitted upgrade.Operation
	submission := upgrade.Submission{Request: first.Operation.Request, ReservedAt: first.Operation.StartedAt}
	if err := (&runnerUpgrades{app: f.app}).call(t.Context(), scope, "runner.upgrade.start", submission, &admitted); err != nil {
		t.Fatal(err)
	}
	if admitted.Admission != api.SubmissionNotAccepted || !admitted.StartedAt.Equal(first.Operation.StartedAt) {
		t.Fatal("executor replaced original history position", admitted)
	}
	page, err = f.app.RunnerUpgrader().ListUpgrades(t.Context(), scope, query)
	if err != nil || len(page.Page.Items) != 1 || page.Page.Items[0].Request.SubmissionID != "second" || page.Page.NextCursor == "" {
		t.Fatal("live merge skipped or repeated a key", page, err)
	}
	query.Cursor = page.Page.NextCursor
	f.disconnect()
	deadline := time.Now().Add(time.Second)
	for f.app.core.Online(f.binding.MachineID) && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	page, err = f.app.RunnerUpgrader().ListUpgrades(t.Context(), scope, query)
	if err != nil || page.Freshness != "last_known" || len(page.Page.Items) != 1 || page.Page.Items[0].ID != admitted.ID || page.Page.NextCursor != "" {
		t.Fatal("offline continuation lost original cursor or repeated unknown", page, err)
	}
	denied := scope
	denied.Principal = identity.User{ID: "another-user"}
	if _, err := f.app.RunnerUpgrader().ListUpgrades(t.Context(), denied, query); err == nil {
		t.Fatal("history bypassed current permission")
	}
}
