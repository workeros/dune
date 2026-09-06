package fabricd

import (
	"context"
	"errors"
	"net"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/aiomni/dune/internal/wire"
	"github.com/aiomni/dune/pkg/access"
	"github.com/aiomni/dune/pkg/api"
	"github.com/aiomni/dune/pkg/client"
	"github.com/aiomni/dune/pkg/gateway"
	pb "github.com/aiomni/dune/proto/dune/dtp/v1"
	"github.com/hashicorp/yamux"
)

func TestReplacementConnectionRejectsOldStreamInput(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	engine, err := Open(ctx, filepath.Join(t.TempDir(), "fabricd"))
	if err != nil {
		t.Fatal(err)
	}
	var wg sync.WaitGroup
	var gateways []*gateway.Gateway
	t.Cleanup(func() {
		cancel()
		for _, g := range gateways {
			g.Close()
		}
		engine.Close()
		wg.Wait()
		engine.tmux.Close()
	})
	connect := func() *client.Client {
		t.Helper()
		g := gateway.New()
		gateways = append(gateways, g)
		left, right := net.Pipe()
		binding, handler, err := (access.Grant{Target: "machine", Role: gateway.RoleDaemon}).Bind()
		if err != nil {
			t.Fatal(err)
		}
		wg.Go(func() { _ = g.ServeConn(ctx, left, binding, handler) })
		wg.Go(func() { _ = engine.ServeConn(ctx, right, "machine") })
		for !g.Online("machine") {
			select {
			case <-ctx.Done():
				t.Fatal("fabricd did not connect")
			case <-time.After(time.Millisecond):
			}
		}
		left, right = net.Pipe()
		binding, handler, err = (access.Grant{Target: "machine", Role: gateway.RoleSDK}).Bind()
		if err != nil {
			t.Fatal(err)
		}
		wg.Go(func() { _ = g.ServeConn(ctx, left, binding, handler) })
		c, err := client.Connect(ctx, right, "machine")
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { c.Close() })
		return c
	}
	original := connect()
	dir := t.TempDir()
	terminal, pty, err := original.Start(ctx, api.Profile{Version: 1, Kind: "agent", WorkingDirectory: dir, Adapter: "pty", Start: api.Command{Argv: []string{"/bin/sh"}}})
	if err != nil {
		t.Fatal(err)
	}
	defer pty.Close()
	agent, acp, err := original.Start(ctx, api.Profile{Version: 1, Kind: "agent", WorkingDirectory: dir, Adapter: "acp", Start: api.Command{Argv: []string{"/bin/cat"}}})
	if err != nil {
		t.Fatal(err)
	}
	defer acp.Close()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	port, err := original.Connect(ctx, listener.Addr().(*net.TCPAddr).Port)
	if err != nil {
		t.Fatal(err)
	}
	defer port.Close()
	server, err := listener.Accept()
	if err != nil {
		t.Fatal(err)
	}
	defer server.Close()
	// A real second reverse handshake advances the engine's connection generation.
	// Keep the first Gateway alive to exercise input already on an old stream.
	current := connect()
	if current.Binding.Generation <= original.Binding.Generation {
		t.Fatal("reverse connection did not advance generation")
	}
	forbidden := filepath.Join(dir, "old-connection-wrote")
	if _, err := pty.Input([]byte("touch " + forbidden + "\n")); err != nil {
		t.Fatal(err)
	}
	if _, err := acp.Input([]byte("{\"jsonrpc\":\"2.0\",\"method\":\"old-connection\"}\n")); err != nil {
		t.Fatal(err)
	}
	if _, err := port.Write([]byte("OLD_CONNECTION")); err != nil {
		t.Fatal(err)
	}
	for name, stream := range map[string]*client.Stream{"terminal": pty, "raw ACP": acp, "port": port.Stream} {
		for {
			_, err := stream.Recv()
			if err == nil {
				continue
			}
			var protocol *api.Error
			if !errors.As(err, &protocol) || protocol.Code != "STALE_BINDING" {
				t.Fatal(name, "old input not rejected at fabricd", err)
			}
			break
		}
	}
	if _, err := os.Stat(forbidden); !os.IsNotExist(err) {
		t.Fatal("old terminal input executed", err)
	}
	_ = server.SetReadDeadline(time.Now().Add(time.Second))
	var received [32]byte
	if n, err := server.Read(received[:]); n != 0 || err == nil {
		t.Fatal("old port bytes reached destination", n, err)
	}
	for _, runtime := range []api.Runtime{terminal, agent} {
		got, err := current.Get(ctx, runtime)
		if err != nil || got.State != "running" || got.Incarnation != runtime.Incarnation {
			t.Fatal("rejecting old connection destroyed current runtime", got, err)
		}
	}
	if result, err := current.Exec(ctx, api.Exec{Command: api.Command{Argv: []string{"/bin/sh", "-c", "printf CURRENT_CONNECTION_OK"}}, WorkingDirectory: dir}); err != nil || result.ExitCode != 0 || string(result.Stdout) != "CURRENT_CONNECTION_OK" {
		t.Fatal("new connection cannot execute", result, err)
	}
}

func TestCancelledConnectionRejectsBufferedMessage(t *testing.T) {
	engine := newEngine(context.Background())
	defer engine.Close()
	left, right := net.Pipe()
	sender, err := yamux.Client(left, wire.Config())
	if err != nil {
		t.Fatal(err)
	}
	defer sender.Close()
	receiver, err := yamux.Server(right, wire.Config())
	if err != nil {
		t.Fatal(err)
	}
	defer receiver.Close()
	raw, err := sender.OpenStream()
	if err != nil {
		t.Fatal(err)
	}
	sent := wire.Wrap(raw)
	inbound, err := receiver.AcceptStream()
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	input := wire.NewInputWindow()
	id := wire.ID()
	if err := input.Begin(id); err != nil {
		t.Fatal(err)
	}
	if err := input.Confirm(id, wire.InputLeaseDuration); err != nil {
		t.Fatal(err)
	}
	s := &executionStream{Stream: wire.Wrap(inbound), ctx: ctx, engine: engine, input: input}
	// Yamux accepts bytes before the application reads them. Do not close the
	// stream here: cancellation must reject buffered content at admission itself.
	if err := sent.Send(&pb.Message{Kind: "input", RequestId: "buffered", InputLeaseId: id, Data: []byte("must not execute")}); err != nil {
		t.Fatal(err)
	}
	cancel()
	_ = s.SetDeadline(time.Now().Add(time.Second))
	if message, err := s.Recv(); message != nil || err == nil {
		t.Fatal("cancelled stream admitted buffered input", message, err)
	}
	_ = sent.SetReadDeadline(time.Now().Add(time.Second))
	failure, err := sent.Recv()
	if err != nil || failure.Code != "STALE_BINDING" {
		t.Fatal("missing explicit stale connection response", failure, err)
	}
}

func TestExpiredLeaseRejectsBufferedMessage(t *testing.T) {
	engine := newEngine(context.Background())
	defer engine.Close()
	left, right := net.Pipe()
	sender, err := yamux.Client(left, wire.Config())
	if err != nil {
		t.Fatal(err)
	}
	defer sender.Close()
	receiver, err := yamux.Server(right, wire.Config())
	if err != nil {
		t.Fatal(err)
	}
	defer receiver.Close()
	raw, err := sender.OpenStream()
	if err != nil {
		t.Fatal(err)
	}
	sent := wire.Wrap(raw)
	inbound, err := receiver.AcceptStream()
	if err != nil {
		t.Fatal(err)
	}
	input := wire.NewInputWindow()
	id := wire.ID()
	if err := input.Begin(id); err != nil {
		t.Fatal(err)
	}
	if err := input.Confirm(id, 30*time.Millisecond); err != nil {
		t.Fatal(err)
	}
	s := &executionStream{Stream: wire.Wrap(inbound), ctx: context.Background(), engine: engine, input: input}
	if err := sent.Send(&pb.Message{Kind: "input", InputLeaseId: id, Data: []byte("expired buffered input")}); err != nil {
		t.Fatal(err)
	}
	// Keep the connection, engine and generation valid. Only the original grant
	// expires; there is deliberately no Watch goroutine to mask admission bugs.
	time.Sleep(40 * time.Millisecond)
	if message, err := s.Recv(); message != nil || err == nil {
		t.Fatal("expired lease admitted buffered message", message, err)
	}
	_ = sent.SetReadDeadline(time.Now().Add(time.Second))
	failure, err := sent.Recv()
	if err != nil || failure.Code != "STALE_BINDING" {
		t.Fatal("missing stale input lease response", failure, err)
	}
}

func TestReverseConnectionRenewsInputLease(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 25*time.Second)
	defer cancel()
	engine := newEngine(ctx)
	defer engine.Close()
	g := gateway.New()
	defer g.Close()
	var wg sync.WaitGroup
	defer wg.Wait()
	// Cleanup ordering must cancel blocked control reads before waiting.
	defer cancel()
	left, right := net.Pipe()
	binding, handler, err := (access.Grant{Target: "machine", Role: gateway.RoleDaemon}).Bind()
	if err != nil {
		t.Fatal(err)
	}
	wg.Go(func() { _ = g.ServeConn(ctx, left, binding, handler) })
	wg.Go(func() { _ = engine.ServeConn(ctx, right, "machine") })
	for !g.Online("machine") {
		select {
		case <-ctx.Done():
			t.Fatal("fabricd did not connect")
		case <-time.After(time.Millisecond):
		}
	}
	left, right = net.Pipe()
	binding, handler, err = (access.Grant{Target: "machine", Role: gateway.RoleSDK}).Bind()
	if err != nil {
		t.Fatal(err)
	}
	wg.Go(func() { _ = g.ServeConn(ctx, left, binding, handler) })
	c, err := client.Connect(ctx, right, "machine")
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	original := c.Binding
	requestID := wire.ID()
	write := api.File{Action: "write", Path: filepath.Join(t.TempDir(), "deduplicated"), Data: []byte("one write")}
	if err := c.CallID(ctx, "files", requestID, write, nil, nil); err != nil {
		t.Fatal(err)
	}
	// More than the first grant's lifetime, including three control renewals.
	select {
	case <-ctx.Done():
		t.Fatal(ctx.Err())
	case <-time.After(wire.InputLeaseDuration + time.Second):
	}
	if !g.Online("machine") {
		t.Fatal("renewing connection went offline")
	}
	if _, err := c.List(ctx); err != nil {
		t.Fatal("current connection cannot execute after original grant expires", err)
	}
	// The new grant is transport state, not a new business request. A repeated
	// create-only write must return the original result rather than run again.
	if err := c.CallID(ctx, "files", requestID, write, nil, nil); err != nil {
		t.Fatal("lease renewal changed request deduplication", err)
	}
	if c.Binding.Incarnation != original.Incarnation || c.Binding.Generation != original.Generation {
		t.Fatal("lease renewal replaced execution identity")
	}
}
