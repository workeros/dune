package client

import (
	"context"
	"net"
	"testing"
	"time"

	"github.com/aiomni/dune/internal/wire"
	"github.com/aiomni/dune/pkg/api"
	pb "github.com/aiomni/dune/proto/dune/dtp/v1"
	"github.com/hashicorp/yamux"
)

func TestConnectCancellationClosesConnection(t *testing.T) {
	local, remote := net.Pipe()
	server, err := yamux.Server(remote, wire.Config())
	if err != nil {
		t.Fatal(err)
	}
	defer server.Close()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() { _, err := Connect(ctx, local, "machine"); done <- err }()
	raw, err := server.AcceptStream()
	if err != nil {
		t.Fatal(err)
	}
	st := wire.Wrap(raw)
	_ = st.SetReadDeadline(time.Now().Add(time.Second))
	if _, err := st.Recv(); err != nil {
		t.Fatal(err)
	}
	// The peer received hello but never answers. Cancellation must interrupt
	// the pending welcome read, rather than wait for the handshake timeout.
	cancel()
	select {
	case err := <-done:
		if err == nil {
			t.Fatal("cancelled handshake succeeded")
		}
	case <-time.After(time.Second):
		t.Fatal("cancellation did not unblock handshake")
	}
	if _, err := remote.Write([]byte("x")); err == nil {
		t.Fatal("connection remained open")
	}
}

func TestConnectUsesEstablishedConnection(t *testing.T) {
	local, remote := net.Pipe()
	s, err := yamux.Server(remote, wire.Config())
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	done := make(chan error, 1)
	go func() {
		raw, err := s.AcceptStream()
		if err != nil {
			done <- err
			return
		}
		st := wire.Wrap(raw)
		m, err := st.Recv()
		if err == nil && m.Target != "machine" {
			t.Errorf("target = %q", m.Target)
		}
		if err == nil {
			err = st.Send(&pb.Message{Kind: "welcome", Payload: api.Payload(api.Binding{Target: "machine", Incarnation: "boot", Generation: 1})})
		}
		done <- err
	}()
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	c, err := Connect(ctx, local, "machine")
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	if c.Binding.Target != "machine" {
		t.Fatalf("binding = %+v", c.Binding)
	}
	cancel()
	if c.s.IsClosed() {
		t.Fatal("dial context cancelled an established client")
	}
}
