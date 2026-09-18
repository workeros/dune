package fabricd

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/aiomni/dune/pkg/api"
	"github.com/google/uuid"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// The fixture uses the actual native launch config and retained bridge binary.
// It only calls a local MCP server; it does not invoke a vendor or model.
func exerciseNativeMCP(args []string) bool {
	var config struct {
		Servers map[string]struct {
			Command string
			Args    []string
			Env     map[string]string
		} `json:"mcpServers"`
	}
	if len(args) != 2 || args[0] != "--mcp-config" || json.Unmarshal([]byte(args[1]), &config) != nil {
		return false
	}
	server, ok := config.Servers["dune-agents"]
	if !ok {
		return false
	}
	ctx, cancel := context.WithTimeout(context.Background(), 8*time.Second)
	defer cancel()
	command := exec.CommandContext(ctx, server.Command, server.Args...)
	command.Env = os.Environ()
	for key, value := range server.Env {
		command.Env = append(command.Env, key+"="+value)
	}
	client, err := mcp.NewClient(&mcp.Implementation{Name: "native-fixture", Version: "1"}, nil).Connect(ctx, &mcp.CommandTransport{Command: command, TerminateDuration: time.Second}, nil)
	if err != nil {
		return false
	}
	defer client.Close()
	result, err := client.CallTool(ctx, &mcp.CallToolParams{Name: "ping", Arguments: map[string]any{}})
	return err == nil && !result.IsError
}

func TestPTYMCPBridgeWaitsAndSurvivesFabricdRestart(t *testing.T) {
	dir := t.TempDir()
	engine, err := Open(t.Context(), filepath.Join(dir, "sessions"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { engine.Close(); _ = engine.tmux.Close() })
	connection, ctx := timeoutClient(t, engine)
	program, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	agent := filepath.Join(dir, "claude")
	data, err := os.ReadFile(program)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(agent, data, 0700); err != nil {
		t.Fatal(err)
	}
	eventPath := filepath.Join(dir, "event")
	writeEvent := func() {
		t.Helper()
		data, _ := json.Marshal(map[string]string{"hook_event_name": "SessionStart", "session_id": uuid.NewString(), "cwd": dir, "transcript_path": "/private/native.jsonl"})
		if err := os.WriteFile(eventPath, data, 0600); err != nil {
			t.Fatal(err)
		}
	}
	writeEvent()
	var calls atomic.Int32
	server := mcp.NewServer(&mcp.Implementation{Name: "local-test", Version: "1"}, nil)
	server.AddTool(&mcp.Tool{Name: "ping", InputSchema: map[string]any{"type": "object"}}, func(context.Context, *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		calls.Add(1)
		return &mcp.CallToolResult{}, nil
	})
	mcpHandler := mcp.NewStreamableHTTPHandler(func(*http.Request) *mcp.Server { return server }, &mcp.StreamableHTTPOptions{Stateless: true, JSONResponse: true})
	token := "test-private-native-credential"
	host := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer "+token {
			w.WriteHeader(401)
			return
		}
		mcpHandler.ServeHTTP(w, r)
	}))
	defer host.Close()
	runtime, stream, err := connection.Start(ctx, api.Profile{Version: 1, Kind: "agent", Adapter: "pty", RequireAgentMCP: true, ProjectID: "project", DirectoryID: "directory", WorkingDirectory: dir, Start: api.Command{Argv: []string{agent}}, Env: map[string]string{"DUNE_TEST_NATIVE_CLI": "1", "DUNE_TEST_NATIVE_EVENT": eventPath}})
	if err != nil {
		t.Fatal(err)
	}
	stream.Close()
	// The CLI's first bridge is already starting, before credentials exist.
	time.Sleep(100 * time.Millisecond)
	if calls.Load() != 0 {
		t.Fatal("bridge called MCP before configuration")
	}
	config := api.AgentMCP{URL: host.URL, Token: token}
	status, err := connection.ConfigureAgentMCP(ctx, runtime, config)
	if err != nil || status.Transport != "stdio" {
		t.Fatal(status, err)
	}
	waitTimeoutTest(t, func() bool { return calls.Load() == 1 })
	if _, err := connection.ConfigureAgentMCP(ctx, runtime, config); err == nil {
		t.Fatal("duplicate configuration accepted")
	}
	snapshot, err := connection.Get(ctx, runtime)
	if err != nil || strings.Contains(string(api.Payload(snapshot)), token) {
		t.Fatal("public Runtime leaked MCP configuration", err)
	}
	connection.Close()
	engine.Close()
	// A new native bridge process can use the retained configuration even while
	// fabricd is offline. Production authentication will separately require it.
	writeEvent()
	waitTimeoutTest(t, func() bool { return calls.Load() == 2 })
	restored, err := Open(t.Context(), filepath.Join(dir, "sessions"))
	if err != nil {
		t.Fatal(err)
	}
	defer restored.Close()
	current, ctx := timeoutClient(t, restored)
	observed, err := current.Get(ctx, runtime)
	if err != nil || observed.ID != runtime.ID || observed.Incarnation != runtime.Incarnation || observed.ProjectID != "project" || observed.DirectoryID != "directory" || observed.State != "running" {
		t.Fatal("live Runtime lost identity or project after reconnect", observed, err)
	}
	if _, err := current.ConfigureAgentMCP(ctx, runtime, config); err == nil {
		t.Fatal("fabricd restart allowed credential replacement")
	}
	if err := current.Stop(ctx, runtime); err != nil {
		t.Fatal(err)
	}
	entries, err := os.ReadDir(filepath.Join(dir, "sessions", "native-agents"))
	if err != nil || len(entries) != 0 {
		t.Fatal("stop retained native credentials", err)
	}
}
