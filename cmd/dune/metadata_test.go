package main

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/aiomni/dune/pkg/storage"
)

func TestClusterRecoveryRejectsSQLiteBeforeCreatingState(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "metadata")
	if _, err := clusterRecovery(context.Background(), storage.Config{SQLiteDir: dir}, ""); err == nil {
		t.Fatal("SQLite recovery accepted")
	}
	if _, err := os.Stat(dir); !os.IsNotExist(err) {
		t.Fatal("rejected recovery created state", err)
	}
}
