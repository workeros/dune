package fabricd

import (
	"bytes"
	"context"
	"net"
	"testing"
	"time"

	"github.com/aiomni/dune/internal/wire"
	pb "github.com/aiomni/dune/proto/dune/dtp/v1"
	"github.com/hashicorp/yamux"
)

func TestBlockedProxyWriteDoesNotHoldConnectionLockForStop(t *testing.T) {
	left, right := net.Pipe()
	connector, err := yamux.Client(left, wire.Config())
	if err != nil {
		t.Fatal(err)
	}
	defer connector.Close()
	host, err := yamux.Server(right, wire.Config())
	if err != nil {
		t.Fatal(err)
	}
	defer host.Close()
	proxy := &sessionProxy{connection: connector, connector: "connector", term: 1}
	ctx, cancel := context.WithTimeout(t.Context(), 3*time.Second)
	defer cancel()
	ordinary := make(chan error, 1)
	go func() {
		s, err := proxy.open(ctx, &pb.Message{Kind: "request", Operation: "submission.acp", Payload: bytes.Repeat([]byte("x"), 512*1024)})
		if s != nil {
			s.Close()
		}
		ordinary <- err
	}()
	blocked, err := host.AcceptStream()
	if err != nil {
		t.Fatal(err)
	}
	defer blocked.Close()
	// Do not consume this stream: its message exceeds the 256 KiB Yamux
	// window. A stop still needs to open and write a separate small stream.
	stopping := make(chan error, 1)
	go func() {
		s, err := proxy.open(ctx, &pb.Message{Kind: "request", Operation: "runtime.stop"})
		if s != nil {
			s.Close()
		}
		stopping <- err
	}()
	select {
	case err := <-stopping:
		if err != nil {
			t.Fatal("blocked ordinary write delayed stop", err)
		}
	case <-ctx.Done():
		t.Fatal("ordinary stream monopolized the proxy connection lock")
	}
	stop, err := host.AcceptStream()
	if err != nil {
		t.Fatal(err)
	}
	defer stop.Close()
	if m, err := wire.Read(stop); err != nil || m.Operation != "runtime.stop" {
		t.Fatal("stop not delivered on its own stream", m, err)
	}
	select {
	case err := <-ordinary:
		t.Fatal("ordinary write unexpectedly finished without consumption", err)
	default:
	}
	cancel()
	select {
	case err := <-ordinary:
		if err == nil {
			t.Fatal("cancelled incomplete proxy write succeeded")
		}
	case <-time.After(time.Second):
		t.Fatal("cancelled proxy write did not release its stream")
	}
}
