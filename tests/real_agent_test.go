package tests

import (
	"encoding/json"
	"fmt"
	"github.com/aiomni/dune/internal/tmux"
	"github.com/aiomni/dune/pkg/api"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// Opt in because this requires the user's already configured Agent account.
func TestRealAgentPTY(t *testing.T) {
	if os.Getenv("DUNE_REAL_AGENT") != "1" {
		t.Skip("set DUNE_REAL_AGENT=1 to run configured Codex/Claude")
	}
	h := start(t)
	name := os.Getenv("DUNE_PTY_AGENT")
	if name == "" {
		name = "codex"
	}
	agent, e := exec.LookPath(name)
	must(t, e)
	work := filepath.Join(h.dir, "task")
	must(t, os.Mkdir(work, 0700))
	must(t, os.WriteFile(filepath.Join(work, "check.py"), []byte("from arithmetic import add\nassert add(2, 3) == 5\nassert add(-7, 4) == -3\nprint('DUNE_REAL_AGENT_CHECK_OK')\n"), 0600))
	prompt := "In this directory create arithmetic.py defining add(a, b) that returns their sum. Run python3 check.py and confirm it passes. Only modify arithmetic.py; do not access other directories."
	argv := []string{agent, "exec", "--ephemeral", "--skip-git-repo-check", "--sandbox", "workspace-write", prompt}
	if name == "claude" {
		argv = []string{agent, "-p", prompt, "--allowedTools", "Write,Bash,Read", "--max-turns", "8"}
	}
	rt, s, e := testStartProfile(h.client, h.ctx, profile(work, "pty", argv...))
	must(t, e)
	defer s.Close()
	defer testStopRuntime(h.client, h.ctx, rt)
	var transcript strings.Builder
	for {
		m, e := s.Recv()
		must(t, e)
		if m.Kind == "data" {
			if transcript.Len() < 16000 {
				transcript.Write(m.Data)
			}
		}
		if m.Kind == "exit" {
			var code int
			must(t, json.Unmarshal(m.Payload, &code))
			t.Log("Agent output:", transcript.String())
			if code != 0 {
				t.Fatalf("Agent exit %d", code)
			}
			break
		}
	}
	r, e := h.client.Exec(h.ctx, api.Exec{Command: api.Command{Argv: []string{"python3", "check.py"}}, WorkingDirectory: work})
	must(t, e)
	if r.ExitCode != 0 || !strings.Contains(r.Stdout, "DUNE_REAL_AGENT_CHECK_OK") {
		t.Fatalf("independent verification: %+v", r)
	}
	t.Logf("Real PTY task passed: %s; independent check: %s", agent, r.Stdout)
}
func TestRealAgentACP(t *testing.T) {
	if os.Getenv("DUNE_REAL_AGENT") != "1" {
		t.Skip("set DUNE_REAL_AGENT=1 for installed Gemini ACP")
	}
	h := start(t)
	argv := realACPCommand(t)
	agent := argv[0]
	work := filepath.Join(h.dir, "acp-task")
	must(t, os.Mkdir(work, 0700))
	rt, s, e := testStartProfile(h.client, h.ctx, profile(work, "acp", argv...))
	must(t, e)
	defer s.Close()
	defer testStopRuntime(h.client, h.ctx, rt)
	raw := newRawTestClient(t, h, rt)
	s.Close()
	e = raw.writeLine(`{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":1,"clientCapabilities":{"fs":{"readTextFile":false,"writeTextFile":false},"terminal":false},"clientInfo":{"name":"dune-e2e","version":"1"}}}`)
	must(t, e)
	for {
		line := raw.nextLine(t, "stdout")
		var reply struct {
			ID     int             `json:"id"`
			Result json.RawMessage `json:"result"`
			Error  json.RawMessage `json:"error"`
		}
		must(t, json.Unmarshal(line, &reply))
		if reply.ID == 1 {
			if len(reply.Result) == 0 {
				t.Fatalf("initialize rejected: %s", line)
			}
			assertRealACPIsolation(t, h, rt)
			t.Logf("Real ACP initialize passed: %s", agent)
			must(t, testStopRuntime(h.client, h.ctx, rt))
			must(t, testForgetRuntime(h.client, h.ctx, rt))
			return
		}
	}
}

// Both entry points use the same actual stdio Agent and submit no model prompt.
func realACPCommand(t *testing.T) []string {
	t.Helper()
	argv := []string{"gemini", "--acp"}
	if configured := os.Getenv("DUNE_REAL_ACP_COMMAND"); configured != "" {
		must(t, json.Unmarshal([]byte(configured), &argv))
	}
	if len(argv) == 0 {
		t.Fatal("real ACP command is empty")
	}
	agent, err := exec.LookPath(argv[0])
	must(t, err)
	argv[0] = agent
	return argv
}

func TestRealAgentManagedACP(t *testing.T) {
	if os.Getenv("DUNE_REAL_AGENT") != "1" {
		t.Skip("set DUNE_REAL_AGENT=1 for configured stdio ACP Agent")
	}
	h := start(t)
	p := profile(h.dir, "acp", realACPCommand(t)...)
	p.ManagedACP = true
	runtime, stream, err := testStartProfile(h.client, h.ctx, p)
	must(t, err)
	stream.Close()
	defer func() { _ = testStopRuntime(h.client, h.ctx, runtime) }()
	state, err := h.client.ACPState(h.ctx, runtime)
	must(t, err)
	if !state.Ready {
		t.Fatal("actual Agent initialization did not complete")
	}
	assertRealACPIsolation(t, h, runtime)
	t.Logf("Real managed ACP initialize passed: %s", p.Start.Argv[0])
	must(t, testStopRuntime(h.client, h.ctx, runtime))
	must(t, testForgetRuntime(h.client, h.ctx, runtime))
}

func assertRealACPIsolation(t *testing.T, h *harness, original api.Runtime) {
	t.Helper()
	server, err := tmux.Open(filepath.Join(h.c.SessionDir, "acp"))
	must(t, err)
	output, err := exec.Command(os.Getenv("DUNE_TMUX"), "-S", server.Socket, "capture-pane", "-p", "-t", "acp-"+original.ID+":0.0").Output()
	must(t, err)
	if strings.TrimSpace(string(output)) != "" {
		t.Fatal("ACP output entered tmux PTY")
	}
	current, err := h.client.Get(h.ctx, original)
	must(t, err)
	if current.ACPHost == nil {
		t.Fatal("missing original host identity")
	}
	if runtime.GOOS != "linux" {
		return
	}
	terminal, err := os.Readlink(fmt.Sprintf("/proc/%d/fd/3", current.ACPHost.HostPID))
	must(t, err)
	if terminal != "/dev/tty" {
		t.Fatal("host does not retain fd3 terminal", terminal)
	}
	for _, pid := range []int{current.ACPHost.AgentPID, current.ACPHost.GroupID} {
		directory := fmt.Sprintf("/proc/%d/fd", pid)
		files, err := os.ReadDir(directory)
		must(t, err)
		for _, file := range files {
			path, _ := os.Readlink(filepath.Join(directory, file.Name()))
			if path == "/dev/tty" || strings.HasPrefix(path, "/dev/pts/") {
				t.Fatal("Agent/guardian inherited terminal descriptor")
			}
		}
	}
	t.Log("host owns fd3; Agent/guardian own no terminal; tmux pane is empty")
}
