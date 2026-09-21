package fabricd

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/aiomni/dune/internal/sessionregistry"
	"github.com/aiomni/dune/internal/wire"
	"github.com/aiomni/dune/pkg/api"
)

func TestCleanupKeepsInstallationLockUntilOldFilesystemActionDrains(t *testing.T) {
	state := filepath.Join(t.TempDir(), "state")
	entered, release := make(chan struct{}), make(chan struct{})
	defer func() {
		select {
		case <-release:
		default:
			close(release)
		}
	}()
	engine, err := openWithCleanupBarrier(context.Background(), state, func(_ api.SubmissionKey, point string) error {
		if point == "runtime_directories:quarantined" {
			close(entered)
			<-release
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(engine.Close)
	runtime := api.Runtime{ID: wire.ID(), Incarnation: wire.ID(), Generation: 1, Adapter: "acp", State: "exited"}
	key := cleanupTestKey(runtime, "cleanup-lock-test")
	directory := localCachePaths(state, runtime)[0]
	if err := os.MkdirAll(directory, 0700); err != nil {
		t.Fatal(err)
	}
	if err := savePrivateFile(filepath.Join(directory, "instance.json"), runtime.Incarnation); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(directory, "cache"), []byte("original cache"), 0600); err != nil {
		t.Fatal(err)
	}
	identity, err := fileIdentity(directory, os.ModeDir)
	if err != nil {
		t.Fatal(err)
	}
	admitTestRuntime(t, engine.registry, key.Target)
	_, receipt, err := engine.registry.AcceptLocalForget(t.Context(), key, "original-cleanup", func() (sessionregistry.LocalCleanup, error) {
		return sessionregistry.LocalCleanup{Runtime: runtime, Directories: []sessionregistry.FileIdentity{identity}}, nil
	})
	if err != nil {
		t.Fatal(err)
	}
	engine.scheduleCleanup(key, receipt.OperationRef)
	<-entered
	closed := make(chan struct{})
	go func() { engine.Close(); close(closed) }()
	<-engine.ctx.Done()
	if replacement, err := Open(t.Context(), state); err == nil {
		replacement.Close()
		t.Fatal("new executor acquired lock while old filesystem action was suspended")
	}
	select {
	case <-closed:
		t.Fatal("Close released a live cleanup executor")
	default:
	}
	close(release)
	<-closed
	replacement, err := Open(t.Context(), state)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(replacement.Close)
	waitTimeoutTest(t, func() bool {
		receipt, err := replacement.registry.Get(t.Context(), key)
		return err == nil && receipt.Stage == "completed"
	})
	if _, err := os.Lstat(directory); !os.IsNotExist(err) {
		t.Fatal("replacement did not finish the original plan", err)
	}
}
