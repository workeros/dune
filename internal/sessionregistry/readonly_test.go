package sessionregistry

import (
	"bytes"
	"os"
	"path/filepath"
	"testing"
)

func TestReadOnlyRegistryDoesNotCreateOrChangeEvidence(t *testing.T) {
	missing := filepath.Join(privateDirectory(t), "absent")
	if _, err := OpenReadOnly(t.Context(), missing); !os.IsNotExist(err) {
		t.Fatal("missing read-only registry", err)
	}
	if _, err := os.Lstat(missing); !os.IsNotExist(err) {
		t.Fatal("read created directory", err)
	}
	dir := privateDirectory(t)
	writable := openRegistry(t, dir, 4)
	key := testKey()
	claim, _, err := writable.ClaimKey(t.Context(), key, Digest("prompt", nil), "original-host")
	if err != nil {
		t.Fatal(err)
	}
	want, err := writable.Accept(t.Context(), claim, "original-operation")
	if err != nil {
		t.Fatal(err)
	}
	before, err := os.ReadFile(filepath.Join(dir, "registry.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	reader, err := OpenReadOnly(t.Context(), dir)
	if err != nil {
		t.Fatal(err)
	}
	defer reader.Close()
	got, err := reader.Get(t.Context(), key)
	if err != nil || got.OperationRef != want.OperationRef || got.SubmissionKey != key || got.Admission != want.Admission {
		t.Fatal(got, err)
	}
	key.SubmissionID = "cannot-write"
	if _, _, err := reader.ClaimKey(t.Context(), key, Digest("prompt", nil), "other-host"); err == nil {
		t.Fatal("read-only handle admitted a write")
	}
	after, err := os.ReadFile(filepath.Join(dir, "registry.sqlite"))
	if err != nil || !bytes.Equal(before, after) {
		t.Fatal("read modified durable evidence", err)
	}
}
