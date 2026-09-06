package tests

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"github.com/aiomni/dune/internal/wire"
	"github.com/aiomni/dune/pkg/api"
	"github.com/aiomni/dune/pkg/sdk"
	pb "github.com/aiomni/dune/proto/dune/dtp/v1"
	"github.com/fasthttp/websocket"
	"github.com/hashicorp/yamux"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"testing"
	"time"
)

func TestHandshakeAndWSFragments(t *testing.T) {
	h := start(t)
	tc, e := h.c.TLS()
	must(t, e)
	dialer := websocket.Dialer{TLSClientConfig: tc}
	w, _, e := dialer.DialContext(h.ctx, h.c.Gateway, http.Header{"Authorization": []string{"Bearer " + h.c.Token}})
	must(t, e)
	defer w.Close()
	sess, e := yamux.Client(wire.NetConn(w), wire.Config())
	must(t, e)
	defer sess.Close()
	raw, e := sess.OpenStream()
	must(t, e)
	st := wire.Wrap(raw)
	must(t, st.Send(&pb.Message{Kind: "request", Operation: "exec", Target: h.c.Target, Payload: api.Payload(api.Exec{Command: api.Command{Argv: []string{"touch", "forbidden"}}, WorkingDirectory: h.dir})}))
	m, e := st.Recv()
	must(t, e)
	if m.Kind != "error" {
		t.Fatal("business before handshake")
	}
	if _, e = os.Stat(filepath.Join(h.dir, "forbidden")); !os.IsNotExist(e) {
		t.Fatal("executed before handshake")
	}
	// The real WS endpoint carries Yamux headers fragmented at every byte.
	w2, _, e := dialer.DialContext(h.ctx, h.c.Gateway, http.Header{"Authorization": []string{"Bearer " + h.c.Token}})
	must(t, e)
	fragmented := &fragmentConn{interfaceConn: wire.NetConn(w2)}
	s2, e := yamux.Client(fragmented, wire.Config())
	must(t, e)
	defer s2.Close()
	ctrl, _, e := wire.Handshake(s2, &pb.Message{Kind: "hello", Target: h.c.Target, Payload: api.Payload(api.Hello{Version: api.Version, Role: "sdk"})})
	must(t, e)
	ctrl.Close()
}

type fragmentConn struct{ interfaceConn }
type interfaceConn interface {
	io.ReadWriteCloser
	SetDeadline(time.Time) error
	SetReadDeadline(time.Time) error
	SetWriteDeadline(time.Time) error
	LocalAddr() net.Addr
	RemoteAddr() net.Addr
}

func (c *fragmentConn) Write(p []byte) (int, error) {
	for i := range p {
		if _, e := c.interfaceConn.Write(p[i : i+1]); e != nil {
			return i, e
		}
	}
	return len(p), nil
}
func TestProfileValidationAndOutput(t *testing.T) {
	h := start(t)
	bad := profile(h.dir, "pty", "/bin/true")
	bad.Start.Run = "echo x"
	if _, _, e := h.client.Start(h.ctx, bad); e == nil {
		t.Fatal("argv/run conflict")
	}
	bad = profile(h.dir, "pty", "/not/exist")
	if _, _, e := h.client.Start(h.ctx, bad); e == nil {
		t.Fatal("missing executable")
	}
	r := h.exec("/usr/bin/printf", "%s", "$HOME")
	if r.Stdout != "$HOME" {
		t.Fatal("argv expanded")
	}
	r = h.exec("/bin/sh", "-c", "head -c 500000 /dev/zero")
	if !r.Truncated || len(r.Stdout) != 128*1024 {
		t.Fatal("unbounded or truncated incorrectly", len(r.Stdout))
	}
	p := profile(h.dir, "pty", "/bin/sh", "-c", "printf OLD; sleep 1; printf NEW; sleep 1")
	rt, s, e := h.client.Start(h.ctx, p)
	must(t, e)
	receive(t, s, "data", "OLD")
	s.Close()
	time.Sleep(100 * time.Millisecond)
	s, e = h.client.Attach(h.ctx, rt, false)
	must(t, e)
	defer s.Close()
	var out bytes.Buffer
	for {
		m, e := s.Recv()
		must(t, e)
		if m.Kind == "data" {
			out.Write(m.Data)
		}
		if m.Kind == "exit" {
			break
		}
	}
	if !strings.Contains(out.String(), "OLD") || !strings.Contains(out.String(), "NEW") {
		t.Fatal("tmux redraw or live output missing", out.String())
	}
	rt.Generation = 2
	if _, e = h.client.Get(h.ctx, rt); e == nil {
		t.Fatal("stale generation")
	}
}
func TestGitConflicts(t *testing.T) {
	h := start(t)
	g := func(a api.Git, ok bool) api.GitResult {
		t.Helper()
		a.Directory = h.dir
		r, e := h.client.Git(h.ctx, a)
		must(t, e)
		if (r.ExitCode == 0) != ok {
			t.Fatalf("%s %+v", a.Action, r)
		}
		return r
	}
	for _, a := range [][]string{{"git", "init", "-b", "main"}, {"git", "config", "user.email", "dune@example.test"}, {"git", "config", "user.name", "Dune"}} {
		if r := h.exec(a...); r.ExitCode != 0 {
			t.Fatal(r)
		}
	}
	write := func(s string) { must(t, os.WriteFile(filepath.Join(h.dir, "conflict"), []byte(s+"\n"), 0600)) }
	commit := func(msg string) {
		g(api.Git{Action: "stage", Paths: []string{"conflict"}}, true)
		g(api.Git{Action: "commit", Message: msg}, true)
	}
	write("base")
	commit("base")
	g(api.Git{Action: "branch", Name: "feature"}, true)
	write("main")
	commit("main")
	g(api.Git{Action: "checkout", Ref: "feature"}, true)
	write("feature")
	commit("feature")
	g(api.Git{Action: "checkout", Ref: "main"}, true)
	for _, mode := range []string{"abort", "continue"} {
		g(api.Git{Action: "merge", Ref: "feature"}, false)
		r := g(api.Git{Action: "conflicts"}, true)
		if len(r.Conflicts) != 1 || r.Conflicts[0] != "conflict" {
			t.Fatal(r)
		}
		if mode == "continue" {
			write("resolved")
			g(api.Git{Action: "stage", Paths: []string{"conflict"}}, true)
		}
		g(api.Git{Action: "merge", Mode: mode}, true)
	}
	// Rebase the original feature against the merged main's conflicting resolution.
	g(api.Git{Action: "checkout", Ref: "feature"}, true)
	write("new feature")
	commit("new feature")
	for _, mode := range []string{"abort", "continue"} {
		g(api.Git{Action: "rebase", Ref: "main"}, false)
		g(api.Git{Action: "conflicts"}, true)
		if mode == "continue" {
			write("rebased")
			g(api.Git{Action: "stage", Paths: []string{"conflict"}}, true)
		}
		g(api.Git{Action: "rebase", Mode: mode}, true)
	}
	if r := g(api.Git{Action: "status"}, true); strings.Contains(r.Stdout, "UU") {
		t.Fatal(r)
	}
	if _, e := h.client.Git(h.ctx, api.Git{Action: "checkout", Directory: h.dir, Ref: "--evil"}); e == nil {
		t.Fatal("argument injection")
	}
	patch := "diff --git a/conflict b/conflict\n--- a/conflict\n+++ b/conflict\n@@ -1 +1 @@\n-stale\n+wrong\n"
	g(api.Git{Action: "stage", Patch: patch}, false)
	g(api.Git{Action: "stash", Mode: "push"}, true)
}
func TestSlowConsumerIsolation(t *testing.T) {
	h := start(t)
	ctx, cancel := context.WithTimeout(h.ctx, 15*time.Second)
	defer cancel()
	rt, s, e := h.client.Start(ctx, profile(h.dir, "pty", "/bin/sh", "-c", "seq 1 100000; echo OUTPUT_FINISHED; touch output-finished; sleep 120"))
	must(t, e)
	defer h.client.Stop(h.ctx, rt)
	// Do not read the attached viewer while the native server consumes output.
	time.Sleep(3 * time.Second)
	start := time.Now()
	r := h.exec("/bin/echo", "still-responsive")
	if r.ExitCode != 0 || time.Since(start) > 2*time.Second {
		t.Fatal("slow viewer stalled another operation")
	}
	if _, e := os.Stat(filepath.Join(h.dir, "output-finished")); e != nil {
		t.Fatal("slow viewer blocked terminal output", e)
	}
	s.Close()
	time.Sleep(100 * time.Millisecond)
	s, e = h.client.Attach(ctx, rt, false)
	must(t, e)
	defer s.Close()
	receive(t, s, "data", "OUTPUT_FINISHED")
}
func TestMultiClient(t *testing.T) {
	h := start(t)
	tc, e := h.c.TLS()
	must(t, e)
	other, e := sdk.Dial(h.ctx, sdk.Options{Gateway: h.c.Gateway, Token: h.c.Token, Target: h.c.Target, TLSConfig: tc})
	must(t, e)
	defer other.Close()
	done := make(chan error, 2)
	for i, c := range []*sdk.Client{h.client, other} {
		go func(i int, c *sdk.Client) {
			r, e := c.Exec(h.ctx, api.Exec{Command: api.Command{Argv: []string{"echo", fmt.Sprint(i)}}, WorkingDirectory: h.dir})
			if e == nil && r.Stdout != fmt.Sprintln(i) {
				e = fmt.Errorf("crossed response %q", r.Stdout)
			}
			done <- e
		}(i, c)
	}
	must(t, <-done)
	must(t, <-done)
}

func TestMixedLoad(t *testing.T) {
	h := start(t)
	var streams []*sdk.Stream
	var runtimes []api.Runtime
	for _, adapter := range []string{"pty", "pty", "acp", "acp"} {
		var p api.Profile
		if adapter == "pty" {
			p = profile(h.dir, adapter, "/bin/sh", "-c", "stty -echo; cat")
		} else {
			p = profile(h.dir, adapter, "/bin/cat")
		}
		rt, s, e := h.client.Start(h.ctx, p)
		must(t, e)
		streams = append(streams, s)
		runtimes = append(runtimes, rt)
	}
	defer func() {
		for i, s := range streams {
			s.Close()
			h.client.Stop(h.ctx, runtimes[i])
		}
	}()
	ln, e := net.Listen("tcp", "127.0.0.1:0")
	must(t, e)
	defer ln.Close()
	go func() {
		conn, e := ln.Accept()
		if e != nil {
			return
		}
		defer conn.Close()
		io.Copy(conn, conn)
	}()
	port, e := h.client.Connect(h.ctx, ln.Addr().(*net.TCPAddr).Port)
	must(t, e)
	defer port.Close()
	done := make(chan error, 2)
	go func() {
		for i := 0; i < 128; i++ {
			chunk := bytes.Repeat([]byte("p"), 32768)
			if _, e := port.Write(chunk); e != nil {
				done <- e
				return
			}
			out := make([]byte, len(chunk))
			if _, e := io.ReadFull(port, out); e != nil {
				done <- e
				return
			}
			if !bytes.Equal(out, chunk) {
				done <- fmt.Errorf("port mismatch")
				return
			}
		}
		done <- nil
	}()
	go func() {
		chunk := bytes.Repeat([]byte("u"), 32768)
		hash := sha256.New()
		for i := 0; i < 512; i++ {
			hash.Write(chunk)
		}
		u, e := h.client.Upload(h.ctx, api.Upload{Action: "create", Path: filepath.Join(h.dir, "bulk"), Size: 16 * 1024 * 1024, SHA256: hex.EncodeToString(hash.Sum(nil))})
		if e != nil {
			done <- e
			return
		}
		for i := 0; i < 512; i++ {
			u, e = h.client.Upload(h.ctx, api.Upload{Action: "chunk", ID: u.ID, Offset: u.Offset, Data: chunk})
			if e != nil {
				done <- e
				return
			}
		}
		_, e = h.client.Upload(h.ctx, api.Upload{Action: "commit", ID: u.ID})
		done <- e
	}()
	var latencies []time.Duration
	for turn := 0; turn < 5; turn++ {
		for i, s := range streams {
			token := fmt.Sprintf("round-%d-agent-%d", turn, i)
			message := token + "\n"
			if i >= 2 {
				message = fmt.Sprintf(`{"jsonrpc":"2.0","method":"%s"}`, token)
			}
			start := time.Now()
			_, e = s.Input([]byte(message))
			must(t, e)
			receive(t, s, "data", token)
			latencies = append(latencies, time.Since(start))
		}
	}
	must(t, <-done)
	must(t, <-done)
	sort.Slice(latencies, func(i, j int) bool { return latencies[i] < latencies[j] })
	p95 := latencies[len(latencies)*95/100-1]
	t.Logf("2 PTY + 2 ACP + 16MiB upload + 4MiB TCP: p95=%s max=%s", p95, latencies[len(latencies)-1])
	if p95 > 2*time.Second {
		t.Fatal("interactive p95 exceeded 2 seconds")
	}
}

func TestMockACP(t *testing.T) {
	h := start(t)
	mock := filepath.Join(h.dir, "mock-acp")
	build := exec.Command("go", "build", "-o", mock, "../samples/mock-acp")
	if b, e := build.CombinedOutput(); e != nil {
		t.Fatalf("mock build: %v %s", e, b)
	}
	rt, s, e := h.client.Start(h.ctx, profile(h.dir, "acp", mock))
	must(t, e)
	defer s.Close()
	defer h.client.Stop(h.ctx, rt)
	for _, line := range []string{`{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":1}}`, `{"jsonrpc":"2.0","id":2,"method":"session/new","params":{"cwd":"/tmp","mcpServers":[]}}`, `{"jsonrpc":"2.0","id":3,"method":"session/prompt","params":{"sessionId":"mock-session","prompt":[{"type":"text","text":"test permission"}]}}`} {
		_, e = s.Input([]byte(line))
		must(t, e)
		receive(t, s, "data", "jsonrpc")
	}
	_, e = s.Input([]byte(`{"jsonrpc":"2.0","id":"mock-permission","result":{"outcome":{"outcome":"selected","optionId":"allow"}}}`))
	must(t, e)
	receive(t, s, "data", "Mock permission response received")
	receive(t, s, "data", "end_turn")
	_, e = s.Input([]byte(`{"jsonrpc":"2.0","id":4,"method":"unknown"}`))
	must(t, e)
	receive(t, s, "data", "Method not found")
}

func TestPendingStreamLimit(t *testing.T) {
	h := start(t)
	tc, e := h.c.TLS()
	must(t, e)
	sess, e := wire.Dial(h.ctx, h.c.Gateway, h.c.Token, tc)
	must(t, e)
	defer sess.Close()
	ctrl, _, e := wire.Handshake(sess, &pb.Message{Kind: "hello", Target: h.c.Target, Payload: api.Payload(api.Hello{Version: api.Version, Role: "sdk"})})
	must(t, e)
	defer ctrl.Close()
	var raw []*yamux.Stream
	for i := 0; i < 65; i++ {
		s, e := sess.OpenStream()
		must(t, e)
		raw = append(raw, s)
	}
	defer func() {
		for _, s := range raw {
			s.Close()
		}
	}()
	must(t, raw[64].SetReadDeadline(time.Now().Add(2*time.Second)))
	b := make([]byte, 1)
	_, e = raw[64].Read(b)
	if e == nil {
		t.Fatal("65th pending stream should close")
	}
	if ne, ok := e.(net.Error); ok && ne.Timeout() {
		t.Fatal("stream limit did not reject promptly")
	}
	r := h.exec("echo", "other-client")
	if r.Stdout != "other-client\n" {
		t.Fatal(r)
	}
}

func TestSignalAndInvalidInput(t *testing.T) {
	h := start(t)
	rt, s, e := h.client.Start(h.ctx, profile(h.dir, "pty", "/bin/sleep", "30"))
	must(t, e)
	defer s.Close()
	must(t, s.Signal("TERM"))
	receive(t, s, "exit", "")
	if _, e := h.client.Get(h.ctx, rt); e == nil {
		t.Fatal("destroyed session is still listed")
	}
	rt, s, e = h.client.Start(h.ctx, profile(h.dir, "acp", "/bin/cat"))
	must(t, e)
	defer s.Close()
	defer h.client.Stop(h.ctx, rt)
	_, e = s.Input([]byte(`{"invalid":true}`))
	must(t, e)
	_, e = s.Recv()
	if e == nil || !strings.Contains(e.Error(), "INPUT_FAILED") {
		t.Fatal("invalid ACP input was accepted", e)
	}
}

func TestHTTPAuthorization(t *testing.T) {
	h := start(t)
	if !strings.HasPrefix(h.c.Gateway, "ws://") {
		t.Fatal("expected plain WebSocket")
	}
	for _, token := range []string{"", "Bearer invalid"} {
		headers := http.Header{}
		if token != "" {
			headers.Set("Authorization", token)
		}
		conn, res, e := websocket.DefaultDialer.DialContext(h.ctx, h.c.Gateway, headers)
		if conn != nil {
			conn.Close()
		}
		if e == nil || res == nil || res.StatusCode != 401 {
			t.Fatalf("invalid token: response=%v err=%v", res, e)
		}
		res.Body.Close()
	}
	r := h.exec("/bin/echo", "http-auth-ok")
	if r.Stdout != "http-auth-ok\n" {
		t.Fatal(r)
	}
}
