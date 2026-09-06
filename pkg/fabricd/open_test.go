package fabricd

import (
	"context"
	"net"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/aiomni/dune/internal/wire"
	"github.com/hashicorp/yamux"
)

func TestMain(m *testing.M) {
	if code, handled := RunHelper(os.Args[1:]); handled {
		os.Exit(code)
	}
	if os.Getenv("DUNE_TMUX") == "" {
		binary, _ := filepath.Abs("../../bin/tmux")
		_ = os.Setenv("DUNE_TMUX", binary)
	}
	os.Exit(m.Run())
}

func TestOpenOwnsStateDirectoryUntilClose(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "state")
	first, err := Open(context.Background(), dir)
	if err != nil {
		t.Fatal(err)
	}
	defer first.Close()
	if second, err := Open(context.Background(), dir); err == nil {
		second.Close()
		t.Fatal("two engines acquired the same state directory")
	}
	// A failed competing open must not release the first engine's lock.
	if second, err := Open(context.Background(), dir); err == nil {
		second.Close()
		t.Fatal("failed competing open released the live engine lock")
	}
	first.Close()
	first.Close()
	second, err := Open(context.Background(), dir)
	if err != nil {
		t.Fatal(err)
	}
	defer second.Close()
}

func TestEngineCloseCancelsHandshake(t *testing.T) {
	engine := newEngine(context.Background())
	defer engine.Close()
	local, remote := net.Pipe()
	peer, err := yamux.Server(remote, wire.Config())
	if err != nil {
		t.Fatal(err)
	}
	defer peer.Close()
	done := make(chan error, 1)
	go func() { done <- engine.ServeConn(context.Background(), local, "machine") }()
	raw, err := peer.AcceptStream()
	if err != nil {
		t.Fatal(err)
	}
	stream := wire.Wrap(raw)
	_ = stream.SetReadDeadline(time.Now().Add(time.Second))
	if _, err := stream.Recv(); err != nil {
		t.Fatal(err)
	}
	engine.Close()
	select {
	case err := <-done:
		if err == nil {
			t.Fatal("engine closed during handshake but handshake succeeded")
		}
	case <-time.After(time.Second):
		t.Fatal("engine close did not unblock handshake")
	}
	local, remote = net.Pipe()
	defer remote.Close()
	if err := engine.ServeConn(context.Background(), local, "machine"); err == nil {
		t.Fatal("closed engine accepted another connection")
	}
	if _, err := remote.Write([]byte("x")); err == nil {
		t.Fatal("rejected connection remained open")
	}
}
