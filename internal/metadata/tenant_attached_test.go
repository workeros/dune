package metadata

import (
	"context"
	"errors"
	"path/filepath"
	"testing"

	publicidentity "github.com/aiomni/dune/pkg/identity"
	"github.com/aiomni/dune/pkg/storage"
)

func TestTenantAttachedEnrollmentPreallocatesOwnedRunner(t *testing.T) {
	ctx := context.Background()
	store, err := Open(ctx, storage.Config{SQLiteDir: filepath.Join(t.TempDir(), "metadata")}, OpenOptions{ExternalIdentity: true})
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	user := publicidentity.User{ID: "email:creator@example.test", Namespace: "sanddance", Kind: "email", Subject: "creator@example.test", Email: "creator@example.test"}
	logical, token, expires, err := store.IssueTenantEnrollment(ctx, user, "tenant-1", "Creator Devbox", "session-hash")
	if err != nil || logical.ID == "" || logical.Kind != "attached" || expires <= 0 {
		t.Fatal(logical, expires, err)
	}
	pending, err := store.RunnerResource(ctx, logical.ID)
	if err != nil || pending.OwnerID != "tenant-1" || pending.Runner.Binding != nil || pending.Runner.Name != "Creator Devbox" {
		t.Fatal("pending tenant Runner", pending, err)
	}
	machine, credential, err := store.Enroll(ctx, token, "linux", "arm64")
	if err != nil || machine.RunnerID != logical.ID || machine.ID == "" || credential == "" {
		t.Fatal(machine, err)
	}
	ready, err := store.RunnerResource(ctx, logical.ID)
	if err != nil || ready.OwnerID != "tenant-1" || ready.Runner.Binding == nil || ready.Runner.Binding.MachineID != machine.ID {
		t.Fatal("ready tenant Runner", ready, err)
	}
	if _, _, err := store.Enroll(ctx, token, "linux", "arm64"); !errors.Is(err, publicidentity.ErrUnauthorized) {
		t.Fatal("tenant enrollment was reusable", err)
	}
}
