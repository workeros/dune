package tests

import (
	"context"
	"encoding/json"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/aiomni/dune/internal/authorization"
	"github.com/aiomni/dune/internal/config"
	"github.com/aiomni/dune/internal/identity"
	"github.com/aiomni/dune/internal/metadata"
	"github.com/aiomni/dune/internal/tmux"
	"github.com/aiomni/dune/pkg/host"
	"github.com/aiomni/dune/pkg/sdk"
	"github.com/aiomni/dune/pkg/storage"
)

func TestWebShutdownPreservesPTY(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 45*time.Second)
	defer cancel()
	dir := t.TempDir()
	database := storage.Config{SQLiteDir: filepath.Join(dir, "accounts")}
	store, err := metadata.Open(ctx, database)
	must(t, err)
	local := identity.NewLocal(store, true)
	user, cookie, err := local.Register(ctx, "shutdown@example.test", "shutdown-process-password")
	must(t, err)
	enrollment, _, err := store.IssueEnrollment(ctx, user.ID, "shutdown machine")
	must(t, err)
	machine, credential, err := store.Enroll(ctx, enrollment, "linux", "amd64")
	must(t, err)
	must(t, store.Close())
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	must(t, err)
	address := listener.Addr().String()
	must(t, listener.Close())
	site, endpoint := "http://"+address+"/dune/", "ws://"+address+"/dune/tunnel"
	serverFile, machineFile := filepath.Join(dir, "server.yaml"), filepath.Join(dir, "machine.yaml")
	must(t, config.Create(serverFile, config.Config{Gateway: endpoint, Listen: address, Token: strings.Repeat("x", 32), Target: "unused"}))
	sessionDir := filepath.Join(dir, "sessions")
	must(t, config.Create(machineFile, config.Config{Gateway: endpoint, Token: credential, Target: machine.ID, SessionDir: sessionDir}))
	log, err := os.Create(filepath.Join(dir, "services.log"))
	must(t, err)
	defer log.Close()
	t.Cleanup(func() {
		data, _ := os.ReadFile(log.Name())
		if strings.Contains(string(data), "DATA RACE") {
			t.Error("process reported a data race")
		}
		if t.Failed() {
			t.Log(string(data))
		}
	})
	fabric := launchHostTestProcess(t, log, "--config", machineFile, "fabricd")
	defer func() {
		fabric.stop(t, syscall.SIGTERM)
		manager, err := tmux.Open(sessionDir)
		if err == nil {
			manager.Close()
		}
	}()
	probe := &http.Client{Timeout: time.Second}
	start := func(timeout string) (*hostTestProcess, *sdk.Client) {
		t.Helper()
		// Prepare a short-lived human credential only while the SQLite host is
		// stopped; never bypass its exclusive metadata ownership.
		store, err := metadata.Open(ctx, database)
		must(t, err)
		local := identity.NewLocal(store, true)
		grant, err := authorization.NewLocal(ctx, local, store).Client(ctx, cookie, machine.ID)
		must(t, err)
		token := grant.Token()
		must(t, store.Close())
		process := launchHostTestProcess(t, log, "--config", serverFile, "web", "--url", site, "--data", database.SQLiteDir, "--drain-timeout", timeout)
		deadline := time.Now().Add(10 * time.Second)
		for {
			req, err := http.NewRequestWithContext(ctx, "GET", site+"api/machines", nil)
			must(t, err)
			req.AddCookie(&http.Cookie{Name: "dune_session", Value: cookie})
			response, err := probe.Do(req)
			if err == nil {
				var page struct{ Items []struct{ Online bool } }
				decodeErr := json.NewDecoder(response.Body).Decode(&page)
				response.Body.Close()
				if decodeErr == nil && response.StatusCode == 200 && len(page.Items) == 1 && page.Items[0].Online {
					break
				}
			}
			if time.Now().After(deadline) || ctx.Err() != nil {
				t.Fatal("fabric did not reconnect to Web host")
			}
			time.Sleep(25 * time.Millisecond)
		}
		client, err := sdk.Dial(ctx, sdk.Options{Gateway: endpoint, Token: token, Target: machine.ID})
		must(t, err)
		return process, client
	}
	original, client := start("2s")
	defer client.Close()
	runtime, stream, err := client.Start(ctx, profile(dir, "pty", "/bin/sh"))
	must(t, err)
	defer stream.Close()
	binding := client.Binding
	must(t, original.cmd.Process.Signal(syscall.SIGTERM))
	for {
		response, err := probe.Get(site + "health/ready")
		must(t, err)
		var status host.Readiness
		must(t, json.NewDecoder(response.Body).Decode(&status))
		response.Body.Close()
		if status.Draining {
			if response.StatusCode != 503 || status.Accepting || !status.Serving {
				t.Fatal("incorrect signal readiness", status)
			}
			break
		}
		time.Sleep(5 * time.Millisecond)
	}
	_, err = stream.Input([]byte("printf 'DRAIN_ACCEPTED_OK\\n'\n"))
	must(t, err)
	receive(t, stream, "data", "DRAIN_ACCEPTED_OK")
	if _, err := client.Get(ctx, runtime); err == nil {
		t.Fatal("existing SDK connection admitted new work during drain")
	}
	// Keep the terminal stream open so the configured deadline must force it
	// closed. A normal exit proves the signal did not fall back to SIGKILL.
	select {
	case err := <-original.done:
		original.stopped = true
		must(t, err)
	case <-time.After(5 * time.Second):
		t.Fatal("Web host exceeded drain budget")
	}
	stream.Close()
	client.Close()
	replacement, fresh := start("2s")
	defer fresh.Close()
	if fresh.Binding.Incarnation != binding.Incarnation || fresh.Binding.Generation <= binding.Generation {
		t.Fatal("fabric engine did not survive host shutdown")
	}
	current, err := fresh.Get(ctx, runtime)
	must(t, err)
	if current.State != "running" || current.Incarnation != runtime.Incarnation {
		t.Fatal("shutdown destroyed PTY")
	}
	attached, err := fresh.Attach(ctx, runtime, false)
	must(t, err)
	defer attached.Close()
	_, err = attached.Input([]byte("printf 'DRAIN_RESTART_OK\\n'\n"))
	must(t, err)
	receive(t, attached, "data", "DRAIN_RESTART_OK")
	must(t, fresh.Stop(ctx, runtime))
	attached.Close()
	// An idle SDK and fabric control connection do not consume the two-second
	// drain budget (the race runtime itself adds one second on process exit).
	started := time.Now()
	must(t, replacement.cmd.Process.Signal(syscall.SIGTERM))
	select {
	case err := <-replacement.done:
		replacement.stopped = true
		must(t, err)
		if time.Since(started) >= 2*time.Second {
			t.Fatal("idle tunnel delayed shutdown")
		}
	case <-time.After(4 * time.Second):
		t.Fatal("idle Web host did not stop")
	}
}
