package sessionregistry

import (
	"github.com/aiomni/dune/pkg/api"
	"testing"
)

func TestHostEvidenceSurvivesReopenAndCannotReplaceIdentity(t *testing.T) {
	dir := privateDirectory(t)
	r := openRegistry(t, dir, 1)
	key := testKey()
	if err := r.ReserveRuntime(t.Context(), key.Target); err != nil {
		t.Fatal(err)
	}
	host := HostRecord{Target: key.Target, Instance: "original-host", BootID: "kernel-boot", PID: 1234, Runtime: api.Runtime{ID: key.Target.RuntimeID, Incarnation: key.Target.RuntimeIncarnation, Generation: key.Target.RuntimeGeneration, State: "starting"}, Registration: api.Payload(map[string]string{"instance": "original-host"})}
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
	if err != nil || len(hosts) != 1 || hosts[0].Target != key.Target {
		t.Fatal(hosts, err)
	}
	key.SubmissionID = "forget"
	claim, _, err := r.ClaimControl(t.Context(), key, Digest("forget", nil), "registry", ControlForget, "")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := r.AcceptForget(t.Context(), claim, "original-cleanup"); err != nil {
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
	if err := r.RegisterHost(t.Context(), HostRecord{Target: key.Target, Instance: "host", BootID: "boot", PID: 1234, Runtime: runtime, Registration: []byte(`{}`)}); err != nil {
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
