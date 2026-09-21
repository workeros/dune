package sessionregistry

import (
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"github.com/aiomni/dune/pkg/api"
	"testing"
)

// registerTestHost follows the same launch preparation and entry gates as fabricd.
func registerTestHost(t *testing.T, r *Registry, host HostRecord) {
	t.Helper()
	var encoded sql.NullString
	err := r.db.QueryRowContext(t.Context(), `SELECT launch_key FROM runtime_reservations WHERE target=?`, encodeTarget(host.Target)).Scan(&encoded)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		t.Fatal(err)
	}
	var receipt api.SubmissionReceipt
	if encoded.Valid {
		var key api.SubmissionKey
		if err := json.Unmarshal([]byte(encoded.String), &key); err != nil {
			t.Fatal(err)
		}
		receipt, err = r.Get(t.Context(), key)
	} else {
		key := api.SubmissionKey{SubmissionID: "launch-" + host.Target.RuntimeID, Target: host.Target}
		key.Target.RuntimeID, key.Target.RuntimeIncarnation, key.Target.RuntimeGeneration = "", "", 0
		claim, _, claimErr := r.ClaimKey(t.Context(), key, Digest("profile.start", nil), "registry:launch")
		if claimErr != nil {
			t.Fatal(claimErr)
		}
		receipt, err = r.AcceptLaunch(t.Context(), claim, "launch-"+host.Instance, host.Runtime)
	}
	if err != nil {
		t.Fatal(err)
	}
	if _, err := r.Progress(t.Context(), receipt.SubmissionKey, receipt.OperationRef, "host_starting", "", nil); err != nil {
		t.Fatal(err)
	}
	prepared := host
	prepared.PID, prepared.Resources.Socket.Inode = 0, 0
	if err := r.PrepareHost(t.Context(), prepared); err != nil {
		t.Fatal(err)
	}
	if _, err := r.EnterHost(t.Context(), host.Resources.Directory.Path, host.BootID, host.PID); err != nil {
		t.Fatal(err)
	}
	if err := r.RegisterHost(t.Context(), host); err != nil {
		t.Fatal(err)
	}
}

func TestPendingHostDiscoveryRetainsOriginalLaunchBeforeRegistration(t *testing.T) {
	dir := privateDirectory(t)
	r := openRegistry(t, dir, 4)
	key := testKey()
	key.Target.RuntimeID, key.Target.RuntimeIncarnation, key.Target.RuntimeGeneration = "", "", 0
	claim, _, err := r.ClaimKey(t.Context(), key, Digest("profile.start", nil), "registry:launch")
	if err != nil {
		t.Fatal(err)
	}
	runtime := api.Runtime{ID: "late-runtime", Incarnation: "original", Generation: 1, Adapter: "acp", State: "starting"}
	if _, err := r.AcceptLaunch(t.Context(), claim, "launch-ref", runtime); err != nil {
		t.Fatal(err)
	}
	if err := r.Close(); err != nil {
		t.Fatal(err)
	}
	r = openRegistry(t, dir, 4)
	page, err := r.Hosts(t.Context())
	if err != nil || len(page.Hosts) != 0 || len(page.Issues) != 1 || page.Issues[0].Runtime == nil || page.Issues[0].Runtime.ID != runtime.ID || page.Issues[0].Code != "HOST_REGISTRATION_PENDING" {
		t.Fatal(page, err)
	}
	key.Target.RuntimeID, key.Target.RuntimeIncarnation, key.Target.RuntimeGeneration = runtime.ID, runtime.Incarnation, 1
	host := HostRecord{Target: key.Target, Instance: "original-host", BootID: "boot", PID: 1234, Runtime: runtime, Registration: []byte(`{}`), Resources: testResources(key.Target, "original-host")}
	if err := r.RegisterHost(t.Context(), host); err == nil {
		t.Fatal("host registered without launch preparation")
	}
	registerTestHost(t, r, host)
	page, err = r.Hosts(t.Context())
	if err != nil || len(page.Hosts) != 1 || len(page.Issues) != 0 {
		t.Fatal("registration did not replace pending evidence", page, err)
	}
}

func TestHostDiscoveryPreservesHealthyRowsBesideCorruption(t *testing.T) {
	r := openRegistry(t, privateDirectory(t), 4)
	for i := range 3 {
		key := testKey()
		key.Target.RuntimeID = fmt.Sprint("runtime-", i)
		if err := reserveTestRuntime(t.Context(), r, key.Target); err != nil {
			t.Fatal(err)
		}
		instance := fmt.Sprint("host-", i)
		host := HostRecord{Target: key.Target, Instance: instance, BootID: "boot", PID: 1234, Runtime: api.Runtime{ID: key.Target.RuntimeID, Incarnation: key.Target.RuntimeIncarnation, Generation: 1, State: "running"}, Registration: []byte(`{}`), Resources: testResources(key.Target, instance)}
		registerTestHost(t, r, host)
		if i == 1 {
			if _, err := r.db.Exec(`UPDATE session_hosts SET runtime=? WHERE target=?`, []byte("damaged-private-content"), encodeTarget(key.Target)); err != nil {
				t.Fatal(err)
			}
		}
	}
	page, err := r.Hosts(t.Context())
	if err != nil || len(page.Hosts) != 2 || len(page.Issues) != 1 || page.Issues[0].Code != "REGISTRATION_INVALID" || page.Issues[0].Runtime == nil || page.Issues[0].Runtime.ID != "runtime-1" {
		t.Fatal(page, err)
	}
}

func TestHostEvidenceSurvivesReopenAndCannotReplaceIdentity(t *testing.T) {
	dir := privateDirectory(t)
	r := openRegistry(t, dir, 2)
	key := testKey()
	if err := reserveTestRuntime(t.Context(), r, key.Target); err != nil {
		t.Fatal(err)
	}
	host := HostRecord{Target: key.Target, Instance: "original-host", BootID: "kernel-boot", PID: 1234, Runtime: api.Runtime{ID: key.Target.RuntimeID, Incarnation: key.Target.RuntimeIncarnation, Generation: key.Target.RuntimeGeneration, State: "starting"}, Registration: api.Payload(map[string]string{"instance": "original-host"}), Resources: testResources(key.Target, "original-host")}
	registerTestHost(t, r, host)
	if err := r.RegisterHost(t.Context(), host); err == nil {
		t.Fatal("copied bootstrap replaced original host")
	}
	if _, _, err := r.ClaimKey(t.Context(), key, Digest("ordinary", nil), "host"); err != nil {
		t.Fatal(err)
	}
	generation, err := r.RecordGroup(t.Context(), key.Target, host.Instance, 0, 5678)
	if err != nil || generation != 1 {
		t.Fatal("ordinary exhaustion blocked preallocated host evidence", generation, err)
	}
	if _, err := r.RecordGroup(t.Context(), key.Target, "replacement-host", 1, 9999); err == nil {
		t.Fatal("foreign instance replaced group evidence")
	}
	if _, err := r.RecordGroup(t.Context(), key.Target, host.Instance, 0, 9999); err == nil {
		t.Fatal("stale process generation replaced group evidence")
	}
	if err := r.Close(); err != nil {
		t.Fatal(err)
	}
	r = openRegistry(t, dir, 2)
	stored, err := r.Host(t.Context(), key.Target)
	if err != nil || stored.Instance != host.Instance || stored.PID != host.PID || stored.GroupID != 5678 || stored.GroupGeneration != 1 {
		t.Fatal(stored, err)
	}
	hosts, err := r.Hosts(t.Context())
	if err != nil || len(hosts.Hosts) != 1 || len(hosts.Issues) != 0 || hosts.Hosts[0].Target != key.Target {
		t.Fatal(hosts, err)
	}
	key.SubmissionID = "forget"
	if _, _, err := r.AcceptForget(t.Context(), key, "original-cleanup", func(HostRecord) error { return nil }); err != nil {
		t.Fatal(err)
	}
	if _, err := r.RecordGroup(t.Context(), key.Target, host.Instance, 1, 9999); err == nil {
		t.Fatal("new process crossed cleanup seal")
	}
}

func TestExitedHostCannotRegisterAnotherProcess(t *testing.T) {
	r := openRegistry(t, privateDirectory(t), 1)
	key := testKey()
	if err := reserveTestRuntime(t.Context(), r, key.Target); err != nil {
		t.Fatal(err)
	}
	runtime := api.Runtime{ID: key.Target.RuntimeID, Incarnation: key.Target.RuntimeIncarnation, Generation: key.Target.RuntimeGeneration, State: "running"}
	registerTestHost(t, r, HostRecord{Target: key.Target, Instance: "host", BootID: "boot", PID: 1234, Runtime: runtime, Registration: []byte(`{}`), Resources: testResources(key.Target, "host")})
	runtime.State = "exited"
	if err := r.RecordHostRuntime(t.Context(), key.Target, "host", runtime); err != nil {
		t.Fatal(err)
	}
	if _, err := r.RecordGroup(t.Context(), key.Target, "host", 0, 5678); err == nil {
		t.Fatal("exited identity spawned a replacement")
	}
	runtime.State = "running"
	if err := r.RecordHostRuntime(t.Context(), key.Target, "host", runtime); err == nil {
		t.Fatal("terminal evidence was reversed")
	}
}
