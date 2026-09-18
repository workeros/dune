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
	"strings"
	"testing"
	"time"

	"github.com/aiomni/dune/internal/mcpbridge"
	"github.com/aiomni/dune/pkg/agents"
	"github.com/aiomni/dune/pkg/api"
	"github.com/aiomni/dune/pkg/runner"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

type injectedMCPServer struct {
	Name    string                         `json:"name"`
	Type    string                         `json:"type,omitempty"`
	URL     string                         `json:"url,omitempty"`
	Command string                         `json:"command,omitempty"`
	Args    []string                       `json:"args,omitempty"`
	Env     []struct{ Name, Value string } `json:"env,omitempty"`
	Headers []struct{ Name, Value string } `json:"headers,omitempty"`
}

func (s injectedMCPServer) token() string {
	for _, header := range s.Headers {
		if header.Name == "Authorization" {
			return strings.TrimPrefix(header.Value, "Bearer ")
		}
	}
	for _, env := range s.Env {
		if env.Name == mcpbridge.TokenEnv {
			return env.Value
		}
	}
	return ""
}

// Called inside the native ACP fixture, with the same config an Agent receives.
// The optional check uses a real HTTP client or spawns the actual stdio bridge.
func exerciseInjectedMCP(servers []injectedMCPServer) bool {
	filename := os.Getenv("DUNE_HOST_FAKE_ACP_MCP_CONFIG")
	if filename == "" {
		return true
	}
	if len(servers) != 1 || servers[0].Name != "dune-agents" {
		return false
	}
	server := servers[0]
	encoded, _ := json.Marshal(server)
	if err := os.WriteFile(filename, encoded, 0600); err != nil {
		return false
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	var transport mcp.Transport
	if server.Type == "http" {
		transport = &mcp.StreamableClientTransport{Endpoint: server.URL, HTTPClient: &http.Client{Transport: mcpBearerTransport{token: server.token()}}, MaxRetries: -1, DisableStandaloneSSE: true}
	} else {
		command := exec.CommandContext(ctx, server.Command, server.Args...)
		command.Env = os.Environ()
		for _, env := range server.Env {
			command.Env = append(command.Env, env.Name+"="+env.Value)
		}
		transport = &mcp.CommandTransport{Command: command, TerminateDuration: time.Second}
	}
	client, err := mcp.NewClient(&mcp.Implementation{Name: "native-fixture", Version: "1"}, nil).Connect(ctx, transport, nil)
	if err != nil {
		return false
	}
	defer client.Close()
	result, err := client.CallTool(ctx, &mcp.CallToolParams{Name: "agents_list", Arguments: map[string]any{}})
	return err == nil && !result.IsError
}

func readInjectedMCP(t *testing.T, filename string) injectedMCPServer {
	t.Helper()
	data, err := os.ReadFile(filename)
	var server injectedMCPServer
	if err != nil || json.Unmarshal(data, &server) != nil || server.token() == "" {
		t.Fatal("native Agent did not receive a credential")
	}
	return server
}

func TestAgentMCPInjectionStartsSwitchesAndResumesWithUsableCredentials(t *testing.T) {
	for _, transport := range []string{"http", "stdio"} {
		t.Run(transport, func(t *testing.T) {
			f := openExecutorFixture(t)
			host := httptest.NewServer(f.app)
			defer host.Close()
			f.app.agentMCPURL = host.URL + "/api/v1/agent-mcp"
			filename := filepath.Join(f.workspace, "injected-config.json")
			environment := map[string]string{"DUNE_HOST_FAKE_ACP_MCP_CONFIG": filename}
			if transport == "http" {
				environment["DUNE_HOST_FAKE_ACP_MCP_HTTP"] = "1"
			}
			started, connection := directoryACPWithEnvironment(t, f, environment)
			injected := readInjectedMCP(t, filename)
			token := injected.token()
			credential, err := f.app.store.ReadAgentCredential(t.Context(), token, "")
			if err != nil || credential.Target.Runtime.ID != started.Runtime.ID {
				t.Fatal("injected credential was not bound to the recorded Runtime", err)
			}
			stored, err := f.app.store.AgentSession(t.Context(), f.owner, started.Session.ID)
			if err != nil || !stored.Launch.Profile.RequireAgentMCP || bytes.Contains(api.Payload(stored.Launch), []byte(token)) || bytes.Contains(api.Payload(started), []byte(token)) {
				t.Fatal("launch snapshot or public response contains the credential")
			}
			if _, err := connection.ConfigureAgentMCP(t.Context(), *started.Runtime, api.AgentMCP{URL: f.app.agentMCPURL, Token: token}); errorCode(err) != "CONFLICT" {
				t.Fatal("configured running credential could be replaced", err)
			}
			directoryAction(t, connection, *started.Runtime, api.ACPAction{Action: "load", SessionID: "another-native", Cwd: f.workspace})
			if readInjectedMCP(t, filename).token() != token {
				t.Fatal("native switch rotated the process credential")
			}
			// Return to the saved conversation, then explicitly recover its process.
			directoryAction(t, connection, *started.Runtime, api.ACPAction{Action: "load", SessionID: started.Session.Native.ID, Cwd: started.Session.Native.Cwd})
			current, err := f.app.AgentDirectory().Get(t.Context(), f.agentScope(), started.AgentRef)
			if err != nil {
				t.Fatal(err)
			}
			stopRecoveryRuntime(t, connection, *started.Runtime)
			resumed, err := f.app.AgentRestorer().Resume(t.Context(), f.agentScope(), agents.ResumeRequest{SessionID: current.Session.ID, Revision: current.Session.Revision})
			if err != nil || resumed.Operation == nil || resumed.Operation.State != "completed" {
				t.Fatal("resume could not use the new injected credential", err)
			}
			nextToken := readInjectedMCP(t, filename).token()
			if nextToken == token {
				t.Fatal("native recovery reused the old process credential")
			}
			if old, err := f.app.store.ReadAgentCredential(t.Context(), token, ""); err == nil && f.app.agentService().VerifyCaller(t.Context(), old.Scope, old.Target) == nil {
				t.Fatal("old credential remained valid after recovery")
			}
			currentCredential, err := f.app.store.ReadAgentCredential(t.Context(), nextToken, "")
			if err != nil || currentCredential.Target.Runtime.ID != resumed.Runtime.ID {
				t.Fatal("recovery credential has wrong Runtime", err)
			}
			// A repeated recovery request only follows the existing attempt.
			if _, err := f.app.AgentRestorer().Resume(t.Context(), f.agentScope(), agents.ResumeRequest{SessionID: current.Session.ID, Revision: current.Session.Revision}); err != nil {
				t.Fatal(err)
			}
			if readInjectedMCP(t, filename).token() != nextToken {
				t.Fatal("duplicate resume rotated the current credential")
			}
		})
	}
}

func TestAgentMCPInjectionFailureRetainsRuntimeWithoutRepeatingNativeStart(t *testing.T) {
	for _, failure := range []string{"configuration", "connection"} {
		t.Run(failure, func(t *testing.T) {
			f := openExecutorFixture(t)
			profile := directoryACPProfile(t, f)
			methods := filepath.Join(f.workspace, "methods")
			profile.Env["DUNE_HOST_FAKE_ACP_METHODS"] = methods
			code, newCount := "MCP_CONFIGURATION_FAILED", 0
			if failure == "configuration" {
				f.app.agentMCPURL = "https://host.test/mcp?invalid=query"
			} else {
				unavailable := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusServiceUnavailable) }))
				defer unavailable.Close()
				f.app.agentMCPURL = unavailable.URL + "/api/v1/agent-mcp"
				profile.Env["DUNE_HOST_FAKE_ACP_MCP_CONFIG"] = filepath.Join(f.workspace, "injected-config")
				profile.Env["DUNE_HOST_FAKE_ACP_MCP_HTTP"] = "1"
				code, newCount = "SESSION_FAILED", 1
			}
			result, err := f.app.AgentLauncher().Start(t.Context(), f.agentScope(), agents.StartRequest{Binding: f.binding, Custom: &profile})
			if errorCode(err) != code || result.Runtime == nil || result.Session == nil || result.AgentRef == "" {
				t.Fatal("MCP failure lost confirmed Runtime", err)
			}
			if newCount == 0 && result.Operation != nil || newCount == 1 && (result.Operation == nil || result.Operation.State != "failed") {
				t.Fatal("wrong native operation after MCP failure")
			}
			calls, err := os.ReadFile(methods)
			if err != nil || strings.Count(string(calls), "initialize:") != 1 || strings.Count(string(calls), "session/new:") != newCount {
				t.Fatal("MCP failure replayed startup")
			}
			items, err := f.app.AgentDirectory().List(t.Context(), f.agentScope(), runner.Query{})
			if err != nil || len(items.Items) != 1 || items.Items[0].Target.Runtime.ID != result.Runtime.ID {
				t.Fatal("partial Runtime no longer discoverable", err)
			}
		})
	}
}
