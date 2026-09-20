package sessionregistry

import (
	"bytes"
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/aiomni/dune/pkg/api"
)

func testKey() api.SubmissionKey {
	return api.SubmissionKey{SubmissionID: "before-first-send", Target: api.SubmissionTarget{
		OwnerID: "tenant", RunnerID: "runner", FabricID: "fabric", MachineID: "machine", BindingRevision: 1,
		RuntimeID: "runtime", RuntimeIncarnation: "runtime-boot", RuntimeGeneration: 1,
	}}
}

func privateDirectory(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	if err := os.Chmod(dir, 0700); err != nil {
		t.Fatal(err)
	}
	return dir
}

func openRegistry(t *testing.T, dir string, limit int) *Registry {
	t.Helper()
	r, err := Open(t.Context(), dir, Options{MaxKeys: limit})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { r.Close() })
	return r
}

func requireCode(t *testing.T, err error, code string) {
	t.Helper()
	var failure *api.Error
	if !errors.As(err, &failure) || failure.Code != code {
		t.Fatalf("want %s, got %v", code, err)
	}
}

func TestReadBeforeAdmissionDoesNotCloseOrConsumeKey(t *testing.T) {
	r := openRegistry(t, privateDirectory(t), 1)
	key := testKey()
	for range 3 {
		receipt, err := r.Get(t.Context(), key)
		if err != nil || receipt.Admission != api.SubmissionUnknown || receipt.SubmissionKey != key {
			t.Fatalf("absent query: %+v, %v", receipt, err)
		}
	}
	claim, receipt, err := r.ClaimKey(t.Context(), key, Digest("prompt", []byte("task")), "host-one")
	if err != nil || !claim.Acquired() || receipt.Admission != api.SubmissionUnknown {
		t.Fatalf("read consumed or closed admission: %+v, %v", receipt, err)
	}
	reserved, err := r.Get(t.Context(), key)
	if err != nil || reserved.Admission != api.SubmissionUnknown {
		t.Fatalf("key occupation masqueraded as acceptance: %+v, %v", reserved, err)
	}
	accepted, err := r.Accept(t.Context(), claim, "original-operation")
	if err != nil || accepted.Admission != api.SubmissionAccepted || accepted.OperationRef != "original-operation" {
		t.Fatalf("late admission: %+v, %v", accepted, err)
	}
}

func TestClaimsSurviveReopenWithoutGrantingExecutionAgain(t *testing.T) {
	dir := privateDirectory(t)
	r := openRegistry(t, dir, 4)
	key := testKey()
	digest := Digest("prompt", []byte("never persist this secret prompt"))
	claim, _, err := r.ClaimKey(t.Context(), key, digest, "host-one")
	if err != nil || !claim.Acquired() {
		t.Fatal(err)
	}
	if err := r.Close(); err != nil {
		t.Fatal(err)
	}
	r = openRegistry(t, dir, 4)
	duplicate, receipt, err := r.ClaimKey(t.Context(), key, digest, "host-one")
	if err != nil || duplicate.Acquired() || receipt.Admission != api.SubmissionUnknown {
		t.Fatalf("unfinished claim executed after reopen: %+v, %v", receipt, err)
	}
	if _, err := r.Accept(t.Context(), duplicate, "replacement"); err == nil {
		t.Fatal("duplicate obtained admission authority")
	}
	// The original surviving owner can finish the same decision after an IPC
	// reconnection; the process that reopened the registry gets no new claim.
	if _, err := r.Accept(t.Context(), claim, "original"); err != nil {
		t.Fatal(err)
	}
	if err := r.Close(); err != nil {
		t.Fatal(err)
	}
	r = openRegistry(t, dir, 4)
	duplicate, receipt, err = r.ClaimKey(t.Context(), key, digest, "host-one")
	if err != nil || duplicate.Acquired() || receipt.Admission != api.SubmissionAccepted || receipt.OperationRef != "original" {
		t.Fatalf("accepted operation changed after reopen: %+v, %v", receipt, err)
	}
	data, err := os.ReadFile(filepath.Join(dir, "registry.sqlite"))
	if err != nil || bytes.Contains(data, []byte("never persist this secret prompt")) {
		t.Fatalf("registry retained request body: %v", err)
	}
}

func TestRejectionCannotReverseOrExpireUnderCapacityPressure(t *testing.T) {
	r := openRegistry(t, privateDirectory(t), 1)
	key := testKey()
	digest := Digest("prompt", []byte("task"))
	claim, _, err := r.ClaimKey(t.Context(), key, digest, "host-one")
	if err != nil {
		t.Fatal(err)
	}
	rejected, err := r.Reject(t.Context(), claim, "CONVERSATION_CHANGED")
	if err != nil || rejected.Admission != api.SubmissionNotAccepted {
		t.Fatal(rejected, err)
	}
	_, err = r.Accept(t.Context(), claim, "must-not-run")
	requireCode(t, err, "SUBMISSION_CONFLICT")
	other := key
	other.SubmissionID = "new-intent"
	_, _, err = r.ClaimKey(t.Context(), other, digest, "host-one")
	requireCode(t, err, "SUBMISSION_CAPACITY_EXHAUSTED")
	duplicate, receipt, err := r.ClaimKey(t.Context(), key, digest, "host-one")
	if err != nil || duplicate.Acquired() || receipt.Admission != api.SubmissionNotAccepted || receipt.ErrorCode != "CONVERSATION_CHANGED" {
		t.Fatalf("capacity pressure lost rejection: %+v, %v", receipt, err)
	}
}

func TestRuntimeScopesAndAdmissionOwnersCannotAlias(t *testing.T) {
	r := openRegistry(t, privateDirectory(t), 10)
	key := testKey()
	digest := Digest("prompt", []byte("task"))
	if claim, _, err := r.ClaimKey(t.Context(), key, digest, "host-one"); err != nil || !claim.Acquired() {
		t.Fatal(err)
	}
	for _, change := range []struct {
		digest   [32]byte
		receiver string
	}{{Digest("forget", nil), "registry"}, {digest, "registry"}, {Digest("prompt", []byte("different task")), "host-one"}} {
		_, _, err := r.ClaimKey(t.Context(), key, change.digest, change.receiver)
		requireCode(t, err, "SUBMISSION_CONFLICT")
	}
	for _, change := range []func(*api.SubmissionKey){
		func(k *api.SubmissionKey) { k.Target.RuntimeID = "other-runtime" },
		func(k *api.SubmissionKey) { k.Target.RuntimeIncarnation = "other-incarnation" },
		func(k *api.SubmissionKey) { k.Target.RuntimeGeneration++ },
		func(k *api.SubmissionKey) { k.Target.BindingRevision++ },
		func(k *api.SubmissionKey) { k.Target.OwnerID = "other-tenant" },
		func(k *api.SubmissionKey) {
			k.Target.RuntimeID, k.Target.RuntimeIncarnation, k.Target.RuntimeGeneration = "", "", 0
		},
	} {
		other := key
		change(&other)
		if claim, _, err := r.ClaimKey(t.Context(), other, digest, "host-one"); err != nil || !claim.Acquired() {
			t.Fatalf("independent scope collided: %+v, %v", other, err)
		}
	}
}

func TestConcurrentRegistryConnectionsGrantExactlyOneClaim(t *testing.T) {
	dir := privateDirectory(t)
	registries := []*Registry{openRegistry(t, dir, 4), openRegistry(t, dir, 4)}
	ready := make(chan struct{})
	var group sync.WaitGroup
	winners := make(chan Claim, 16)
	for i := range 16 {
		group.Go(func() {
			<-ready
			claim, _, err := registries[i%2].ClaimKey(t.Context(), testKey(), Digest("prompt", []byte("task")), "host-one")
			if err != nil {
				t.Error(err)
			} else if claim.Acquired() {
				winners <- claim
			}
		})
	}
	close(ready)
	group.Wait()
	close(winners)
	if len(winners) != 1 {
		t.Fatalf("granted %d executions for one key", len(winners))
	}
	if _, err := registries[1].Accept(t.Context(), <-winners, "original"); err != nil {
		t.Fatal(err)
	}
}

func TestRegistryProcessAdmission(t *testing.T) {
	if dir := os.Getenv("DUNE_REGISTRY_TEST_DIRECTORY"); dir != "" {
		r, err := Open(context.Background(), dir, Options{MaxKeys: 4})
		if err != nil {
			t.Fatal(err)
		}
		defer r.Close()
		claim, _, err := r.ClaimKey(context.Background(), testKey(), Digest("prompt", []byte("task")), "host-one")
		if err != nil {
			t.Fatal(err)
		}
		if claim.Acquired() {
			if _, err := r.Accept(context.Background(), claim, "process-operation"); err != nil {
				t.Fatal(err)
			}
			// A real process exits here; a second process may only observe this
			// original admission, never receive a replacement claim.
			if err := os.WriteFile(filepath.Join(dir, "winner-"+os.Getenv("DUNE_REGISTRY_TEST_CHILD")), nil, 0600); err != nil {
				t.Fatal(err)
			}
		}
		return
	}
	dir := privateDirectory(t)
	var group sync.WaitGroup
	for _, child := range []string{"a", "b", "c"} {
		group.Go(func() {
			cmd := exec.Command(os.Args[0], "-test.run=^TestRegistryProcessAdmission$", "-test.count=1")
			cmd.Env = append(os.Environ(), "DUNE_REGISTRY_TEST_DIRECTORY="+dir, "DUNE_REGISTRY_TEST_CHILD="+child)
			if output, err := cmd.CombinedOutput(); err != nil {
				t.Errorf("child admission failed: %s, %v", output, err)
			}
		})
	}
	group.Wait()
	winners, err := filepath.Glob(filepath.Join(dir, "winner-*"))
	if err != nil || len(winners) != 1 {
		t.Fatalf("processes executed %d times: %v", len(winners), err)
	}
	r := openRegistry(t, dir, 4)
	receipt, err := r.Get(t.Context(), testKey())
	if err != nil || receipt.Admission != api.SubmissionAccepted || receipt.OperationRef != "process-operation" {
		t.Fatalf("process exit lost original admission: %+v, %v", receipt, err)
	}
}

func TestRegistryRejectsUnsafePathsAndConfigurationChanges(t *testing.T) {
	t.Run("directory symlink", func(t *testing.T) {
		root := t.TempDir()
		link := filepath.Join(root, "link")
		if err := os.Symlink(t.TempDir(), link); err != nil {
			t.Fatal(err)
		}
		if r, err := Open(t.Context(), link, Options{}); err == nil {
			r.Close()
			t.Fatal("accepted symlink directory")
		}
	})
	for _, suffix := range []string{"", "-journal", "-wal", "-shm"} {
		t.Run("file symlink"+suffix, func(t *testing.T) {
			dir := privateDirectory(t)
			outside := filepath.Join(t.TempDir(), "preserve")
			if err := os.WriteFile(outside, []byte("preserve"), 0600); err != nil {
				t.Fatal(err)
			}
			if err := os.Symlink(outside, filepath.Join(dir, "registry.sqlite"+suffix)); err != nil {
				t.Fatal(err)
			}
			if r, err := Open(t.Context(), dir, Options{}); err == nil {
				r.Close()
				t.Fatal("accepted symlink file")
			}
			data, err := os.ReadFile(outside)
			if err != nil || string(data) != "preserve" {
				t.Fatal("modified symlink target", err)
			}
		})
	}
	t.Run("capacity changes", func(t *testing.T) {
		dir := privateDirectory(t)
		openRegistry(t, dir, 2)
		if r, err := Open(t.Context(), dir, Options{MaxKeys: 3}); err == nil {
			r.Close()
			t.Fatal("second process silently changed shared capacity")
		}
	})
	t.Run("invalid UTF8 target", func(t *testing.T) {
		r := openRegistry(t, privateDirectory(t), 1)
		key := testKey()
		key.Target.OwnerID = strings.Repeat(string([]byte{255}), 2)
		_, _, err := r.ClaimKey(t.Context(), key, Digest("prompt", nil), "host")
		requireCode(t, err, "INVALID_ARGUMENT")
	})
}
