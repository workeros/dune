package service

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/aiomni/dune/internal/wire"
)

func TestServiceDefinitionsKeepRecoveryIndependent(t *testing.T) {
	def := definition{name: "dune-upgrade", program: "/private space/recovery/dune", args: []string{"--config", "/private & config.yaml", "upgrade-worker", "--root", "/private space"}, description: "Dune recovery"}
	unit := systemdDefinition(def, "/usr/bin:/bin")
	plist := launchdDefinition(def, "/usr/bin:/bin")
	for _, unwanted := range []string{"PartOf=", "BindsTo=", "KillMode=control-group", "/current/dune"} {
		if strings.Contains(unit, unwanted) {
			t.Fatal("worker shares connector lifetime", unwanted)
		}
	}
	if !strings.Contains(unit, "KillMode=process") || !strings.Contains(unit, "Restart=always") || !strings.Contains(plist, "<key>AbandonProcessGroup</key><true/>") || !strings.Contains(plist, "private &amp; config.yaml") {
		t.Fatal("invalid service lifetime or argument escaping")
	}
}

type serviceHeartbeat struct {
	PID int
	At  time.Time
}

// Invoked only by the native manager test as the actual managed child process.
func TestServiceFixtureChild(t *testing.T) {
	args := flag.Args()
	if len(args) != 1 {
		return
	}
	for {
		data, _ := json.Marshal(serviceHeartbeat{PID: os.Getpid(), At: time.Now().UTC()})
		if err := os.WriteFile(args[0], data, 0600); err != nil {
			os.Exit(2)
		}
		time.Sleep(30 * time.Millisecond)
	}
}

func TestNativeServiceManagerKeepsWorkerAcrossConnectorRestart(t *testing.T) {
	if os.Getenv("DUNE_TEST_SERVICE_MANAGER") != "1" {
		t.Skip("set DUNE_TEST_SERVICE_MANAGER=1 for isolated per-user service jobs")
	}
	ctx, cancel := context.WithTimeout(t.Context(), time.Minute)
	defer cancel()
	if err := CheckManager(ctx); err != nil {
		t.Skip("native user service manager is unavailable")
	}
	home, err := os.UserHomeDir()
	if err != nil {
		t.Fatal(err)
	}
	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	root := t.TempDir()
	name := "dune-test-" + wire.ID()
	connectorPath, workerPath := filepath.Join(root, "connector"), filepath.Join(root, "worker")
	definitions := []definition{
		{name: name, program: executable, args: []string{"-test.run=^TestServiceFixtureChild$", "--", connectorPath}, description: "Dune isolated connector lifecycle test"},
		{name: name + "-upgrade", program: executable, args: []string{"-test.run=^TestServiceFixtureChild$", "--", workerPath}, description: "Dune isolated worker lifecycle test"},
	}
	for _, def := range definitions {
		t.Cleanup(func() {
			cleanup, stop := context.WithTimeout(context.Background(), 10*time.Second)
			defer stop()
			if runtime.GOOS == "darwin" {
				_ = exec.CommandContext(cleanup, "launchctl", "bootout", domain()+"/com.dune."+def.name).Run()
				_ = os.Remove(filepath.Join(home, "Library/LaunchAgents", "com.dune."+def.name+".plist"))
			} else {
				_ = exec.CommandContext(cleanup, "systemctl", "--user", "disable", "--now", def.name+".service").Run()
				_ = os.Remove(filepath.Join(home, ".config/systemd/user", def.name+".service"))
				_ = exec.CommandContext(cleanup, "systemctl", "--user", "daemon-reload").Run()
			}
		})
		if err := installDefinition(ctx, home, def, os.Getenv("PATH")); err != nil {
			t.Fatal(err)
		}
	}
	firstConnector := waitHeartbeat(t, ctx, connectorPath, 0)
	firstWorker := waitHeartbeat(t, ctx, workerPath, 0)
	for _, def := range definitions {
		if err := installDefinition(ctx, home, def, os.Getenv("PATH")); err != nil {
			t.Fatal(err)
		}
	}
	if repaired := waitHeartbeat(t, ctx, workerPath, 0); repaired.PID != firstWorker.PID {
		t.Fatal("service repair restarted existing worker")
	}
	if err := Run(ctx, "restart", name); err != nil {
		t.Fatal(err)
	}
	secondConnector := waitHeartbeat(t, ctx, connectorPath, firstConnector.PID)
	worker := waitHeartbeat(t, ctx, workerPath, 0)
	if secondConnector.PID == firstConnector.PID || worker.PID != firstWorker.PID {
		t.Fatal("connector restart killed independent worker", secondConnector, worker)
	}
	if err := syscall.Kill(firstWorker.PID, syscall.SIGKILL); err != nil {
		t.Fatal(err)
	}
	recovered := waitHeartbeat(t, ctx, workerPath, firstWorker.PID)
	if recovered.PID == firstWorker.PID {
		t.Fatal("worker was not restarted by service manager")
	}
	t.Logf("native %s/%s manager: connector %d→%d; worker survived as %d, recovered after SIGKILL as %d", runtime.GOOS, runtime.GOARCH, firstConnector.PID, secondConnector.PID, firstWorker.PID, recovered.PID)
}
func waitHeartbeat(t *testing.T, ctx context.Context, path string, previous int) serviceHeartbeat {
	t.Helper()
	for {
		var heartbeat serviceHeartbeat
		data, err := os.ReadFile(path)
		if err == nil && json.Unmarshal(data, &heartbeat) == nil && heartbeat.PID > 1 && heartbeat.PID != previous && time.Since(heartbeat.At) < time.Second {
			return heartbeat
		}
		select {
		case <-ctx.Done():
			t.Fatal(fmt.Sprintf("service heartbeat did not advance: %s", filepath.Base(path)))
		case <-time.After(50 * time.Millisecond):
		}
	}
}
