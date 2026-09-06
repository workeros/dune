package tests

import (
	"context"
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
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

type hostTestProcess struct {
	cmd     *exec.Cmd
	done    chan error
	stopped bool
}

func launchHostTestProcess(t *testing.T, log *os.File, args ...string) *hostTestProcess {
	t.Helper()
	p := &hostTestProcess{cmd: exec.Command(binary, args...), done: make(chan error, 1)}
	p.cmd.Stdout, p.cmd.Stderr = log, log
	must(t, p.cmd.Start())
	go func() { p.done <- p.cmd.Wait() }()
	t.Cleanup(func() { p.stop(t, syscall.SIGTERM) })
	return p
}

func (p *hostTestProcess) stop(t *testing.T, sig os.Signal) {
	t.Helper()
	if p.stopped {
		return
	}
	p.stopped = true
	_ = p.cmd.Process.Signal(sig)
	select {
	case <-p.done:
	case <-time.After(8 * time.Second):
		_ = p.cmd.Process.Kill()
		<-p.done
		t.Error("test subprocess did not stop")
	}
}

func TestTmuxSurvivesFabricdAndGateway(t *testing.T) {
	// Short path also exercises the platform's actual AF_UNIX path limit.
	dir, err := os.MkdirTemp("", "dune-tmux-")
	must(t, err)
	t.Cleanup(func() { os.RemoveAll(dir) })
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	must(t, err)
	address := ln.Addr().String()
	ln.Close()
	path := filepath.Join(dir, "config.yaml")
	must(t, config.Init(path, address))
	c, err := config.Load(path)
	must(t, err)
	c.SessionDir = filepath.Join(dir, "sessions")
	b, err := yaml.Marshal(c)
	must(t, err)
	must(t, os.WriteFile(path, b, 0600))
	logFile, err := os.Create(filepath.Join(dir, "service.log"))
	must(t, err)
	t.Cleanup(func() {
		logFile.Close()
		b, _ := os.ReadFile(logFile.Name())
		if strings.Contains(string(b), "DATA RACE") {
			t.Error("host or connector data race")
		}
		if t.Failed() {
			t.Log(string(b))
		}
	})
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	gw := launchHostTestProcess(t, logFile, "--config", path, "gateway")
	connector := launchHostTestProcess(t, logFile, "--config", path, "fabricd")
	t.Cleanup(func() {
		connector.stop(t, syscall.SIGTERM)
		server, err := tmux.Open(c.SessionDir)
		must(t, err)
		must(t, server.Close())
	})
	dial := func(t *testing.T) *sdk.Client {
		t.Helper()
		deadline := time.Now().Add(10 * time.Second)
		for time.Now().Before(deadline) {
			client, err := sdk.Dial(ctx, sdk.Options{Gateway: c.Gateway, Token: c.Token, Target: c.Target})
			if err == nil {
				return client
			}
			time.Sleep(50 * time.Millisecond)
		}
		t.Fatal("persistent host did not register")
		return nil
	}
	client := dial(t)
	defer func() { client.Close() }()
	inc := client.Binding.Incarnation
	gen := client.Binding.Generation
	rt, stream, err := client.Start(ctx, profile(dir, "pty", "/bin/sh", "-c", `printf '%s\n' "$$" >> starts; while IFS= read -r line; do case "$line" in history) sleep 0.2; i=0; while [ "$i" -lt 80 ]; do printf 'offline-%03d\n' "$i"; i=$((i+1)); done;; *) printf 'reply:%s:%s\n' "$$" "$line";; esac; done`))
	must(t, err)
	_, err = stream.Input([]byte("before\n"))
	must(t, err)
	receive(t, stream, "data", ":before")
	starts, err := os.ReadFile(filepath.Join(dir, "starts"))
	must(t, err)
	pid := strings.TrimSpace(string(starts))

	duplicate := exec.CommandContext(ctx, binary, "--config", path, "fabricd")
	if out, err := duplicate.CombinedOutput(); err == nil || !strings.Contains(string(out), "already running") {
		t.Fatalf("duplicate fabricd should fail: %s %v", out, err)
	}

	parentTest := t
	for _, restart := range []string{"browser", "connector-kill", "gateway", "connector-graceful"} {
		t.Run(restart, func(t *testing.T) {
			if restart == "browser" {
				_, err := stream.Input([]byte("history\n"))
				must(t, err)
				receive(t, stream, "written", "")
			}
			stream.Close()
			client.Close()
			if restart == "browser" {
				time.Sleep(350 * time.Millisecond)
			}
			switch restart {
			case "connector-kill", "connector-graceful":
				sig := syscall.SIGKILL
				if restart == "connector-graceful" {
					sig = syscall.SIGTERM
				}
				connector.stop(t, sig)
				connector = launchHostTestProcess(parentTest, logFile, "--config", path, "fabricd")
			case "gateway":
				gw.stop(t, syscall.SIGTERM)
				gw = launchHostTestProcess(parentTest, logFile, "--config", path, "gateway")
			}
			client = dial(t)
			if strings.HasPrefix(restart, "connector-") {
				if client.Binding.Incarnation == inc {
					t.Fatal("fabricd identity did not change")
				}
			} else if client.Binding.Incarnation != inc {
				t.Fatal("unexpected fabricd identity change")
			}
			if restart == "gateway" && client.Binding.Generation <= gen {
				t.Fatal("gateway lease did not advance")
			}
			inc = client.Binding.Incarnation
			gen = client.Binding.Generation
			list, err := client.List(ctx)
			must(t, err)
			if len(list) != 1 || list[0].ID != rt.ID || list[0].Incarnation != rt.Incarnation || list[0].State != "running" {
				t.Fatalf("lost original runtime: %+v", list)
			}
			var screen tmux.Capture
			err = client.CallID(ctx, "runtime.capture", wire.ID(), struct{}{}, &screen, &rt)
			must(t, err)
			if screen.HistoryLimit != 50000 || screen.HistoryLines < 50 || !strings.Contains(screen.Content, "offline-079") {
				t.Fatalf("native screen/history lost: %+v", screen)
			}
			must(t, client.CallID(ctx, "runtime.history", wire.ID(), map[string]string{"action": "older"}, nil, &rt))
			must(t, client.CallID(ctx, "runtime.history", wire.ID(), map[string]string{"action": "close"}, nil, &rt))
			view, err := client.Attach(ctx, rt, true)
			must(t, err)
			receive(t, view, "data", "offline-079")
			view.Close()
			deadline := time.Now().Add(5 * time.Second)
			for {
				stream, err = client.Attach(ctx, rt, false)
				if err == nil {
					break
				}
				if time.Now().After(deadline) {
					t.Fatal(err)
				}
				time.Sleep(25 * time.Millisecond)
			}
			_, err = stream.Input([]byte(restart + "\n"))
			must(t, err)
			receive(t, stream, "data", fmt.Sprintf("reply:%s:%s", pid, restart))
			stream.Close()
			current, err := os.ReadFile(filepath.Join(dir, "starts"))
			must(t, err)
			if string(current) != string(starts) {
				t.Fatal("start command was executed again")
			}
		})
		if t.Failed() {
			return
		}
	}
	must(t, client.Stop(ctx, rt))
}
