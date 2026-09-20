package host

import (
	"bytes"
	"encoding/json"
	"github.com/aiomni/dune/internal/wire"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/aiomni/dune/pkg/agents"
	"github.com/aiomni/dune/pkg/api"
	"github.com/aiomni/dune/pkg/profiles"
	"github.com/aiomni/dune/pkg/workbench"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

type mcpBearerTransport struct{ token string }

func (b mcpBearerTransport) RoundTrip(request *http.Request) (*http.Response, error) {
	request = request.Clone(request.Context())
	request.Header.Set("Authorization", "Bearer "+b.token)
	return http.DefaultTransport.RoundTrip(request)
}

func connectMCP(t *testing.T, endpoint, token string) *mcp.ClientSession {
	t.Helper()
	client := mcp.NewClient(&mcp.Implementation{Name: "test-agent", Version: "1"}, nil)
	session, err := client.Connect(t.Context(), &mcp.StreamableClientTransport{Endpoint: endpoint, HTTPClient: &http.Client{Transport: mcpBearerTransport{token: token}}, MaxRetries: -1, DisableStandaloneSSE: true}, nil)
	if err != nil {
		t.Fatal("connect MCP", err)
	}
	t.Cleanup(func() { _ = session.Close() })
	return session
}

func callMCP[Out any](t *testing.T, session *mcp.ClientSession, name string, arguments any) Out {
	t.Helper()
	result, err := session.CallTool(t.Context(), &mcp.CallToolParams{Name: name, Arguments: arguments})
	if err != nil || result.IsError {
		t.Fatal("MCP call failed", name, result, err)
	}
	var out Out
	encoded, err := json.Marshal(result.StructuredContent)
	if err != nil || json.Unmarshal(encoded, &out) != nil {
		t.Fatal("invalid MCP structured output", name, err)
	}
	return out
}

func issueMCPFixture(t *testing.T, f executorFixture, runtime api.Runtime) string {
	t.Helper()
	token, err := f.app.store.IssueAgentCredential(t.Context(), f.agentScope(), fixtureTarget(f, runtime), time.Now().Add(time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	return token
}

func TestAgentMCPToolsUseAuthenticatedScopeAndFabricdAcrossHTTPHandlers(t *testing.T) {
	f := openExecutorFixture(t)
	started, _ := directoryACP(t, f)
	token := issueMCPFixture(t, f, *started.Runtime)
	profile := directoryACPProfile(t, f)
	for _, name := range []string{"Assistant one", "Assistant two"} {
		if _, err := f.app.store.Profiles().Create(t.Context(), profiles.Record{OwnerID: f.owner, Name: name, Profile: profile, CreatedBy: profiles.Actor{Type: "user", Subject: f.principal.ID}}); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := f.app.store.Profiles().Create(t.Context(), profiles.Record{OwnerID: "foreign-tenant", Name: "Foreign private profile", Profile: profile, CreatedBy: profiles.Actor{Type: "user", Subject: "other"}}); err != nil {
		t.Fatal(err)
	}
	// Independent stateless HTTP handlers alternate for every protocol request.
	// The actual execution service remains the local SDK/Gateway/fabricd fixture.
	second := f.app.newAgentMCP("", "http://dune.example.test")
	var requests atomic.Int64
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if requests.Add(1)%2 == 0 {
			second.ServeHTTP(w, r)
		} else {
			f.app.ServeHTTP(w, r)
		}
	}))
	defer server.Close()
	client := connectMCP(t, server.URL+"/api/v1/agent-mcp", token)
	tools, err := client.ListTools(t.Context(), nil)
	if err != nil || len(tools.Tools) != 11 {
		t.Fatal("tool catalog", tools, err)
	}
	for _, tool := range tools.Tools {
		encoded, _ := json.Marshal(tool.InputSchema)
		if strings.Contains(string(encoded), "owner_id") || strings.Contains(string(encoded), "tenant_id") || strings.Contains(string(encoded), "principal") {
			t.Fatal("tool accepts caller identity", tool.Name)
		}
	}
	type catalog struct {
		Items []struct {
			ID       string `json:"id"`
			Revision int64  `json:"revision"`
			Adapter  string `json:"adapter"`
			Name     string `json:"name"`
		} `json:"items"`
		NextCursor string `json:"next_cursor"`
	}
	first := callMCP[catalog](t, client, "profiles_list", map[string]any{"limit": 1})
	last := callMCP[catalog](t, client, "profiles_list", map[string]any{"limit": 1, "cursor": first.NextCursor})
	if len(first.Items) != 1 || first.NextCursor == "" || len(last.Items) != 1 || last.NextCursor != "" || first.Items[0].ID == last.Items[0].ID {
		t.Fatal("profile pagination or Tenant filtering", first, last)
	}
	raw, err := client.CallTool(t.Context(), &mcp.CallToolParams{Name: "profiles_list", Arguments: map[string]any{}})
	encoded, _ := json.Marshal(raw)
	if err != nil || bytes.Contains(encoded, []byte("TEST_LAUNCH_SECRET")) || bytes.Contains(encoded, []byte("not-for-discovery")) || bytes.Contains(encoded, []byte("Foreign private profile")) {
		t.Fatal("profile catalog exposed private configuration", err)
	}
	page := callMCP[agents.DirectoryPage](t, client, "agents_list", map[string]any{})
	if len(page.Items) != 1 || page.Items[0].Ref != started.AgentRef {
		t.Fatal("Agent discovery did not share launch reference", page)
	}
	launched := callMCP[agents.LaunchResult](t, client, "agents_start", map[string]any{"submission_id": "mcp-launch", "binding": f.binding, "profile": map[string]any{"id": first.Items[0].ID, "revision": first.Items[0].Revision}, "working_directory": f.workspace})
	if launched.Runtime == nil || launched.AgentRef == "" || launched.Operation == nil || launched.Operation.State != "completed" {
		t.Fatal("MCP start did not prepare a native session", launched)
	}
	prompt := callMCP[agents.Operation](t, client, "agents_prompt", agents.PromptRequest{SubmissionID: wire.ID(), ExpectedConversationID: launched.Runtime.ConversationID, AgentRef: launched.AgentRef, Text: "delegated task"})
	waited := callMCP[agents.WaitResult](t, client, "agents_wait", agents.WaitRequest{OperationRef: prompt.Ref, TimeoutMS: 3000})
	if waited.Operation == nil || waited.Operation.State != "completed" || waited.Operation.Ref != prompt.Ref {
		t.Fatal("operation completion crossed HTTP handlers", waited)
	}
	output := callMCP[agents.ReadResult](t, client, "agents_read", agents.ReadRequest{OperationRef: prompt.Ref})
	if output.Operation == nil || output.Operation.Incomplete || output.Operation.NextPosition != 1 || len(output.Operation.Output) != 1 {
		t.Fatal("MCP could not read accepted operation", output)
	}
	bad, err := client.CallTool(t.Context(), &mcp.CallToolParams{Name: "agents_list", Arguments: map[string]any{"owner_id": "foreign-tenant"}})
	if err == nil && !bad.IsError {
		t.Fatal("tool accepted caller-provided Tenant")
	}
	if err := f.app.store.RevokeAgentCredential(t.Context(), f.owner, fixtureTarget(f, *started.Runtime)); err != nil {
		t.Fatal(err)
	}
	if _, err := client.CallTool(t.Context(), &mcp.CallToolParams{Name: "agents_list", Arguments: map[string]any{}}); err == nil {
		t.Fatal("MCP protocol connection retained revoked authority")
	}
}

func TestAgentMCPRejectsUntrustedHTTPAndExitedCaller(t *testing.T) {
	f := openExecutorFixture(t)
	started, connection := directoryACP(t, f)
	token := issueMCPFixture(t, f, *started.Runtime)
	invoke := func(path, bearer, origin string) int {
		r := httptest.NewRequest("POST", path, strings.NewReader(`{"jsonrpc":"2.0","id":1,"method":"tools/list","params":{}}`))
		r.Header.Set("Content-Type", "application/json")
		r.Header.Set("Accept", "application/json, text/event-stream")
		r.Header.Set("MCP-Protocol-Version", "2025-11-25")
		if bearer != "" {
			r.Header.Set("Authorization", "Bearer "+bearer)
		}
		if origin != "" {
			r.Header.Set("Origin", origin)
		}
		w := httptest.NewRecorder()
		f.app.ServeHTTP(w, r)
		return w.Code
	}
	for _, test := range []struct {
		path, bearer, origin string
		status               int
	}{
		{"/api/v1/agent-mcp", "", "", 401},
		{"/api/v1/agent-mcp?token=forbidden", token, "", 401},
		{"/api/v1/agent-mcp", token, "http://untrusted.test", 403},
		{"/api/v1/agent-mcp", "invalid", "", 401},
		{"/api/v1/agent-mcp", token, "", 200},
	} {
		if got := invoke(test.path, test.bearer, test.origin); got != test.status {
			t.Fatal("unexpected MCP admission", got, test.status)
		}
	}
	stopAgentRuntime(t, connection, *started.Runtime)
	if got := invoke("/api/v1/agent-mcp", token, ""); got != http.StatusUnauthorized {
		t.Fatal("exited Runtime retained MCP authority", got)
	}
}

func fixtureTarget(f executorFixture, runtime api.Runtime) workbench.AgentTarget {
	return workbench.AgentTarget{Binding: f.binding, Runtime: workbench.RuntimeRef{ID: runtime.ID, Incarnation: runtime.Incarnation, Generation: runtime.Generation, Adapter: runtime.Adapter}}
}
