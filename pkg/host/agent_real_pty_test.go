package host

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/aiomni/dune/pkg/agents"
	"github.com/aiomni/dune/pkg/api"
)

// Opt in with an isolated, already authenticated Codex home. Ordinary tests
// never invoke a model or change the developer's native CLI configuration.
func TestRealNativeAgentMCPRecovery(t *testing.T) {
	if os.Getenv("DUNE_REAL_AGENT") != "1" || os.Getenv("DUNE_NATIVE_CODEX_HOME") == "" {
		t.Skip("set DUNE_REAL_AGENT=1 and DUNE_NATIVE_CODEX_HOME for native Codex acceptance")
	}
	program, err := exec.LookPath("codex")
	if err != nil {
		t.Fatal(err)
	}
	f := openExecutorFixtureFor(t, 3*time.Minute)
	var toolCalls atomic.Int32
	host := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		r.Body = io.NopCloser(bytes.NewReader(body))
		var rpc struct {
			Method string
			Params struct{ Name string }
		}
		if json.Unmarshal(body, &rpc) == nil && rpc.Method == "tools/call" && rpc.Params.Name == "agents_list" {
			toolCalls.Add(1)
		}
		f.app.ServeHTTP(w, r)
	}))
	defer host.Close()
	f.app.agentMCPURL = host.URL + "/api/v1/agent-mcp"
	profile := api.Profile{Version: 1, Kind: "agent", Adapter: "pty", WorkingDirectory: f.workspace, Start: api.Command{Argv: []string{program}}, Env: map[string]string{"CODEX_HOME": os.Getenv("DUNE_NATIVE_CODEX_HOME")}}
	started, err := f.app.AgentLauncher().Start(t.Context(), f.agentScope(), agents.StartRequest{Binding: f.binding, Custom: &profile})
	if err != nil {
		t.Fatal(err)
	}
	connection, closeConnection, err := (&runnerExecutor{app: f.app}).connect(t.Context(), f.principal, f.owner, f.binding, "runtime.get")
	if err != nil {
		t.Fatal(err)
	}
	defer closeConnection()
	trustedDirectory, trustedHooks, submitted := false, false, false
	for deadline := time.Now().Add(60 * time.Second); ; time.Sleep(250 * time.Millisecond) {
		current, err := connection.Get(t.Context(), *started.Runtime)
		if err != nil {
			t.Fatal(err)
		}
		if current.NativeSession != nil && toolCalls.Load() > 0 {
			t.Log("Native SessionStart and MCP tool call confirmed")
			break
		}
		snapshot, err := connection.CaptureTerminal(t.Context(), *started.Runtime)
		if err != nil {
			t.Fatal(err)
		}
		if !trustedDirectory && strings.Contains(snapshot.Content, "Do you trust the contents of this directory?") && strings.Contains(snapshot.Content, "1. Yes, continue") {
			operation, err := connection.PTYSendKeys(t.Context(), *started.Runtime, api.PTYKeys{Agent: "codex", Keys: []string{"Enter"}})
			if err != nil {
				t.Fatal(err)
			}
			if _, err := connection.WaitAgentOperation(t.Context(), *started.Runtime, api.AgentOperationWait{Ref: operation.Ref, TimeoutMS: 2000}); err != nil {
				t.Fatal(err)
			}
			trustedDirectory = true
		}
		if !trustedHooks && strings.Contains(snapshot.Content, "Hooks need review") && strings.Contains(snapshot.Content, "1 hook is new or changed.") && strings.Contains(snapshot.Content, "2. Trust all and continue") {
			// This isolated home has only the SessionStart command under test.
			operation, err := connection.PTYSendKeys(t.Context(), *started.Runtime, api.PTYKeys{Agent: "codex", Keys: []string{"Down", "Enter"}})
			if err != nil {
				t.Fatal(err)
			}
			if _, err := connection.WaitAgentOperation(t.Context(), *started.Runtime, api.AgentOperationWait{Ref: operation.Ref, TimeoutMS: 2000}); err != nil {
				t.Fatal(err)
			}
			trustedHooks = true
		}
		if !submitted && strings.Contains(snapshot.Content, "OpenAI Codex") && strings.Contains(snapshot.Content, "model:") {
			operation, err := connection.PTYPrompt(t.Context(), *started.Runtime, api.PTYPrompt{Agent: "codex", Text: "Use the dune-agents MCP agents_list tool exactly once, then reply DUNE_MCP_VERIFIED. Do not run shell commands or read/write files."})
			if err != nil {
				t.Fatal(err)
			}
			if _, err := connection.WaitAgentOperation(t.Context(), *started.Runtime, api.AgentOperationWait{Ref: operation.Ref, TimeoutMS: 2000}); err != nil {
				t.Fatal(err)
			}
			submitted = true
			t.Log("Native prompt delivered")
		}
		if strings.Contains(snapshot.Content, "hit your usage limit") {
			t.Fatalf("native Codex account usage is exhausted; SessionStart confirmed=%t, MCP tool calls=%d", current.NativeSession != nil, toolCalls.Load())
		}
		if time.Now().After(deadline) || current.State == "exited" {
			t.Log("MCP tool calls:", toolCalls.Load())
			t.Fatalf("Native acceptance incomplete (state %s):\n%s", current.State, snapshot.Content)
		}
	}
	session := awaitNativeIndex(t, f, started.Session.ID)
	if err := connection.Stop(t.Context(), *started.Runtime); err != nil {
		t.Fatal(err)
	}
	resumed, err := f.app.AgentRestorer().Resume(t.Context(), f.agentScope(), agents.ResumeRequest{SessionID: session.ID, Revision: session.Revision})
	if err != nil || resumed.Session == nil || resumed.Runtime == nil || resumed.Session.Attempt.State != "ready" || resumed.Session.Native == nil || resumed.Session.Native.ID != session.Native.ID {
		t.Fatal("native CLI did not confirm recovery of the saved session", err)
	}
	t.Log("Native CLI recovered the exact saved session")

}
