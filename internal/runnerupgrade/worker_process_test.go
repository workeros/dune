package runnerupgrade

import (
	"context"
	"encoding/json"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"

	"github.com/aiomni/dune/internal/installation"
	"github.com/aiomni/dune/internal/launchgate"
	"github.com/aiomni/dune/internal/upgradejob"
	"github.com/aiomni/dune/pkg/api"
	"github.com/aiomni/dune/pkg/upgrade"
)

type workerProcessFixture struct {
	Root, Template string
	Operation      upgrade.Operation
	Source         installation.Location
	Running        api.RunningProgram
	Started        *upgrade.Probe
	Restarts       int
}

func TestUpgradeWorkerProcessHelper(t *testing.T) {
	path := os.Getenv("DUNE_UPGRADE_WORKER_FIXTURE")
	if path == "" {
		t.Skip("upgrade worker crash child only")
	}
	body, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var fixture workerProcessFixture
	if err := json.Unmarshal(body, &fixture); err != nil {
		t.Fatal(err)
	}
	f := &workerFixture{t: t, source: fixture.Source, target: fixture.Operation.Target, template: fixture.Template, operation: fixture.Operation, binding: fixture.Operation.Request.Binding, running: fixture.Running}
	store, err := installation.Lock(fixture.Root)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	state, err := store.Read()
	if err != nil {
		t.Fatal(err)
	}
	jobs, err := upgradejob.Open(t.Context(), filepath.Join(fixture.Root, "upgrades"))
	if err != nil {
		t.Fatal(err)
	}
	defer jobs.Close()
	f.worker = &worker{root: fixture.Root, installed: store, jobs: jobs, metadata: state.Metadata, control: f, poll: time.Millisecond}
	configureWorkerFixture(f)
	f.worker.barrier = func(stage string) error {
		if stage != os.Getenv("DUNE_UPGRADE_WORKER_POINT") {
			return nil
		}
		connection, err := net.DialTimeout("tcp", os.Getenv("DUNE_UPGRADE_WORKER_GATE"), 5*time.Second)
		if err != nil {
			return err
		}
		defer connection.Close()
		fixture.Running, fixture.Started, fixture.Restarts = f.running, f.started, f.restarts
		if err := json.NewEncoder(connection).Encode(fixture); err != nil {
			return err
		}
		var resumed [1]byte
		_, err = connection.Read(resumed[:])
		return err
	}
	if err := f.worker.execute(t.Context(), f.operation.ID); err != nil {
		t.Fatal(err)
	}
	t.Fatal("worker reached terminal state without being killed at its checkpoint")
}

func TestWorkerSIGKILLAtDurableBoundaries(t *testing.T) {
	for _, point := range []string{"downloaded", "prepared", "switched", "restarted", "proof_received", "confirmed", "unseal"} {
		t.Run(point, func(t *testing.T) {
			f := newWorkerFixture(t)
			fixture := workerProcessFixture{Root: f.worker.root, Template: f.template, Operation: f.operation, Source: f.source, Running: f.running}
			body, err := json.Marshal(fixture)
			if err != nil {
				t.Fatal(err)
			}
			path := filepath.Join(t.TempDir(), "worker.json")
			if err := os.WriteFile(path, body, 0600); err != nil {
				t.Fatal(err)
			}
			f.worker.installed.Close()
			f.worker.jobs.Close()
			listener, err := net.Listen("tcp", "127.0.0.1:0")
			if err != nil {
				t.Fatal(err)
			}
			defer listener.Close()
			ctx, cancel := context.WithTimeout(t.Context(), 20*time.Second)
			defer cancel()
			child := exec.CommandContext(ctx, os.Args[0], "-test.run=^TestUpgradeWorkerProcessHelper$", "-test.timeout=20s")
			child.Env = append(os.Environ(), "DUNE_UPGRADE_WORKER_FIXTURE="+path, "DUNE_UPGRADE_WORKER_POINT="+point, "DUNE_UPGRADE_WORKER_GATE="+listener.Addr().String())
			child.Stderr = os.Stderr
			if err := child.Start(); err != nil {
				t.Fatal(err)
			}
			defer func() { _ = child.Process.Kill(); _ = child.Wait() }()
			_ = listener.(*net.TCPListener).SetDeadline(time.Now().Add(10 * time.Second))
			barrier, err := listener.Accept()
			if err != nil {
				t.Fatal(err)
			}
			defer barrier.Close()
			_ = barrier.SetDeadline(time.Now().Add(10 * time.Second))
			if err := json.NewDecoder(barrier).Decode(&fixture); err != nil {
				t.Fatal(err)
			}
			if err := child.Process.Kill(); err != nil {
				t.Fatal(err)
			}
			if err := child.Wait(); err == nil {
				t.Fatal("worker was not killed")
			}
			// No Go defers ran in the child. The OS released its locks; its seal
			// must nevertheless protect every checkpoint after preparation.
			f.worker.installed, err = installation.Lock(f.worker.root)
			if err != nil {
				t.Fatal("dead worker kept the file lock", err)
			}
			defer f.worker.installed.Close()
			gate, err := launchgate.Acquire(f.worker.metadata.StateDir, false)
			if point == "downloaded" {
				if err != nil {
					t.Fatal("download needlessly sealed launches", err)
				}
				gate.Close()
			} else if err == nil {
				gate.Close()
				t.Fatal("dead worker reopened launch admission")
			}
			f.worker.jobs, err = upgradejob.Open(t.Context(), filepath.Join(f.worker.root, "upgrades"))
			if err != nil {
				t.Fatal(err)
			}
			defer f.worker.jobs.Close()
			f.running, f.started, f.restarts = fixture.Running, fixture.Started, fixture.Restarts
			if err := f.worker.execute(t.Context(), f.operation.ID); err != nil {
				t.Fatal(err)
			}
			result := f.result()
			if !result.Confirmed || result.Phase != upgrade.Succeeded || f.restarts != 1 {
				t.Fatal("recovery replayed or lost effects", result, f.restarts)
			}
			f.assertUnsealed()
		})
	}
}
