package launchgate

import (
	"bufio"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
)

var originalSeal = Seal{InstallationID: "installation", OperationID: "upgrade", Owner: "worker-1"}

func TestSealWorker(t *testing.T) {
	dir := os.Getenv("DUNE_TEST_SEAL_DIRECTORY")
	if dir == "" {
		t.Skip("child process")
	}
	gate, err := Acquire(dir, true)
	if err != nil {
		t.Fatal(err)
	}
	defer gate.Close()
	if err := gate.ClaimSeal(nil, originalSeal); err != nil {
		t.Fatal(err)
	}
	fmt.Println("sealed")
	_, _ = bufio.NewReader(os.Stdin).ReadString('\n')
}

func TestSealSurvivesWorkerDeathAndFencesRecovery(t *testing.T) {
	dir := t.TempDir()
	if err := os.Chmod(dir, 0700); err != nil {
		t.Fatal(err)
	}
	worker := exec.Command(os.Args[0], "-test.run=^TestSealWorker$")
	worker.Env = append(os.Environ(), "DUNE_TEST_SEAL_DIRECTORY="+dir)
	stdin, err := worker.StdinPipe()
	if err != nil {
		t.Fatal(err)
	}
	defer stdin.Close()
	stdout, err := worker.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	worker.Stderr = os.Stderr
	if err := worker.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = worker.Process.Kill(); _ = worker.Wait() })
	line, err := bufio.NewReader(stdout).ReadString('\n')
	if err != nil || line != "sealed\n" {
		t.Fatalf("worker did not seal: %q %v", line, err)
	}
	if err := worker.Process.Kill(); err != nil {
		t.Fatal(err)
	}
	_ = worker.Wait()
	// Intentionally leave recovery idle. The OS lock is gone, but launches must
	// remain refused for the entire gap, including another open/close cycle.
	for range 2 {
		if gate, err := Acquire(dir, false); !errors.Is(err, ErrBusy) {
			if gate != nil {
				gate.Close()
			}
			t.Fatalf("dead worker reopened admission: %v", err)
		}
	}
	recovery, err := Acquire(dir, true)
	if err != nil {
		t.Fatal(err)
	}
	next := originalSeal
	next.Owner = "worker-2"
	if err := recovery.ClaimSeal(&originalSeal, next); err != nil {
		t.Fatal(err)
	}
	if err := recovery.ClearSeal(originalSeal); !errors.Is(err, ErrOwnerChanged) {
		t.Fatal("stale owner cleared seal", err)
	}
	if err := recovery.Close(); err != nil {
		t.Fatal(err)
	}
	if err := recovery.ClearSeal(next); !errors.Is(err, ErrOwnerChanged) {
		t.Fatal("closed owner cleared seal", err)
	}
	current, err := Acquire(dir, true)
	if err != nil {
		t.Fatal(err)
	}
	defer current.Close()
	if err := current.ClearSeal(next); err != nil {
		t.Fatal(err)
	}
	current.Close()
	launch, err := Acquire(dir, false)
	if err != nil {
		t.Fatal(err)
	}
	launch.Close()
}

func TestInvalidSealNeverOpensAdmission(t *testing.T) {
	for _, kind := range []string{"truncated", "symlink", "permissions", "unknown_contract"} {
		t.Run(kind, func(t *testing.T) {
			dir := t.TempDir()
			if err := os.Chmod(dir, 0700); err != nil {
				t.Fatal(err)
			}
			path := filepath.Join(dir, sealName)
			var err error
			switch kind {
			case "symlink":
				err = os.Symlink("missing", path)
			case "permissions":
				err = os.WriteFile(path, []byte(`{}`), 0644)
			case "unknown_contract":
				err = os.WriteFile(path, []byte(`{"installation_id":"installation","operation_id":"upgrade","owner":"worker-1","new_semantics":true}`), 0600)
			default:
				err = os.WriteFile(path, []byte(`{"installation_id":`), 0600)
			}
			if err != nil {
				t.Fatal(err)
			}
			gate, err := Acquire(dir, false)
			if err == nil {
				gate.Close()
				t.Fatal("invalid seal admitted launch")
			}
		})
	}
}
