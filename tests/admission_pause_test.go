package tests

import (
	"context"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/aiomni/dune/internal/authorization"
	"github.com/aiomni/dune/internal/config"
	"github.com/aiomni/dune/internal/identity"
	"github.com/aiomni/dune/internal/metadata"
	"github.com/aiomni/dune/internal/tmux"
	"github.com/aiomni/dune/internal/wire"
	"github.com/aiomni/dune/pkg/sdk"
	"gopkg.in/yaml.v3"
)

func TestPostgresAdmissionSurvivesProcessPause(t *testing.T) {
	database := postgresWorkbenchConfig(t)
	ctx, cancel := context.WithTimeout(context.Background(), 65*time.Second)
	defer cancel()
	store, err := metadata.Open(ctx, database)
	must(t, err)
	defer store.Close()
	local := identity.NewLocal(store, true)
	user, cookie, err := local.Register(ctx, "admission-pause@example.test", "admission-pause-password")
	must(t, err)
	enrollment, _, err := store.IssueEnrollment(ctx, user.ID, "pause machine")
	must(t, err)
	machine, credential, err := store.Enroll(ctx, enrollment, "linux", "amd64")
	must(t, err)
	authorizer := authorization.NewLocal(ctx, local, store)
	dir := t.TempDir()
	public, err := net.Listen("tcp", "127.0.0.1:0")
	must(t, err)
	address := public.Addr().String()
	must(t, public.Close())
	site := "http://" + address + "/dune/"
	endpoint := "ws://" + address + "/dune/tunnel"
	serverConfig := filepath.Join(dir, "server.yaml")
	must(t, config.Create(serverConfig, config.Config{Gateway: endpoint, Listen: address, Token: strings.Repeat("x", 32), Target: "unused-local-token"}))
	databaseFile := filepath.Join(dir, "database.yaml")
	data, err := yaml.Marshal(map[string]any{"postgres": map[string]string{"url": postgresWorkbenchURL(t, database)}})
	must(t, err)
	must(t, os.WriteFile(databaseFile, data, 0600))
	machineConfig := filepath.Join(dir, "machine.yaml")
	sessionDir := filepath.Join(dir, "sessions")
	must(t, config.Create(machineConfig, config.Config{Gateway: endpoint, Token: credential, Target: machine.ID, SessionDir: sessionDir}))
	log, err := os.Create(filepath.Join(dir, "services.log"))
	must(t, err)
	defer log.Close()
	args := []string{"--config", serverConfig, "web", "--url", site, "--database-config", databaseFile}
	original := launchHostTestProcess(t, log, args...)
	defer original.stop(t, syscall.SIGTERM)
	defer original.cmd.Process.Signal(syscall.SIGCONT)
	fabric := launchHostTestProcess(t, log, "--config", machineConfig, "fabricd")
	defer func() {
		fabric.stop(t, syscall.SIGTERM)
		manager, err := tmux.Open(sessionDir)
		if err == nil {
			manager.Close()
		}
	}()
	dial := func() *sdk.Client {
		t.Helper()
		deadline := time.Now().Add(12 * time.Second)
		for time.Now().Before(deadline) {
			grant, err := authorizer.Client(ctx, cookie, machine.ID)
			must(t, err)
			client, err := sdk.Dial(ctx, sdk.Options{Gateway: endpoint, Target: machine.ID, Token: grant.Token()})
			if err == nil {
				return client
			}
			select {
			case <-ctx.Done():
				t.Fatal(ctx.Err())
			case <-time.After(100 * time.Millisecond):
			}
		}
		t.Fatal("fabricd did not connect to the admitted host")
		return nil
	}
	client := dial()
	defer func() { client.Close() }()
	runtime, stream, err := client.Start(ctx, profile(dir, "pty", "/bin/sh"))
	must(t, err)
	defer func() { stream.Close() }()
	_, err = stream.Input([]byte("printf 'ADMISSION_INITIAL_OK\\n'\n"))
	must(t, err)
	receive(t, stream, "data", "ADMISSION_INITIAL_OK")
	binding := client.Binding
	must(t, original.cmd.Process.Signal(syscall.SIGSTOP))
	stopped := time.Now().Add(time.Second)
	for {
		state, err := exec.Command("ps", "-o", "state=", "-p", strconv.Itoa(original.cmd.Process.Pid)).Output()
		must(t, err)
		if strings.Contains(string(state), "T") {
			break
		}
		if time.Now().After(stopped) {
			t.Fatal("host did not pause")
		}
		time.Sleep(10 * time.Millisecond)
	}
	_, err = stream.Input([]byte("touch expired-admission-input\n"))
	must(t, err)
	select {
	case <-ctx.Done():
		t.Fatal(ctx.Err())
	case <-time.After(wire.InputLeaseDuration + time.Second):
	}
	must(t, original.cmd.Process.Signal(syscall.SIGCONT))
	select {
	case <-original.done:
		original.stopped = true
	case <-time.After(5 * time.Second):
		t.Fatal("expired host resumed serving after process pause")
	}
	stream.Close()
	client.Close()
	// A new boot with a changed configuration can start only after the old
	// database reservation expires; it never revives the previous boot's lease.
	replacement := launchHostTestProcess(t, log, append(args, "--configuration-version", "replacement-v2")...)
	defer replacement.stop(t, syscall.SIGTERM)
	client = dial()
	if client.Binding.Generation <= binding.Generation || client.Binding.Incarnation != binding.Incarnation {
		t.Fatal("replacement did not reconnect the original fabricd engine")
	}
	current, err := client.Get(ctx, runtime)
	must(t, err)
	if current.State != "running" || current.Incarnation != runtime.Incarnation {
		t.Fatal("admission expiry destroyed the running PTY")
	}
	stream, err = client.Attach(ctx, runtime, false)
	must(t, err)
	_, err = stream.Input([]byte("printf 'ADMISSION_REPLACEMENT_OK\\n'\n"))
	must(t, err)
	receive(t, stream, "data", "ADMISSION_REPLACEMENT_OK")
	if _, err := os.Stat(filepath.Join(dir, "expired-admission-input")); !os.IsNotExist(err) {
		t.Fatal("buffered input executed under the expired configuration", err)
	}
	must(t, client.Stop(ctx, runtime))
}
