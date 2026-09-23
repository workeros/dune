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
