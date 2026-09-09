package tests

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"github.com/aiomni/dune/internal/config"
	"github.com/aiomni/dune/internal/tmux"
	"github.com/aiomni/dune/pkg/api"
	"github.com/aiomni/dune/pkg/sdk"
	"io"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"
)

var binary string

func TestMain(m *testing.M) {
	if os.Getenv("DUNE_TMUX") == "" {
		path, _ := filepath.Abs("../bin/tmux")
		os.Setenv("DUNE_TMUX", path)
	}
	dir, e := os.MkdirTemp("", "dune-e2e-bin-")
	if e != nil {
		panic(e)
	}
	binary = filepath.Join(dir, "dune")
	cmd := exec.Command("go", "build", "-race", "-o", binary, "../cmd/dune")
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	if e = cmd.Run(); e != nil {
		panic(e)
	}
	code := m.Run()
	os.RemoveAll(dir)
	os.Exit(code)
}

type harness struct {
	t         *testing.T
	dir, path string
	c         config.Config
	client    *sdk.Client
	ctx       context.Context
	cancel    context.CancelFunc
	cmds      map[string]*exec.Cmd
	log       *os.File
}

func start(t *testing.T) *harness {
	t.Helper()
	dir := t.TempDir()
	ln, e := net.Listen("tcp", "127.0.0.1:0")
	must(t, e)
	addr := ln.Addr().String()
	ln.Close()
	path := filepath.Join(dir, "config.yaml")
	must(t, config.Init(path, addr))
	c, e := config.Load(path)
	must(t, e)
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	h := &harness{t: t, dir: dir, path: path, c: c, ctx: ctx, cancel: cancel, cmds: make(map[string]*exec.Cmd)}
	h.log, e = os.Create(filepath.Join(dir, "service.log"))
	must(t, e)
	h.startProcess("gateway")
	h.startProcess("fabricd")
	t.Cleanup(func() {
		if h.client != nil {
			h.client.Close()
		}
		h.stopProcess("fabricd", syscall.SIGTERM)
		h.stopProcess("gateway", syscall.SIGTERM)
		if server, err := tmux.Open(c.SessionDir); err == nil {
			_ = server.Close()
		}
		cancel()
		h.log.Close()
		logBytes, _ := os.ReadFile(h.log.Name())
		if strings.Contains(string(logBytes), "DATA RACE") {
			t.Error("service data race detected")
		}
		if t.Failed() {
			b, _ := os.ReadFile(h.log.Name())
			t.Log(string(b))
		}
	})
	h.reconnect()
	return h
}

func (h *harness) startProcess(name string) {
	h.t.Helper()
	cmd := exec.Command(binary, "--config", h.path, name)
	cmd.Stdout = h.log
	cmd.Stderr = h.log
	must(h.t, cmd.Start())
	h.cmds[name] = cmd
}

func (h *harness) stopProcess(name string, signal os.Signal) {
	h.t.Helper()
	cmd := h.cmds[name]
	if cmd == nil {
		return
	}
	_ = cmd.Process.Signal(signal)
	done := make(chan struct{})
	go func() {
		_ = cmd.Wait()
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(8 * time.Second):
		_ = cmd.Process.Kill()
		<-done
	}
	delete(h.cmds, name)
}

func (h *harness) restartProcess(name string) {
	h.t.Helper()
	h.stopProcess(name, syscall.SIGKILL)
	h.startProcess(name)
}

func (h *harness) reconnect() {
	h.t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		tc, e := h.c.TLS()
		must(h.t, e)
		c, e := sdk.Dial(h.ctx, sdk.Options{Gateway: h.c.Gateway, Token: h.c.Token, Target: h.c.Target, TLSConfig: tc})
		if e == nil {
			h.client = c
			return
		}
		time.Sleep(50 * time.Millisecond)
	}
	h.t.Fatal("fabricd did not register")
}
func must(t *testing.T, e error) {
	t.Helper()
	if e != nil {
		t.Fatal(e)
	}
}
func (h *harness) exec(args ...string) api.ExecResult {
	h.t.Helper()
	r, e := h.client.Exec(h.ctx, api.Exec{Command: api.Command{Argv: args}, WorkingDirectory: h.dir})
	must(h.t, e)
	return r
}
func profile(dir, adapter string, args ...string) api.Profile {
	return api.Profile{Version: 1, Kind: "agent", WorkingDirectory: dir, Adapter: adapter, Start: api.Command{Argv: args}}
}
func receive(t *testing.T, s *sdk.Stream, kind, contains string) {
	t.Helper()
	for i := 0; i < 50; i++ {
		m, e := s.Recv()
		must(t, e)
		if m.Kind == kind && strings.Contains(string(m.Data), contains) {
			return
		}
	}
	t.Fatalf("event %s %q missing", kind, contains)
}
func TestExecProfilePTY(t *testing.T) {
	h := start(t)
	r := h.exec("/bin/sh", "-c", "printf hello; printf diagnostic >&2; exit 7")
	if r.Stdout != "hello" || r.Stderr != "diagnostic" || r.ExitCode != 7 {
		t.Fatalf("%+v", r)
	}
	r, e := h.client.Exec(h.ctx, api.Exec{Command: api.Command{Argv: []string{"/bin/sleep", "10"}, TimeoutSeconds: 1}, WorkingDirectory: h.dir})
	must(t, e)
	if !r.TimedOut {
		t.Fatal("timeout missing")
	}
	p := profile(h.dir, "pty", "/bin/sh")
	p.Env = map[string]string{"DUNE_TEST": "value"}
	p.Setup.Steps = []api.Command{{Run: "printf ready > prepared", Shell: "/bin/sh"}}
	rt, s, e := h.client.Start(h.ctx, p)
	must(t, e)
	defer s.Close()
	b, e := os.ReadFile(filepath.Join(h.dir, "prepared"))
	must(t, e)
	if string(b) != "ready" {
		t.Fatal("setup")
	}
	_, e = s.Input([]byte("printf 'RESULT:%s\\n' \"$DUNE_TEST\"\n"))
	must(t, e)
	receive(t, s, "data", "RESULT:value")
	must(t, s.Resize(40, 100))
	_, e = s.Input([]byte("stty size\n"))
	must(t, e)
	receive(t, s, "data", "40 100")
	_, e = h.client.Attach(h.ctx, rt, false)
	if e == nil {
		t.Fatal("two input owners")
	}
	observer, e := h.client.Attach(h.ctx, rt, true)
	must(t, e)
	observer.Close()
	must(t, h.client.Stop(h.ctx, rt))
	receive(t, s, "exit", "")
	list, e := h.client.List(h.ctx)
	must(t, e)
	if len(list) != 0 {
		t.Fatal(list)
	}
	p.Setup.Steps = []api.Command{{Argv: []string{"/bin/sh", "-c", "exit 2"}}, {Argv: []string{"touch", "never"}}}
	_, _, e = h.client.Start(h.ctx, p)
	if e == nil {
		t.Fatal("setup failure expected")
	}
	if _, e = os.Stat(filepath.Join(h.dir, "never")); !os.IsNotExist(e) {
		t.Fatal("continued failed setup")
	}
}
func TestACP(t *testing.T) {
	h := start(t)
	script := `import sys,json
print('diagnostic',file=sys.stderr,flush=True)
for line in sys.stdin:
 m=json.loads(line)
 if m.get('method')=='bad': print('not-json',flush=True); continue
 if m.get('method')=='permission': print(json.dumps({'jsonrpc':'2.0','id':'perm','method':'session/request_permission','params':{}}),flush=True)
 else: print(json.dumps(m),flush=True)
`
	path := filepath.Join(h.dir, "acp.py")
	must(t, os.WriteFile(path, []byte(script), 0600))
	rt, s, e := h.client.Start(h.ctx, profile(h.dir, "acp", "python3", "-u", path))
	must(t, e)
	defer s.Close()
	defer h.client.Stop(h.ctx, rt)
	receive(t, s, "stderr", "diagnostic")
	for _, line := range []string{`{"jsonrpc":"2.0","id":1,"method":"unknown","params":{}}`, `{"jsonrpc":"2.0","method":"notification"}`, `{"jsonrpc":"2.0","id":"permission","method":"permission"}`, `{"jsonrpc":"2.0","id":"perm","result":{"allowed":true}}`} {
		_, e = s.Input([]byte(line))
		must(t, e)
		receive(t, s, "data", "jsonrpc")
	}
	_, e = s.Input([]byte(`{"jsonrpc":"2.0","method":"bad"}`))
	must(t, e)
	for {
		_, e = s.Recv()
		if e != nil {
			if !strings.Contains(e.Error(), "INVALID_ACP") {
				t.Fatal(e)
			}
			break
		}
	}
}
func TestFilesUploads(t *testing.T) {
	h := start(t)
	path := filepath.Join(h.dir, "file")
	must(t, h.client.Files(h.ctx, api.File{Action: "write", Path: path, Data: []byte("abc")}, nil))
	if e := h.client.Files(h.ctx, api.File{Action: "write", Path: path, Data: []byte("oops")}, nil); e == nil {
		t.Fatal("overwrite")
	}
	var info api.FileInfo
	must(t, h.client.Files(h.ctx, api.File{Action: "stat", Path: path}, &info))
	if info.Size != 3 {
		t.Fatal(info)
	}
	var read struct{ Data []byte }
	must(t, h.client.Files(h.ctx, api.File{Action: "read", Path: path, Length: 3}, &read))
	if string(read.Data) != "abc" {
		t.Fatal(read)
	}
	must(t, h.client.Files(h.ctx, api.File{Action: "mkdir", Path: filepath.Join(h.dir, "sub")}, nil))
	var entries []api.FileInfo
	must(t, h.client.Files(h.ctx, api.File{Action: "list", Path: h.dir}, &entries))
	must(t, h.client.Files(h.ctx, api.File{Action: "rename", Path: path, Destination: path + "2", Overwrite: true}, nil))
	must(t, h.client.Files(h.ctx, api.File{Action: "remove", Path: path + "2"}, nil))
	data := []byte(strings.Repeat("abcdefgh", 10000))
	sum := sha256.Sum256(data)
	u, e := h.client.Upload(h.ctx, api.Upload{Action: "create", Path: path, Size: int64(len(data)), SHA256: hex.EncodeToString(sum[:])})
	must(t, e)
	id := u.ID
	u, e = h.client.Upload(h.ctx, api.Upload{Action: "chunk", ID: id, Data: data[:100], Offset: 0})
	must(t, e)
	h.client.Close()
	h.reconnect()
	u, e = h.client.Upload(h.ctx, api.Upload{Action: "query", ID: id})
	must(t, e)
	if u.Offset != 100 {
		t.Fatal(u)
	}
	_, e = h.client.Upload(h.ctx, api.Upload{Action: "chunk", ID: id, Data: data[:10], Offset: 0})
	if e == nil {
		t.Fatal("offset conflict")
	}
	_, e = h.client.Upload(h.ctx, api.Upload{Action: "chunk", ID: id, Data: data[:10], Offset: 100, ChunkSHA256: strings.Repeat("0", 64)})
	if e == nil {
		t.Fatal("chunk hash conflict")
	}
	for u.Offset < int64(len(data)) {
		end := u.Offset + 32768
		if end > int64(len(data)) {
			end = int64(len(data))
		}
		u, e = h.client.Upload(h.ctx, api.Upload{Action: "chunk", ID: id, Offset: u.Offset, Data: data[u.Offset:end]})
		must(t, e)
	}
	u, e = h.client.Upload(h.ctx, api.Upload{Action: "commit", ID: id})
	must(t, e)
	if !u.Committed {
		t.Fatal(u)
	}
	_, e = h.client.Upload(h.ctx, api.Upload{Action: "commit", ID: id})
	must(t, e)
	b, e := os.ReadFile(path)
	must(t, e)
	if string(b) != string(data) {
		t.Fatal("upload content")
	}
	for _, action := range []string{"cancel", "expire", "bad-hash"} {
		u, e = h.client.Upload(h.ctx, api.Upload{Action: "create", Path: path + action, Size: 0, SHA256: strings.Repeat("0", 64), TTLSeconds: 1})
		must(t, e)
		if action == "cancel" {
			_, e = h.client.Upload(h.ctx, api.Upload{Action: "cancel", ID: u.ID})
			must(t, e)
		} else if action == "expire" {
			time.Sleep(1100 * time.Millisecond)
			_, e = h.client.Upload(h.ctx, api.Upload{Action: "query", ID: u.ID})
			if e == nil {
				t.Fatal("expiry")
			}
		} else {
			_, e = h.client.Upload(h.ctx, api.Upload{Action: "commit", ID: u.ID})
			if e == nil {
				t.Fatal("hash mismatch")
			}
		}
	}
}
func TestPortsAndConcurrency(t *testing.T) {
	h := start(t)
	ln, e := net.Listen("tcp", "127.0.0.1:0")
	must(t, e)
	defer ln.Close()
	go func() {
		for {
			c, e := ln.Accept()
			if e != nil {
				return
			}
			go func() {
				defer c.Close()
				b, e := io.ReadAll(io.LimitReader(c, 2*1024*1024))
				if e == nil {
					c.Write(b)
				}
			}()
		}
	}()
	port := ln.Addr().(*net.TCPAddr).Port
	var wg sync.WaitGroup
	for i := 0; i < 3; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			ctx, cancel := context.WithTimeout(h.ctx, 10*time.Second)
			defer cancel()
			p, e := h.client.Connect(ctx, port)
			if e != nil {
				t.Error(e)
				return
			}
			defer p.Close()
			b := strings.Repeat("hello", 200000)
			_, e = p.Write([]byte(b))
			if e != nil {
				t.Error(e)
				return
			}
			if e = p.CloseWrite(); e != nil {
				t.Error(e)
				return
			}
			out, e := io.ReadAll(p)
			if e != nil || string(out) != b {
				t.Errorf("port bytes %d %v", len(out), e)
				return
			}
			if e = p.Finish(); e != nil {
				t.Error(e)
			}
		}()
	}
	beg := time.Now()
	r := h.exec("/bin/echo", "interactive")
	if r.Stdout != "interactive\n" {
		t.Fatal(r)
	}
	t.Logf("Exec latency during 3 bulk connections: %s", time.Since(beg))
	wg.Wait()
	ln.Close()
	_, e = h.client.Connect(h.ctx, port)
	if e == nil {
		t.Fatal("refusal expected")
	}
}
func TestAuthDedup(t *testing.T) {
	h := start(t)
	tc, e := h.c.TLS()
	must(t, e)
	for _, token := range []string{"", "wrong"} {
		_, e = sdk.Dial(h.ctx, sdk.Options{Gateway: h.c.Gateway, Token: token, Target: h.c.Target, TLSConfig: tc})
		if e == nil {
			t.Fatal("invalid token accepted")
		}
	}
	a := api.Exec{Command: api.Command{Argv: []string{"/bin/sh", "-c", "printf x >> dedup"}}, WorkingDirectory: h.dir}
	var r api.ExecResult
	for i := 0; i < 2; i++ {
		must(t, h.client.CallID(h.ctx, "exec", "same-id", a, &r, nil))
	}
	b, e := os.ReadFile(filepath.Join(h.dir, "dedup"))
	must(t, e)
	if string(b) != "x" {
		t.Fatal("duplicate executed")
	}
	a.Argv = []string{"/bin/true"}
	if e = h.client.CallID(h.ctx, "exec", "same-id", a, &r, nil); e == nil {
		t.Fatal("conflict not rejected")
	}
}
func TestRestart(t *testing.T) {
	h := start(t)
	pidfile := filepath.Join(h.dir, "agent.pid")
	rt, s, e := h.client.Start(h.ctx, profile(h.dir, "pty", "/bin/sh", "-c", "echo $$ > "+pidfile+"; while :; do sleep 1; done"))
	must(t, e)
	defer s.Close()
	for i := 0; i < 50; i++ {
		if _, e = os.Stat(pidfile); e == nil {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	old := h.client.Binding
	h.restartProcess("gateway")
	for e == nil {
		_, e = s.Recv()
	}
	h.client.Close()
	h.reconnect()
	if h.client.Binding.Incarnation != old.Incarnation {
		t.Fatal("Gateway restart lost daemon")
	}
	_, e = h.client.Get(h.ctx, rt)
	must(t, e)
	sum := sha256.Sum256(nil)
	u, e := h.client.Upload(h.ctx, api.Upload{Action: "create", Path: filepath.Join(h.dir, "upload"), SHA256: hex.EncodeToString(sum[:])})
	must(t, e)
	h.restartProcess("fabricd")
	h.client.Close()
	time.Sleep(400 * time.Millisecond)
	h.reconnect()
	if h.client.Binding.Incarnation == old.Incarnation {
		t.Fatal("incarnation unchanged")
	}
	_, e = h.client.Get(h.ctx, rt)
	must(t, e)
	_, e = h.client.Upload(h.ctx, api.Upload{Action: "query", ID: u.ID})
	if e == nil {
		t.Fatal("stale upload accepted")
	}
	list, e := h.client.List(h.ctx)
	must(t, e)
	if len(list) != 1 || list[0].ID != rt.ID {
		t.Fatal("tmux runtime lost", list)
	}
	b, e := os.ReadFile(pidfile)
	must(t, e)
	var pid int
	fmt.Sscanf(string(b), "%d", &pid)
	if syscall.Kill(pid, 0) != nil {
		t.Fatal("tmux child did not survive fabricd restart")
	}
	leftovers, _ := filepath.Glob(filepath.Join(h.dir, ".dune-upload-*"))
	if len(leftovers) != 0 {
		t.Fatal("orphan upload files", leftovers)
	}
	must(t, h.client.Stop(h.ctx, rt))
}
func TestGitBasics(t *testing.T) {
	h := start(t)
	for _, args := range [][]string{{"git", "init", "-b", "main"}, {"git", "config", "user.email", "dune@example.test"}, {"git", "config", "user.name", "Dune Test"}} {
		if r := h.exec(args...); r.ExitCode != 0 {
			t.Fatal(r)
		}
	}
	git := func(a api.Git) api.GitResult {
		t.Helper()
		a.Directory = h.dir
		r, e := h.client.Git(h.ctx, a)
		must(t, e)
		if r.ExitCode != 0 {
			t.Fatalf("%s: %+v", a.Action, r)
		}
		return r
	}
	must(t, os.WriteFile(filepath.Join(h.dir, "a"), []byte("one\ntwo\nthree\n"), 0600))
	git(api.Git{Action: "stage", Paths: []string{"a"}})
	git(api.Git{Action: "commit", Message: "initial"})
	must(t, os.WriteFile(filepath.Join(h.dir, "a"), []byte("one\nchanged\nthree\n"), 0600))
	r := git(api.Git{Action: "status"})
	if len(r.Entries) == 0 || r.Entries[0].Path != "a" {
		t.Fatal(r)
	}
	patch := git(api.Git{Action: "diff"}).Stdout
	git(api.Git{Action: "stage", Patch: patch})
	git(api.Git{Action: "unstage", Patch: patch})
	git(api.Git{Action: "discard", Patch: patch})
	git(api.Git{Action: "log"})
	git(api.Git{Action: "show"})
	git(api.Git{Action: "branch", Name: "feature"})
	git(api.Git{Action: "checkout", Ref: "feature"})
	must(t, os.WriteFile(filepath.Join(h.dir, "a"), []byte("stash\n"), 0600))
	git(api.Git{Action: "stash", Mode: "push"})
	git(api.Git{Action: "stash", Mode: "list"})
	git(api.Git{Action: "stash", Mode: "apply"})
	git(api.Git{Action: "discard", Paths: []string{"a"}})
	git(api.Git{Action: "stash", Mode: "pop"})
	git(api.Git{Action: "stage", Paths: []string{"a"}})
	git(api.Git{Action: "unstage", Paths: []string{"a"}})
	git(api.Git{Action: "stage", Paths: []string{"a"}})
	git(api.Git{Action: "commit", Message: "feature"})
	git(api.Git{Action: "amend", Message: "amended"})
	must(t, os.WriteFile(filepath.Join(h.dir, "a"), []byte("drop stash\n"), 0600))
	git(api.Git{Action: "stash", Mode: "push"})
	git(api.Git{Action: "stash", Mode: "drop"})
	git(api.Git{Action: "checkout", Create: true, Name: "scratch"})
	git(api.Git{Action: "checkout", Ref: "main"})
	git(api.Git{Action: "merge", Ref: "feature"})
	git(api.Git{Action: "rebase", Ref: "main"})
	git(api.Git{Action: "conflicts"})
	remote := filepath.Join(t.TempDir(), "remote.git")
	if r := h.exec("git", "init", "--bare", remote); r.ExitCode != 0 {
		t.Fatal(r)
	}
	git(api.Git{Action: "push", Remote: remote, Ref: "main"})
	git(api.Git{Action: "fetch", Remote: remote, Ref: "main"})
	git(api.Git{Action: "pull", Remote: remote, Ref: "main"})
}
