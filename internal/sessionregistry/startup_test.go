package sessionregistry

import (
	"errors"
	"reflect"
	"testing"

	"github.com/aiomni/dune/pkg/api"
)

func TestStartupFailureNeedsProofAndFencesLateRegistration(t *testing.T) {
	r := openRegistry(t, privateDirectory(t), 8)
	key := testKey()
	key.Target.RuntimeID, key.Target.RuntimeIncarnation, key.Target.RuntimeGeneration = "", "", 0
	claim, _, err := r.ClaimKey(t.Context(), key, Digest("profile.start", nil), "registry:launch")
	if err != nil {
		t.Fatal(err)
	}
	runtime := api.Runtime{ID: "runtime", Incarnation: "original", Generation: 1, Adapter: "acp", State: "starting", ACPHost: &api.ACPHostInfo{Protocol: 1, Instance: "instance", Startup: &api.ACPStartupDiagnostic{Phase: "host_pending"}}}
	accepted, err := r.AcceptLaunch(t.Context(), claim, "original-ref", runtime)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := r.Progress(t.Context(), key, accepted.OperationRef, "host_starting", "", nil); err != nil {
		t.Fatal(err)
	}
	target := key.Target
	target.RuntimeID, target.RuntimeIncarnation, target.RuntimeGeneration = runtime.ID, runtime.Incarnation, 1
	resources := testResources(target, "instance")
	resources.Socket.Inode = 0
	host := HostRecord{Target: target, Instance: "instance", BootID: "boot", Runtime: runtime, Registration: []byte(`{}`), Resources: resources}
	if err := r.PrepareHost(t.Context(), host); err != nil {
		t.Fatal(err)
	}
	if err := r.StartupProgress(t.Context(), target, host.Instance, "host_pending", true); err != nil {
		t.Fatal(err)
	}
	if err := r.FailHost(t.Context(), target, host.Instance, "HOST_EXITED_BEFORE_ENTRY", func(HostRecord) error { return errors.New("inconclusive") }); err == nil {
		t.Fatal("unproven failure accepted")
	}
	pending, err := r.Get(t.Context(), key)
	if err != nil || pending.Stage != "host_starting" || !pending.Runtime.ACPHost.Startup.ConfirmationTimeout {
		t.Fatal(pending, err)
	}
	if err := r.ObserveHostPID(t.Context(), target, host.Instance, 1234); err != nil {
		t.Fatal(err)
	}
	if _, err := r.EnterHost(t.Context(), resources.Directory.Path, "boot", 9999); err == nil {
		t.Fatal("different process claimed original launch")
	}
	if err := r.FailHost(t.Context(), target, host.Instance, "HOST_EXITED_BEFORE_ENTRY", func(current HostRecord) error {
		if current.PID != 1234 {
			t.Fatal(current)
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	failed, err := r.Get(t.Context(), key)
	if err != nil || failed.Admission != api.SubmissionAccepted || failed.Stage != "failed" || failed.ErrorCode != "HOST_EXITED_BEFORE_ENTRY" || failed.SubmissionKey != key || failed.OperationRef != accepted.OperationRef || failed.Runtime.ID != runtime.ID {
		t.Fatal(failed, err)
	}
	if _, err := r.EnterHost(t.Context(), resources.Directory.Path, "boot", 1234); err == nil {
		t.Fatal("failed original host entered again")
	}
	host.PID, host.Resources.Socket.Inode = 1234, 2
	if err := r.RegisterHost(t.Context(), host); err == nil {
		t.Fatal("terminal failure allowed registration")
	}
	if _, err := r.RecordGroup(t.Context(), target, host.Instance, 0, 2345); err == nil {
		t.Fatal("terminal failure allowed Agent spawn")
	}
	if _, err := r.Progress(t.Context(), key, accepted.OperationRef, "started", "", nil); err == nil {
		t.Fatal("failure was overwritten by late success")
	}
	duplicate, receipt, err := r.ClaimKey(t.Context(), key, Digest("profile.start", nil), "registry:launch")
	if err != nil || duplicate.Acquired() || !reflect.DeepEqual(failed, receipt) {
		t.Fatal(receipt, err)
	}
}
