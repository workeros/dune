package migrate

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/aiomni/dune/internal/identity"
	"github.com/aiomni/dune/internal/metadata"
	"github.com/aiomni/dune/pkg/storage"
)

func TestSQLiteBackupAndRestore(t *testing.T) {
	ctx := context.Background()
	dir := filepath.Join(t.TempDir(), "source")
	backup := storage.Config{SQLiteDir: filepath.Join(t.TempDir(), "backup")}
	if _, err := SQLite(ctx, dir, backup); err == nil {
		t.Fatal("missing source accepted")
	}
	if _, err := os.Stat(backup.SQLiteDir); !os.IsNotExist(err) {
		t.Fatal("invalid source created a target")
	}
	s, err := metadata.Open(ctx, storage.Config{SQLiteDir: dir})
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	local := identity.NewLocal(s, true)
	user, cookie, err := local.Register(ctx, "backup@example.test", "backup-test-password")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := SQLite(ctx, dir, backup); err == nil {
		t.Fatal("live source copied")
	}
	s.Close()
	if _, err := SQLite(ctx, dir, storage.Config{SQLiteDir: dir}); err == nil {
		t.Fatal("source also accepted as target")
	}
	if _, err := SQLite(ctx, dir, backup); err != nil {
		t.Fatal(err)
	}
	restored := storage.Config{SQLiteDir: filepath.Join(t.TempDir(), "restored")}
	if _, err := SQLite(ctx, backup.SQLiteDir, restored); err != nil {
		t.Fatal(err)
	}
	s, err = metadata.Open(ctx, restored)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	local = identity.NewLocal(s, true)
	if got, err := local.Authenticate(ctx, cookie); err != nil || got.ID != user.ID {
		t.Fatal("backup lost session", err)
	}
	if got, _, err := local.Login(ctx, user.Email, "backup-test-password"); err != nil || got.ID != user.ID {
		t.Fatal("backup lost password", err)
	}
}

func TestClusterRecoveryRejectsSQLiteBeforeCreatingState(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "must-not-create")
	for _, expected := range []string{"", "old-generation"} {
		if _, err := ClusterRecovery(context.Background(), storage.Config{SQLiteDir: dir}, expected); err == nil {
			t.Fatal("SQLite accepted cluster recovery")
		}
	}
	if _, err := os.Stat(dir); !os.IsNotExist(err) {
		t.Fatal("rejected recovery created a SQLite directory", err)
	}
}
