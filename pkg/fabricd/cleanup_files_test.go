package fabricd

import (
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/aiomni/dune/internal/sessionregistry"
	"github.com/aiomni/dune/internal/wire"
)

func cleanupDirectoryFixture(t *testing.T) (sessionregistry.FileIdentity, string, string) {
	t.Helper()
	root := t.TempDir()
	directory := filepath.Join(root, wire.ID())
	if err := os.Mkdir(directory, 0700); err != nil {
		t.Fatal(err)
	}
	instance := wire.ID()
	if err := savePrivateFile(filepath.Join(directory, "instance.json"), instance); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(directory, "cache"), []byte("Dune cache"), 0600); err != nil {
		t.Fatal(err)
	}
	project := filepath.Join(t.TempDir(), "project-file")
	if err := os.WriteFile(project, []byte("project data"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Dir(project), filepath.Join(directory, "external")); err != nil {
		t.Fatal(err)
	}
	identity, err := fileIdentity(directory, os.ModeDir)
	if err != nil {
		t.Fatal(err)
	}
	return identity, instance, project
}

func TestCleanupDirectoryRecoversEachPhysicalBoundaryWithoutFollowingProjectLinks(t *testing.T) {
	for _, point := range []string{"quarantined", "contents_removed", "marker_removed"} {
		t.Run(point, func(t *testing.T) {
			identity, instance, project := cleanupDirectoryFixture(t)
			interrupted := errors.New("interrupted cleanup")
			err := removeCleanupResource(t.Context(), identity, instance, os.ModeDir, func(at string) error {
				if at == point {
					return interrupted
				}
				return nil
			})
			if !errors.Is(err, interrupted) {
				t.Fatal("did not reach physical checkpoint", err)
			}
			if err := removeCleanupResource(t.Context(), identity, instance, os.ModeDir, func(string) error { return nil }); err != nil {
				t.Fatal("original quarantine could not resume", err)
			}
			if _, err := os.Lstat(identity.Path); !os.IsNotExist(err) {
				t.Fatal(err)
			}
			if _, err := os.Lstat(filepath.Join(filepath.Dir(identity.Path), ".forget-"+instance)); !os.IsNotExist(err) {
				t.Fatal(err)
			}
			if body, err := os.ReadFile(project); err != nil || string(body) != "project data" {
				t.Fatal("followed project symlink", err)
			}
			if err := removeCleanupResource(t.Context(), identity, instance, os.ModeDir, func(string) error { return nil }); err != nil {
				t.Fatal("completed physical deletion was not idempotent", err)
			}
		})
	}
}

func TestCleanupPreservesReusedOriginalAndQuarantineDirectories(t *testing.T) {
	for _, replace := range []string{"original", "quarantine", "quarantine-symlink"} {
		t.Run(replace, func(t *testing.T) {
			identity, instance, project := cleanupDirectoryFixture(t)
			other := filepath.Join(filepath.Dir(identity.Path), "replaced-original")
			var newPath string
			reuse := func(path string) error {
				if err := os.Rename(path, other); err != nil {
					return err
				}
				newPath = path
				if replace == "quarantine-symlink" {
					return os.Symlink(filepath.Dir(project), path)
				}
				if err := os.Mkdir(path, 0700); err != nil {
					return err
				}
				return os.WriteFile(filepath.Join(path, "keep"), []byte("new owner's content"), 0600)
			}
			if replace == "original" {
				if err := reuse(identity.Path); err != nil {
					t.Fatal(err)
				}
			}
			err := removeCleanupResource(t.Context(), identity, instance, os.ModeDir, func(point string) error {
				if replace != "original" && point == "quarantined" {
					return reuse(filepath.Join(filepath.Dir(identity.Path), ".forget-"+instance))
				}
				return nil
			})
			if err == nil {
				t.Fatal("accepted a reused resource")
			}
			if replace != "quarantine-symlink" {
				if body, err := os.ReadFile(filepath.Join(newPath, "keep")); err != nil || string(body) != "new owner's content" {
					t.Fatal("deleted reused directory", err)
				}
			}
			if _, err := os.ReadFile(filepath.Join(other, "cache")); err != nil {
				t.Fatal("deleted moved original after identity mismatch", err)
			}
			if body, err := os.ReadFile(project); err != nil || string(body) != "project data" {
				t.Fatal("deleted external project", err)
			}
		})
	}
}
