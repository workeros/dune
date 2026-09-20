package sessionregistry

import (
	"fmt"
	"github.com/aiomni/dune/pkg/api"
	"testing"
)

func TestHostDiscoveryPreservesHealthyRowsBesideCorruption(t *testing.T) {
	r := openRegistry(t, privateDirectory(t), 4)
	for i := range 3 {
		key := testKey()
		key.Target.RuntimeID = fmt.Sprint("runtime-", i)
		if err := r.ReserveRuntime(t.Context(), key.Target); err != nil {
			t.Fatal(err)
		}
		instance := fmt.Sprint("host-", i)
		host := HostRecord{Target: key.Target, Instance: instance, BootID: "boot", PID: 1234, Runtime: api.Runtime{ID: key.Target.RuntimeID, Incarnation: key.Target.RuntimeIncarnation, Generation: 1, State: "running"}, Registration: []byte(`{}`), Resources: testResources(key.Target, instance)}
		if err := r.RegisterHost(t.Context(), host); err != nil {
			t.Fatal(err)
		}
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
	r := openRegistry(t, dir, 1)
	key := testKey()
	if err := r.ReserveRuntime(t.Context(), key.Target); err != nil {
		t.Fatal(err)
	}
	host := HostRecord{Target: key.Target, Instance: "original-host", BootID: "kernel-boot", PID: 1234, Runtime: api.Runtime{ID: key.Target.RuntimeID, Incarnation: key.Target.RuntimeIncarnation, Generation: key.Target.RuntimeGeneration, State: "starting"}, Registration: api.Payload(map[string]string{"instance": "original-host"}), Resources: testResources(key.Target, "original-host")}
	if err := r.RegisterHost(t.Context(), host); err != nil {
		t.Fatal(err)
	}
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
	r = openRegistry(t, dir, 1)
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
	if err := r.ReserveRuntime(t.Context(), key.Target); err != nil {
		t.Fatal(err)
	}
	runtime := api.Runtime{ID: key.Target.RuntimeID, Incarnation: key.Target.RuntimeIncarnation, Generation: key.Target.RuntimeGeneration, State: "running"}
	if err := r.RegisterHost(t.Context(), HostRecord{Target: key.Target, Instance: "host", BootID: "boot", PID: 1234, Runtime: runtime, Registration: []byte(`{}`), Resources: testResources(key.Target, "host")}); err != nil {
		t.Fatal(err)
	}
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
