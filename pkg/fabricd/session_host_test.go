package fabricd

import (
	"net"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/aiomni/dune/internal/wire"
	"github.com/aiomni/dune/pkg/api"
	pb "github.com/aiomni/dune/proto/dune/dtp/v1"
	"github.com/hashicorp/yamux"
)

func TestSessionHostControlFencesOlderConnectorsAndProbesAreReadOnly(t *testing.T) {
	a, _ := queueFixture(t)
	r := a.r
	r.id, r.inc = wire.ID(), wire.ID()
	d := newEngine(t.Context())
	d.runtimes[r.id] = r
	t.Cleanup(func() { delete(d.runtimes, r.id); d.Close() })
	reg := sessionRegistration{Version: sessionProtocol, Instance: wire.ID(), Installation: installationID(t.TempDir()), Machine: "machine", Runtime: r.info()}
	control := &sessionControl{}
	connect := func(hello sessionHello) (*yamux.Session, *pb.Message) {
		t.Helper()
		local, remote := net.Pipe()
		go serveSessionConnection(t.Context(), remote, d, r, reg, control)
		sess, err := yamux.Client(local, wire.Config())
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { sess.Close() })
		raw, err := sess.OpenStream()
		if err != nil {
			t.Fatal(err)
		}
		stream := wire.Wrap(raw)
		_ = stream.SetReadDeadline(time.Now().Add(time.Second))
		// A rejected peer may send its reply and close before Send returns.
		// Consume the original reply even then; never replay the handshake.
		sendErr := stream.Send(&pb.Message{Kind: "host.connect", Payload: api.Payload(hello)})
		m, err := stream.Recv()
		if err != nil {
			t.Fatalf("host reply unavailable: send=%v receive=%v", sendErr, err)
		}
		return sess, m
	}
	firstHello := sessionHello{Registration: reg, Term: 1, Connector: wire.ID()}
	first, response := connect(firstHello)
	if response.Kind != "host.ready" {
		t.Fatal(response)
	}
	wrongProgram := reg
	wrongProgram.Program.SHA256 = "different-program"
	_, response = connect(sessionHello{Registration: wrongProgram, Probe: true})
	if response.Kind != "error" || response.Code != "HOST_IDENTITY_MISMATCH" {
		t.Fatal("program identity mismatch accepted", response)
	}
	_, response = connect(sessionHello{Registration: reg, Probe: true})
	control.mu.RLock()
	unchanged := control.term == 1 && control.connector == firstHello.Connector
	control.mu.RUnlock()
	if response.Kind != "host.ready" || !unchanged || first.IsClosed() {
		t.Fatal("read-only probe took control", response)
	}
	secondHello := sessionHello{Registration: reg, Term: 2, Connector: wire.ID()}
	_, response = connect(secondHello)
	if response.Kind != "host.ready" {
		t.Fatal(response)
	}
	select {
	case <-first.CloseChan():
	case <-time.After(time.Second):
		t.Fatal("old IPC connection retained control")
	}
	_, response = connect(firstHello)
	if response.Code != "STALE_CONTROL" {
		t.Fatal("late old connector reclaimed the host", response)
	}
	forged := secondHello
	forged.Connector = wire.ID()
	_, response = connect(forged)
	if response.Code != "STALE_CONTROL" {
		t.Fatal("another connector reused a control term", response)
	}
	forged = secondHello
	forged.Registration.Instance = wire.ID()
	_, response = connect(forged)
	if response.Code != "HOST_IDENTITY_MISMATCH" {
		t.Fatal("reused endpoint accepted a different host identity", response)
	}
}

func TestSessionFilesAndControlTerms(t *testing.T) {
	dir := t.TempDir()
	for expected := uint64(1); expected <= 3; expected++ {
		term, err := nextSessionTerm(dir)
		if err != nil || term != expected {
			t.Fatal("control term did not persist", term, err)
		}
	}
	path := filepath.Join(dir, "acp-control-term.json")
	if err := os.Chmod(path, 0644); err != nil {
		t.Fatal(err)
	}
	if _, err := nextSessionTerm(dir); err == nil {
		t.Fatal("accepted nonprivate control evidence")
	}
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join(dir, "missing"), path); err != nil {
		t.Fatal(err)
	}
	if _, err := nextSessionTerm(dir); err == nil {
		t.Fatal("followed a substituted control file")
	}
}
