package tests

import (
	"encoding/json"
	"github.com/aiomni/dune/pkg/api"
	"os"
	"os/exec"
	"path/filepath"
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
	rt, s, e := h.client.Start(h.ctx, profile(work, "pty", argv...))
	must(t, e)
	defer s.Close()
	defer h.client.Stop(h.ctx, rt)
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
	agent, e := exec.LookPath("gemini")
	must(t, e)
	work := filepath.Join(h.dir, "acp-task")
	must(t, os.Mkdir(work, 0700))
	rt, s, e := h.client.Start(h.ctx, profile(work, "acp", agent, "--acp"))
	must(t, e)
	defer s.Close()
	defer h.client.Stop(h.ctx, rt)
	_, e = s.Input([]byte(`{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":1,"clientCapabilities":{"fs":{"readTextFile":false,"writeTextFile":false},"terminal":false},"clientInfo":{"name":"dune-e2e","version":"1"}}}`))
	must(t, e)
	for {
		m, e := s.Recv()
		must(t, e)
		if m.Kind == "stderr" {
			t.Log("Agent diagnostic:", string(m.Data))
		}
		if m.Kind == "data" {
			var reply struct {
				ID     int             `json:"id"`
				Result json.RawMessage `json:"result"`
				Error  json.RawMessage `json:"error"`
			}
			must(t, json.Unmarshal(m.Data, &reply))
			if reply.ID == 1 {
				if len(reply.Result) == 0 {
					t.Fatalf("initialize rejected: %s", m.Data)
				}
				t.Logf("Real ACP initialize passed: %s --acp: %s", agent, m.Data)
				return
			}
		}
	}
}
