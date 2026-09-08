package access

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/aiomni/dune/internal/tmux"
	"github.com/aiomni/dune/pkg/api"
	"github.com/aiomni/dune/pkg/client"
	"github.com/aiomni/dune/pkg/fabricd"
	"github.com/aiomni/dune/pkg/gateway"
	"github.com/aiomni/dune/pkg/runner"
	pb "github.com/aiomni/dune/proto/dune/dtp/v1"
)

type checkFunc func(context.Context, Request) (Decision, error)

func (f checkFunc) Check(ctx context.Context, r Request) (Decision, error) { return f(ctx, r) }
func testScope() Scope {
	return Scope{PrincipalID: "alice", Namespace: "verified-issuer", Subject: "verified-subject", OwnerID: "alice", Binding: runner.Binding{RunnerID: "logical", FabricID: "attached", MachineID: "machine", Revision: 7}}
}
func allow(r Request, duration time.Duration) Decision {
	return Decision{Allowed: true, Reason: "TEST_ALLOW", ID: r.RequestID, ValidUntil: time.Now().Add(duration)}
}

func TestMain(m *testing.M) {
	if code, handled := fabricd.RunHelper(os.Args[1:]); handled {
		os.Exit(code)
	}
	if os.Getenv("DUNE_TMUX") == "" {
		path, _ := filepath.Abs("../../bin/tmux")
		os.Setenv("DUNE_TMUX", path)
	}
	os.Exit(m.Run())
}

func policyFixture(t *testing.T, checker Checker, valid func() bool) (context.Context, *client.Client) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	g := gateway.New()
	dir := filepath.Join(t.TempDir(), "fabricd")
	engine, err := fabricd.Open(ctx, dir)
	if err != nil {
		cancel()
		g.Close()
		t.Fatal(err)
	}
	var wg sync.WaitGroup
	t.Cleanup(func() {
		cancel()
		g.Close()
		engine.Close()
		wg.Wait()
		manager, err := tmux.Open(dir)
		if err == nil {
			manager.Close()
		}
	})
	left, right := net.Pipe()
	binding, handler, err := (Grant{Target: "machine", Role: gateway.RoleDaemon}).Bind()
	if err != nil {
		t.Fatal(err)
	}
	wg.Go(func() { g.ServeConn(ctx, left, binding, handler) })
	wg.Go(func() { engine.ServeConn(ctx, right, "machine") })
	for !g.Online("machine") {
		select {
		case <-ctx.Done():
			t.Fatal("fabricd did not register")
		case <-time.After(time.Millisecond):
		}
	}
	policy := &Policy{Scope: testScope(), Checker: checker}
	binding, handler, err = (Grant{Target: "machine", Role: gateway.RoleSDK, Policy: policy, Valid: valid}).Bind()
	if err != nil {
		t.Fatal(err)
	}
	// Caller mutation must not replace the authenticated scope after Bind.
	policy.Scope.PrincipalID = "mallory"
	policy.Scope.Namespace = "forged-issuer"
	policy.Scope.Subject = "forged-subject"
	policy.Scope.Binding.Revision = 99
	left, right = net.Pipe()
	wg.Go(func() { g.ServeConn(ctx, left, binding, handler) })
	sdk, err := client.Connect(ctx, right, "machine")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { sdk.Close() })
	return ctx, sdk
}

func TestPolicyExecutionAndReadOnlyStreams(t *testing.T) {
	var mu sync.Mutex
	var seen []Request
	checker := checkFunc(func(ctx context.Context, r Request) (Decision, error) {
		mu.Lock()
		seen = append(seen, r)
		mu.Unlock()
		if r.Scope != testScope() {
			t.Error("request changed authenticated scope")
		}
		d := allow(r, time.Second)
		d.Allowed = r.Operation != "exec" && !(r.Operation == "files" && r.Suboperation == "write") && !(r.Operation == "upload" && r.Suboperation == "commit")
		return d, nil
	})
	ctx, c := policyFixture(t, checker, nil)
	dir := t.TempDir()
	forbidden := filepath.Join(dir, "forbidden")
	if err := c.Call(ctx, "files", api.File{Action: "write", Path: forbidden, Data: []byte("private-file-data")}, nil); err == nil {
		t.Fatal("denied file write executed")
	}
	if _, err := os.Stat(forbidden); !os.IsNotExist(err) {
		t.Fatal("denied request created a file")
	}
	if err := c.Call(ctx, "exec", api.Exec{Command: api.Command{Argv: []string{"/bin/sh", "-c", "touch " + forbidden}}, WorkingDirectory: dir}, nil); err == nil {
		t.Fatal("denied exec succeeded")
	}
	contents := []byte("private-file-data")
	sum := sha256.Sum256(contents)
	var upload api.UploadState
	if err := c.Call(ctx, "upload", api.Upload{Action: "create", Path: forbidden, Size: int64(len(contents)), SHA256: hex.EncodeToString(sum[:])}, &upload); err != nil {
		t.Fatal(err)
	}
	if err := c.Call(ctx, "upload", api.Upload{Action: "chunk", ID: upload.ID, Data: contents}, nil); err != nil {
		t.Fatal(err)
	}
	if err := c.Call(ctx, "upload", api.Upload{Action: "commit", ID: upload.ID}, nil); err == nil {
		t.Fatal("upload commit bypassed access check")
	}
	if _, err := os.Stat(forbidden); !os.IsNotExist(err) {
		t.Fatal("denied upload committed a file")
	}
	if err := c.Call(ctx, "upload", api.Upload{Action: "cancel", ID: upload.ID}, nil); err != nil {
		t.Fatal(err)
	}
	r, s, err := c.Start(ctx, api.Profile{Version: 1, Kind: "agent", WorkingDirectory: dir, Adapter: "pty", Start: api.Command{Argv: []string{"/bin/sh"}}, Env: map[string]string{"PRIVATE_ENV": "private-env-value"}})
	if err != nil {
		t.Fatal(err)
	}
	s.Close()
	t.Cleanup(func() { c.Stop(context.Background(), r) })
	for _, kind := range []string{"input", "resize", "signal"} {
		observer, err := c.Attach(ctx, r, true)
		if err != nil {
			t.Fatal(err)
		}
		switch kind {
		case "input":
			_, err = observer.Input([]byte("touch " + forbidden + "\n"))
		case "resize":
			err = observer.Resize(25, 80)
		case "signal":
			err = observer.Signal("TERM")
		}
		if err != nil {
			observer.Close()
			t.Fatal(err)
		}
		for {
			_, err = observer.Recv()
			if err != nil {
				break
			}
		}
		observer.Close()
	}
	if _, err := os.Stat(forbidden); !os.IsNotExist(err) {
		t.Fatal("observer sent input")
	}
	var current api.Runtime
	if err := c.CallID(ctx, "runtime.get", "check-running", nil, &current, &r); err != nil || current.State != "running" {
		t.Fatal("observer signal reached runtime", err)
	}
	mu.Lock()
	data, _ := json.Marshal(seen)
	mu.Unlock()
	if strings.Contains(string(data), "private-file-data") || strings.Contains(string(data), "private-env-value") || strings.Contains(string(data), "touch ") {
		t.Fatal("checker received command or content")
	}
}

func TestPolicyIdleRenewalFailureClosesStream(t *testing.T) {
	var calls atomic.Int32
	ctx, c := policyFixture(t, checkFunc(func(ctx context.Context, r Request) (Decision, error) {
		if r.Operation == "profile.start" && calls.Add(1) > 1 {
			return Decision{}, errors.New("private enterprise error")
		}
		return allow(r, 400*time.Millisecond), nil
	}), nil)
	_, s, err := c.Start(ctx, api.Profile{Version: 1, Kind: "agent", WorkingDirectory: t.TempDir(), Adapter: "pty", Start: api.Command{Argv: []string{"/bin/sh"}}})
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	started := time.Now()
	for {
		_, err = s.Recv()
		if err != nil {
			break
		}
	}
	if time.Since(started) > 2*time.Second || calls.Load() < 2 {
		t.Fatal("idle access was not renewed/revoked", calls.Load())
	}
}

func TestPolicyDecisionValidation(t *testing.T) {
	r := Request{Scope: testScope(), RequestID: "decision", Operation: "runtime.list"}
	for _, d := range []Decision{{Allowed: false}, {Allowed: true, Reason: "OK", ID: "expired", ValidUntil: time.Now().Add(-time.Second)}, {Allowed: true, Reason: "unsafe reason", ID: "decision", ValidUntil: time.Now().Add(time.Hour)}} {
		p := Policy{Scope: r.Scope, Checker: checkFunc(func(context.Context, Request) (Decision, error) { return d, nil })}
		if _, err := p.check(context.Background(), r); !errors.Is(err, ErrDenied) {
			t.Fatal("invalid decision accepted", err)
		}
	}
	p := Policy{Scope: r.Scope, Checker: checkFunc(func(context.Context, Request) (Decision, error) { return allow(r, time.Hour), nil })}
	d, err := p.check(context.Background(), r)
	if err != nil || time.Until(d.ValidUntil) > MaxLease {
		t.Fatal("unbounded access lease", err)
	}
}

func TestPolicyObserverReceivesFinalDecision(t *testing.T) {
	request := Request{Scope: testScope(), RequestID: "observed-request", Operation: "runtime.list", Suboperation: "all"}
	observed := make(chan struct {
		request  Request
		decision Decision
		err      error
		elapsed  time.Duration
	}, 2)
	observer := func(r Request, d Decision, err error, elapsed time.Duration) {
		observed <- struct {
			request  Request
			decision Decision
			err      error
			elapsed  time.Duration
		}{r, d, err, elapsed}
	}
	allowed := Policy{Scope: request.Scope, Checker: checkFunc(func(context.Context, Request) (Decision, error) {
		return allow(request, time.Hour), nil
	}), Observer: observer}
	if _, err := allowed.check(context.Background(), request); err != nil {
		t.Fatal(err)
	}
	first := <-observed
	if first.request != request || first.decision.ID != request.RequestID || first.err != nil || first.elapsed < 0 {
		t.Fatal("observer did not receive final allowed decision", first)
	}

	denied := Policy{Scope: request.Scope, Checker: checkFunc(func(context.Context, Request) (Decision, error) {
		return Decision{Allowed: false, Reason: "TEST_DENY", ID: request.RequestID, ValidUntil: time.Now().Add(time.Minute)}, nil
	}), Observer: observer}
	if _, err := denied.check(context.Background(), request); !errors.Is(err, ErrDenied) {
		t.Fatal("denial was not enforced", err)
	}
	second := <-observed
	if !errors.Is(second.err, ErrDenied) || second.decision != (Decision{}) {
		t.Fatal("observer did not receive normalized denial", second)
	}
	panicking := Policy{Scope: request.Scope, Checker: allowed.Checker, Observer: func(Request, Decision, error, time.Duration) { panic("diagnostic failure") }}
	if _, err := panicking.check(context.Background(), request); err != nil {
		t.Fatal("observer panic changed access result", err)
	}
}

func TestOperationMappingRejectsUnknownAndKeepsContentPrivate(t *testing.T) {
	cases := []struct {
		op        string
		body      any
		sub, mode string
	}{
		{"files", api.File{Action: "read", Path: "/tmp/file", Data: []byte("secret")}, "read", ""},
		{"upload", api.Upload{Action: "commit", ID: "upload", Data: []byte("secret")}, "commit", ""},
		{"git", api.Git{Action: "stash", Mode: "list", Patch: "secret", Message: "secret"}, "stash", "list"},
		{"git", api.Git{Action: "branch", Name: "feature"}, "branch", "create"},
		{"git", api.Git{Action: "branch"}, "branch", "list"},
		{"acp.action", map[string]string{"action": "permission", "prompt": "secret"}, "permission", ""},
		{"agent.config", api.AgentConfigRequest{Action: "save", Config: &api.AgentConfig{ID: "config", Command: "secret"}}, "save", ""},
	}
	for _, tc := range cases {
		r, err := Describe(testScope(), &pb.Message{Kind: "request", Target: "machine", RequestId: "request", Operation: tc.op, Payload: api.Payload(tc.body)})
		if err != nil || r.Suboperation != tc.sub || r.Mode != tc.mode {
			t.Fatal("operation not mapped", tc.op, err)
		}
		data, _ := json.Marshal(r)
		if strings.Contains(string(data), "secret") {
			t.Fatal("request content exposed")
		}
	}
	for _, tc := range []struct{ op, body string }{{"new.operation", "{}"}, {"files", `{"action":"new"}`}, {"git", `{"action":"stash","mode":"unknown"}`}, {"upload", `{"action":"begin"}`}, {"acp.action", `{"action":"arbitrary-rpc"}`}} {
		if _, err := Describe(testScope(), &pb.Message{Kind: "request", Target: "machine", RequestId: "request", Operation: tc.op, Payload: []byte(tc.body)}); !errors.Is(err, ErrDenied) {
			t.Fatal("unknown operation accepted", tc, err)
		}
	}
	for _, tc := range []struct {
		op   string
		body any
	}{
		{"upload", api.Upload{Action: "commit", ID: "handle", Path: "/forged/allowed/path"}},
		{"acp.action", map[string]string{"action": "prompt", "cwd": "/forged/allowed/path"}},
	} {
		r, err := Describe(testScope(), &pb.Message{Kind: "request", Target: "machine", RequestId: "request", Operation: tc.op, Payload: api.Payload(tc.body)})
		if err != nil || r.Resource.Path != "" || r.Resource.Directory != "" {
			t.Fatal("ignored client fields impersonated effective resource attributes", tc.op, err)
		}
	}
	base := Request{Operation: "runtime.attach", Resource: Resource{Observe: false}}
	r, err := continuation(base, RuntimeIdentity{ID: "runtime", Adapter: "acp"}, &pb.Message{Kind: "input", Data: []byte(`{"method":"session/prompt","text":"secret"}`)})
	if err != nil || r.Operation != "acp.raw" || r.Suboperation != "exchange" {
		t.Fatal("raw ACP was not checked as a whole", err)
	}
}

func TestInputActionLeaseAndLateRefresh(t *testing.T) {
	t.Run("input-cache-and-resize", func(t *testing.T) {
		var inputs atomic.Int32
		ctx, c := policyFixture(t, checkFunc(func(ctx context.Context, r Request) (Decision, error) {
			if r.Suboperation == "input" {
				inputs.Add(1)
			}
			d := allow(r, MaxLease)
			d.Allowed = r.Suboperation != "resize"
			return d, nil
		}), nil)
		r, s, err := c.Start(ctx, api.Profile{Version: 1, Kind: "agent", WorkingDirectory: t.TempDir(), Adapter: "pty", Start: api.Command{Argv: []string{"/bin/sh"}}})
		if err != nil {
			t.Fatal(err)
		}
		defer s.Close()
		t.Cleanup(func() { c.Stop(context.Background(), r) })
		for range 2 {
			id, err := s.Input([]byte("printf POLICY_INPUT_OK\\n\n"))
			if err != nil {
				t.Fatal(err)
			}
			for {
				m, err := s.Recv()
				if err != nil {
					t.Fatal(err)
				}
				if m.Kind == "written" && m.RequestId == id {
					break
				}
			}
		}
		if inputs.Load() != 1 {
			t.Fatal("terminal bytes triggered repeated enterprise checks", inputs.Load())
		}
		if err := s.Resize(30, 100); err != nil {
			t.Fatal(err)
		}
		for {
			_, err = s.Recv()
			if err != nil {
				break
			}
		}
	})
	t.Run("blocked-refresh", func(t *testing.T) {
		release := make(chan struct{})
		finished := make(chan struct{})
		var once sync.Once
		defer once.Do(func() { close(release) })
		var calls atomic.Int32
		ctx, c := policyFixture(t, checkFunc(func(ctx context.Context, r Request) (Decision, error) {
			if r.Operation == "profile.start" && calls.Add(1) > 1 {
				<-release // Deliberately violate the callback contract to test the watchdog.
				defer close(finished)
			}
			return allow(r, 500*time.Millisecond), nil
		}), nil)
		_, s, err := c.Start(ctx, api.Profile{Version: 1, Kind: "agent", WorkingDirectory: t.TempDir(), Adapter: "pty", Start: api.Command{Argv: []string{"/bin/sh"}}})
		if err != nil {
			t.Fatal(err)
		}
		defer s.Close()
		started := time.Now()
		for {
			_, err = s.Recv()
			if err != nil {
				break
			}
		}
		if time.Since(started) > 2*time.Second {
			t.Fatal("blocked checker extended expired access")
		}
		once.Do(func() { close(release) })
		select {
		case <-finished:
		case <-time.After(time.Second):
			t.Fatal("late checker did not return")
		}
		if _, err = s.Recv(); err == nil {
			t.Fatal("late approval resurrected a closed stream")
		}
	})
}

func TestGitWriteAndPortInputAreChecked(t *testing.T) {
	ctx, c := policyFixture(t, checkFunc(func(ctx context.Context, r Request) (Decision, error) {
		d := allow(r, MaxLease)
		d.Allowed = !(r.Operation == "git" && r.Suboperation == "stage") && !(r.Operation == "ports.connect" && r.Suboperation == "data")
		return d, nil
	}), nil)
	dir := t.TempDir()
	if out, err := exec.Command("git", "init", "-q", dir).CombinedOutput(); err != nil {
		t.Fatalf("git init: %v %s", err, out)
	}
	if err := os.WriteFile(filepath.Join(dir, "file"), []byte("private diff content"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := c.Call(ctx, "git", api.Git{Action: "stage", Directory: dir, Paths: []string{"file"}}, nil); err == nil {
		t.Fatal("denied Git stage succeeded")
	}
	if out, err := exec.Command("git", "-C", dir, "ls-files").Output(); err != nil || len(out) != 0 {
		t.Fatal("denied Git stage changed the index", err)
	}
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	received := make(chan int, 1)
	go func() {
		conn, err := listener.Accept()
		if err != nil {
			received <- 0
			return
		}
		defer conn.Close()
		conn.SetReadDeadline(time.Now().Add(3 * time.Second))
		data := make([]byte, 32)
		n, _ := conn.Read(data)
		received <- n
	}()
	port, err := c.Connect(ctx, listener.Addr().(*net.TCPAddr).Port)
	if err != nil {
		t.Fatal(err)
	}
	defer port.Close()
	if _, err = port.Write([]byte("private port bytes")); err != nil {
		t.Fatal(err)
	}
	if _, err = port.Read(make([]byte, 32)); err == nil {
		t.Fatal("denied port input left stream usable")
	}
	select {
	case n := <-received:
		if n != 0 {
			t.Fatal("denied bytes reached the target port")
		}
	case <-ctx.Done():
		t.Fatal("port did not close")
	}
}
