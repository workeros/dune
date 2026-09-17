package host

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/aiomni/dune/internal/agentintegration"
	"github.com/aiomni/dune/internal/mcpbridge"
	"github.com/aiomni/dune/pkg/agents"
	"github.com/aiomni/dune/pkg/api"
	"github.com/aiomni/dune/pkg/runner"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// This protocol fixture checks the injected command and runs the actual bridge.
// CLI-specific configuration parsing also needs separate vendor acceptance.
func exerciseHostNativeMCP(args []string) bool {
	if len(args) != 2 {
		return false
	}
	helper, dir := os.Getenv(agentintegration.HelperEnv), os.Getenv(agentintegration.SessionDirEnv)
	if os.Getenv(mcpbridge.TokenEnv) != "" || os.Getenv(mcpbridge.URLEnv) != "" {
		return false
	}
	switch args[0] {
	case "--mcp-config":
		var settings struct {
			Servers map[string]struct {
				Command string
				Args    []string
				Env     map[string]string
			} `json:"mcpServers"`
		}
		if json.Unmarshal([]byte(args[1]), &settings) != nil {
			return false
		}
		server := settings.Servers["dune-agents"]
		if server.Command != helper || len(server.Args) != 1 || server.Args[0] != mcpbridge.Command || server.Env[agentintegration.SessionDirEnv] != dir {
			return false
		}
	case "-c":
		if !strings.HasPrefix(args[1], "mcp_servers.dune-agents={") || !strings.Contains(args[1], "command="+strconv.Quote(helper)) || !strings.Contains(args[1], strconv.Quote(mcpbridge.Command)) || !strings.Contains(args[1], agentintegration.SessionDirEnv+"="+strconv.Quote(dir)) {
			return false
		}
	default:
		return false
	}
	filename := os.Getenv("DUNE_HOST_NATIVE_MCP_RESULT")
	if filename == "" {
		return true
	}
	ctx, cancel := context.WithTimeout(context.Background(), 8*time.Second)
	defer cancel()
	command := exec.CommandContext(ctx, helper, mcpbridge.Command)
	command.Env = os.Environ()
	client, err := mcp.NewClient(&mcp.Implementation{Name: "native-host-fixture", Version: "1"}, nil).Connect(ctx, &mcp.CommandTransport{Command: command, TerminateDuration: time.Second}, nil)
	if err != nil {
		return false
	}
	defer client.Close()
	result, err := client.CallTool(ctx, &mcp.CallToolParams{Name: "agents_list", Arguments: map[string]any{}})
	if err != nil || result.IsError {
		return false
	}
	file, err := os.OpenFile(filename, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0600)
	if err != nil {
		return false
	}
	defer file.Close()
	_, err = file.WriteString("ok\n")
	return err == nil
}

func TestPTYMCPInjectionAuthenticatesAndRotatesOnResume(t *testing.T) {
	for _, agent := range []string{"claude", "codex"} {
		t.Run(agent, func(t *testing.T) {
			f := openExecutorFixture(t)
			var mu sync.Mutex
			var received []string
			host := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				mu.Lock()
				received = append(received, strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer "))
				mu.Unlock()
				f.app.ServeHTTP(w, r)
			}))
			defer host.Close()
			f.app.agentMCPURL = host.URL + "/api/v1/agent-mcp"
			profile := nativeRecoveryProfile(t, f, agent)
			profile.Env["DUNE_HOST_NATIVE_MCP_RESULT"] = filepath.Join(f.workspace, "mcp-calls")
			started, err := f.app.AgentLauncher().Start(t.Context(), f.agentScope(), agents.StartRequest{Binding: f.binding, Custom: &profile})
			if err != nil || started.Runtime == nil || started.Session == nil {
				t.Fatal("native MCP launch failed", err)
			}
			session := awaitNativeIndex(t, f, started.Session.ID)
			waitLaunchFile(t, profile.Env["DUNE_HOST_NATIVE_MCP_RESULT"], "ok\n")
			mu.Lock()
			token := received[0]
			mu.Unlock()
			credential, err := f.app.store.ReadAgentCredential(t.Context(), token, "")
			if err != nil || credential.Target.Runtime.ID != started.Runtime.ID || credential.SessionID != session.ID {
				t.Fatal("MCP credential preceded or mismatched Runtime binding", err)
			}
			stored, err := f.app.store.AgentSession(t.Context(), f.owner, session.ID)
			if err != nil || !stored.Launch.Profile.RequireAgentMCP || bytes.Contains(api.Payload(stored.Launch), []byte(token)) || bytes.Contains(api.Payload(started), []byte(token)) {
				t.Fatal("native credential persisted in launch snapshot or public result")
			}
			connection, closeConnection, err := (&runnerExecutor{app: f.app}).connect(t.Context(), f.principal, f.owner, f.binding, "runtime.stop")
			if err != nil {
				t.Fatal(err)
			}
			defer closeConnection()
			if err := connection.Stop(t.Context(), *started.Runtime); err != nil {
				t.Fatal(err)
			}
			request := agents.ResumeRequest{SessionID: session.ID, Revision: session.Revision}
			resumed, err := f.app.AgentRestorer().Resume(t.Context(), f.agentScope(), request)
			if err != nil || resumed.Session == nil || resumed.Session.Attempt.State != "ready" {
				t.Fatal("native resume could not authenticate MCP", err)
			}
			waitLaunchFile(t, profile.Env["DUNE_HOST_NATIVE_MCP_RESULT"], "ok\nok\n")
			mu.Lock()
			nextToken := received[len(received)-1]
			mu.Unlock()
			if nextToken == token {
				t.Fatal("resume reused old process credential")
			}
			if _, err := f.app.store.ReadAgentCredential(t.Context(), token, ""); err == nil {
				t.Fatal("old native credential remained usable")
			}
			credential, err = f.app.store.ReadAgentCredential(t.Context(), nextToken, "")
			if err != nil || credential.Target.Runtime.ID != resumed.Runtime.ID {
				t.Fatal("wrong recovered MCP Runtime", err)
			}
			duplicate, err := f.app.AgentRestorer().Resume(t.Context(), f.agentScope(), request)
			if err != nil || duplicate.Runtime == nil || duplicate.Runtime.ID != resumed.Runtime.ID {
				t.Fatal("duplicate request did not follow native resume", err)
			}
			if _, err := f.app.store.ReadAgentCredential(t.Context(), nextToken, ""); err != nil {
				t.Fatal("duplicate resume rotated credential", err)
			}
			waitLaunchFile(t, profile.Env["DUNE_HOST_NATIVE_MCP_RESULT"], "ok\nok\n")
		})
	}
}

func TestPTYMCPConfigurationFailureKeepsOneDiscoverableRuntime(t *testing.T) {
	f := openExecutorFixture(t)
	f.app.agentMCPURL = "https://host.test/mcp?invalid=query"
	profile := nativeRecoveryProfile(t, f, "claude")
	result, err := f.app.AgentLauncher().Start(t.Context(), f.agentScope(), agents.StartRequest{Binding: f.binding, Custom: &profile})
	if errorCode(err) != "MCP_CONFIGURATION_FAILED" || result.Runtime == nil || result.Session == nil || result.AgentRef == "" {
		t.Fatal("native configuration failure lost Runtime", err)
	}
	page, err := f.app.AgentDirectory().List(t.Context(), f.agentScope(), runner.Query{})
	if err != nil || len(page.Items) != 1 || page.Items[0].Target.Runtime.ID != result.Runtime.ID {
		t.Fatal("MCP failure started another Runtime or lost the original", err)
	}
	if result.Operation != nil {
		t.Fatal("PTY MCP configuration invented an ACP operation")
	}
}
