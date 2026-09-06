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

	"github.com/aiomni/dune/internal/config"
	"github.com/aiomni/dune/internal/tmux"
	"github.com/aiomni/dune/internal/wire"
	"github.com/aiomni/dune/pkg/sdk"
	"gopkg.in/yaml.v3"
)

func TestInputLeaseSurvivesProcessPause(t *testing.T) {
	dir, err := os.MkdirTemp("", "dune-lease-")
	must(t, err)
	t.Cleanup(func() { os.RemoveAll(dir) })
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	must(t, err)
	address := listener.Addr().String()
	listener.Close()
	path := filepath.Join(dir, "config.yaml")
	must(t, config.Init(path, address))
	cfg, err := config.Load(path)
	must(t, err)
	cfg.SessionDir = filepath.Join(dir, "sessions")
	data, err := yaml.Marshal(cfg)
	must(t, err)
	must(t, os.WriteFile(path, data, 0600))
	log, err := os.Create(filepath.Join(dir, "service.log"))
	must(t, err)
	t.Cleanup(func() {
		log.Close()
		data, _ := os.ReadFile(log.Name())
		if strings.Contains(string(data), "DATA RACE") {
			t.Error("service data race")
		}
		if t.Failed() {
			t.Log(string(data))
		}
	})
	ctx, cancel := context.WithTimeout(context.Background(), 75*time.Second)
	defer cancel()
	gateway := launchHostTestProcess(t, log, "--config", path, "gateway")
	fabric := launchHostTestProcess(t, log, "--config", path, "fabricd")
	t.Cleanup(func() {
		// Resume even on an assertion failure before asking a process to exit.
		_ = gateway.cmd.Process.Signal(syscall.SIGCONT)
		_ = fabric.cmd.Process.Signal(syscall.SIGCONT)
		fabric.stop(t, syscall.SIGTERM)
		server, err := tmux.Open(cfg.SessionDir)
		must(t, err)
		must(t, server.Close())
	})
	dial := func() *sdk.Client {
		t.Helper()
		deadline := time.Now().Add(10 * time.Second)
		for time.Now().Before(deadline) {
			client, err := sdk.Dial(ctx, sdk.Options{Gateway: cfg.Gateway, Target: cfg.Target, Token: cfg.Token})
			if err == nil {
				return client
			}
			time.Sleep(25 * time.Millisecond)
		}
		t.Fatal("fabricd did not register after pause")
		return nil
	}
	client := dial()
	defer func() { client.Close() }()
	runtime, stream, err := client.Start(ctx, profile(dir, "pty", "/bin/sh"))
	must(t, err)
	defer func() { stream.Close() }()
	_, err = stream.Input([]byte("printf LEASE_INITIAL_OK\\n\n"))
	must(t, err)
	receive(t, stream, "data", "LEASE_INITIAL_OK")
	for _, phase := range []struct {
		name    string
		process *hostTestProcess
	}{{"fabricd", fabric}, {"gateway", gateway}} {
		t.Run(phase.name, func(t *testing.T) {
			original := client.Binding
			must(t, phase.process.cmd.Process.Signal(syscall.SIGSTOP))
			defer phase.process.cmd.Process.Signal(syscall.SIGCONT)
			pauseDeadline := time.Now().Add(time.Second)
			for {
				state, err := exec.Command("ps", "-o", "state=", "-p", strconv.Itoa(phase.process.cmd.Process.Pid)).Output()
				must(t, err)
				if strings.Contains(string(state), "T") {
					break
				}
				if time.Now().After(pauseDeadline) {
					t.Fatal("process did not enter stopped state")
				}
				time.Sleep(10 * time.Millisecond)
			}
			marker := "expired-" + phase.name
			_, err := stream.Input([]byte("touch " + marker + "\n"))
			must(t, err)
			// The real process cannot run lease timers or drain buffered input.
			// The peer must expire the tunnel; resuming cannot make old bytes valid.
			select {
			case <-ctx.Done():
				t.Fatal(ctx.Err())
			case <-time.After(wire.InputLeaseDuration + time.Second):
			}
			must(t, phase.process.cmd.Process.Signal(syscall.SIGCONT))
			stream.Close()
			client.Close()
			client = dial()
			if client.Binding.Generation <= original.Generation || client.Binding.Incarnation != original.Incarnation {
				t.Fatal("pause recovery did not reconnect the original engine")
			}
			current, err := client.Get(ctx, runtime)
			must(t, err)
			if current.State != "running" || current.Incarnation != runtime.Incarnation {
				t.Fatal("lease expiration destroyed original runtime", current)
			}
			deadline := time.Now().Add(5 * time.Second)
			for {
				stream, err = client.Attach(ctx, runtime, false)
				if err == nil {
					break
				}
				if time.Now().After(deadline) {
					t.Fatal(err)
				}
				time.Sleep(25 * time.Millisecond)
			}
			_, err = stream.Input([]byte("printf 'LEASE_%s_RESUMED_OK\\n' " + phase.name + "\n"))
			must(t, err)
			receive(t, stream, "data", "LEASE_"+phase.name+"_RESUMED_OK")
			if _, err := os.Stat(filepath.Join(dir, marker)); !os.IsNotExist(err) {
				t.Fatal("buffered input executed after expired process resumed", err)
			}
		})
		if t.Failed() {
			return
		}
	}
	must(t, client.Stop(ctx, runtime))
}
