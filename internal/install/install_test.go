package install

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/aiomni/dune/internal/config"
)

func TestCleanupOnlyOwnedOldReleases(t *testing.T) {
	root := t.TempDir()
	current := filepath.Join(root, "new")
	for _, name := range []string{"old", "new", "unowned"} {
		path := filepath.Join(root, name)
		if err := os.Mkdir(path, 0700); err != nil {
			t.Fatal(err)
		}
		if name != "unowned" {
			if err := os.WriteFile(filepath.Join(path, ".dune-release"), []byte("1\n"), 0600); err != nil {
				t.Fatal(err)
			}
		}
	}
	outside := t.TempDir()
	if err := os.Symlink(outside, filepath.Join(root, "alias")); err != nil {
		t.Fatal(err)
	}
	if err := cleanupReleases(root, current); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(root, "old")); !os.IsNotExist(err) {
		t.Fatal("old release not removed")
	}
	for _, name := range []string{"new", "unowned", "alias"} {
		if _, err := os.Lstat(filepath.Join(root, name)); err != nil {
			t.Fatal(name, err)
		}
	}
}
func TestPersistentPathResolvesParentAliases(t *testing.T) {
	root := t.TempDir()
	target := t.TempDir()
	alias := filepath.Join(root, "alias")
	if err := os.Symlink(target, alias); err != nil {
		t.Fatal(err)
	}
	got, err := resolveExistingParent(filepath.Join(alias, "missing", "config.yaml"))
	canonical, evalErr := filepath.EvalSymlinks(target)
	if err != nil || evalErr != nil || got != filepath.Join(canonical, "missing", "config.yaml") {
		t.Fatalf("%s %v", got, err)
	}
}

func TestInstallationRejectsAliasedReleaseDirectory(t *testing.T) {
	root := t.TempDir()
	configPath := filepath.Join(root, "config.yaml")
	if err := config.Init(configPath, "127.0.0.1:7443", ""); err != nil {
		t.Fatal(err)
	}
	outside := t.TempDir()
	if err := os.Symlink(outside, filepath.Join(root, "releases")); err != nil {
		t.Fatal(err)
	}
	err := Run(context.Background(), "repair", configPath, root, "test")
	if err == nil || !strings.Contains(err.Error(), "releases must be an owned directory") {
		t.Fatalf("aliased release directory accepted: %v", err)
	}
	entries, err := os.ReadDir(outside)
	if err != nil || len(entries) != 0 {
		t.Fatalf("installer modified the external directory: %v %v", entries, err)
	}
}
