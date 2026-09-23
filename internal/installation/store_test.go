package installation

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/aiomni/dune/internal/launchgate"
	"github.com/aiomni/dune/internal/statecontract"
	"github.com/aiomni/dune/internal/wire"
	"github.com/aiomni/dune/pkg/api"
	"github.com/aiomni/dune/pkg/upgrade"
)

func digest(value string) string {
	hash := sha256.Sum256([]byte(value))
	return hex.EncodeToString(hash[:])
}

func location(t *testing.T, root, id string) Location {
	t.Helper()
	l := Location{Directory: filepath.Join("releases", wire.ID()), Manifest: upgrade.Manifest{ID: id, Platform: upgrade.Platform{OS: "linux", Arch: "amd64"}, ArchiveURL: "https://releases.example/" + id, ArchiveSHA256: digest(id), StateContract: statecontract.ID()}}
	for _, path := range []string{"dune", "tmux", "rg", "licenses/NOTICE"} {
		body := path
		if path != "dune" {
			body += id
		}
		mode := uint32(0700)
		if path == "licenses/NOTICE" {
			mode = 0600
		}
		l.Manifest.Components = append(l.Manifest.Components, upgrade.Component{Path: path, SHA256: digest(body), Bytes: int64(len(body)), Mode: mode})
		file := filepath.Join(root, l.Directory, path)
		if err := os.MkdirAll(filepath.Dir(file), 0700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(file, []byte(body), os.FileMode(mode)); err != nil {
			t.Fatal(err)
		}
	}
	return l
}

func installationFixture(t *testing.T) (*Store, Location, launchgate.Seal) {
	t.Helper()
	root, state := t.TempDir(), t.TempDir()
	for _, dir := range []string{root, state} {
		if err := os.Chmod(dir, 0700); err != nil {
			t.Fatal(err)
		}
	}
	config := filepath.Join(state, "config.yaml")
	if err := os.WriteFile(config, []byte("must retain original configuration bytes\n"), 0600); err != nil {
		t.Fatal(err)
	}
	store, err := Lock(root)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { store.Close() })
	source := location(t, root, "R1")
	if err := os.Symlink(source.Directory, filepath.Join(root, "current")); err != nil {
		t.Fatal(err)
	}
	metadata := Metadata{ID: "installation", Method: "service", ServiceName: "dune-test", StateDir: state, ConfigPath: config}
	if _, err := store.Initialize(t.Context(), metadata, source); err != nil {
		t.Fatal(err)
	}
	seal := launchgate.Seal{InstallationID: metadata.ID, OperationID: "operation", Owner: "worker-1"}
	gate, err := launchgate.Acquire(state, true)
	if err != nil {
		t.Fatal(err)
	}
	if err := gate.ClaimSeal(nil, seal); err != nil {
		t.Fatal(err)
	}
	gate.Close()
	return store, source, seal
}

func TestSourceRevisionIncludesHelpersAndCannotBeReusedAfterRollback(t *testing.T) {
	store, source, seal := installationFixture(t)
	original, err := store.Observe(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	target := location(t, store.root, "R2")
	if target.Manifest.ProgramSHA256() != original.Release.ProgramSHA256() {
		t.Fatal("fixture changed Dune")
	}
	if err := store.Switch(t.Context(), seal, original.Revision, target); err != nil {
		t.Fatal(err)
	}
	updated, err := store.Observe(t.Context())
	if err != nil || updated.Revision == original.Revision {
		t.Fatal(updated, err)
	}
	if err := store.Restore(t.Context(), seal, source, original.Components); err != nil {
		t.Fatal(err)
	}
	restored, err := store.Observe(t.Context())
	if err != nil || restored.Revision == original.Revision || restored.Revision == updated.Revision || restored.Release.ID != source.Manifest.ID {
		t.Fatal(restored, err)
	}
	if err := store.Switch(t.Context(), seal, original.Revision, target); err == nil {
		t.Fatal("rollback reactivated obsolete source revision")
	}
	// A different writer repairs/changes only rg. Inspection atomically updates
	// the source revision, and the old R1 request still cannot overwrite it.
	if err := os.WriteFile(filepath.Join(store.root, source.Directory, "rg"), []byte("R3 external helper"), 0700); err != nil {
		t.Fatal(err)
	}
	external, err := store.Observe(t.Context())
	if err != nil || external.Revision == restored.Revision {
		t.Fatal(external, err)
	}
	var failure *api.Error
	if err := store.Switch(t.Context(), seal, restored.Revision, target); !errors.As(err, &failure) || failure.Code != "INSTALLATION_CHANGED" {
		t.Fatal("stale helper source accepted", err)
	}
	repeat, err := store.Observe(t.Context())
	if err != nil || repeat.Revision != external.Revision {
		t.Fatal("plain read changed revision", repeat, err)
	}
}

func TestSwitchRecoveryUsesOriginalIntentAtEveryBoundary(t *testing.T) {
	for _, point := range []string{"intent", "switched", "recorded"} {
		t.Run(point, func(t *testing.T) {
			store, source, seal := installationFixture(t)
			target := location(t, store.root, "R2")
			original, err := store.Observe(t.Context())
			if err != nil {
				t.Fatal(err)
			}
			store.barrier = func(stage string) error {
				if stage == point {
					return errors.New("worker interrupted")
				}
				return nil
			}
			if err := store.Switch(t.Context(), seal, original.Revision, target); err == nil {
				t.Fatal("barrier did not interrupt")
			}
			store.Close()
			recovery, err := Lock(store.root)
			if err != nil {
				t.Fatal(err)
			}
			defer recovery.Close()
			if err := recovery.RecoverSwitch(t.Context(), seal); err != nil {
				t.Fatal(err)
			}
			observed, err := recovery.Observe(t.Context())
			if err != nil {
				t.Fatal(err)
			}
			want := target.Manifest.ID
			if point == "intent" {
				want = source.Manifest.ID
			}
			if observed.Release.ID != want {
				t.Fatal("recovery replayed a switch", observed)
			}
			if point != "intent" && observed.Revision == original.Revision {
				t.Fatal("physical switch did not invalidate old revision")
			}
			record, err := recovery.Read()
			if err != nil || record.Pending != nil {
				t.Fatal(record, err)
			}
			body, err := os.ReadFile(record.Metadata.ConfigPath)
			if err != nil || string(body) != "must retain original configuration bytes\n" {
				t.Fatal("configuration changed", err)
			}
			if _, err := launchgate.Acquire(record.Metadata.StateDir, false); !errors.Is(err, launchgate.ErrBusy) {
				t.Fatal("reconciliation removed persistent seal", err)
			}
		})
	}
}

func TestSwitchRequiresCurrentRecoveryOwnerAndInstallationLock(t *testing.T) {
	store, _, seal := installationFixture(t)
	if other, err := Lock(store.root); !errors.Is(err, ErrBusy) {
		if other != nil {
			other.Close()
		}
		t.Fatal("two installation writers", err)
	}
	source, err := store.Observe(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	target := location(t, store.root, "R2")
	stale := seal
	stale.Owner = "stale-worker"
	if err := store.Switch(t.Context(), stale, source.Revision, target); err == nil {
		t.Fatal("stale seal owner switched installation")
	}
	after, err := store.Observe(t.Context())
	if err != nil || after.Revision != source.Revision {
		t.Fatal("rejected owner changed installation", after, err)
	}
}
