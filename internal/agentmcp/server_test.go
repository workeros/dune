package agentmcp

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/aiomni/dune/pkg/agents"
	"github.com/aiomni/dune/pkg/api"
	"github.com/aiomni/dune/pkg/identity"
)

type failingMessenger struct {
	agents.Messenger
	calls int
}

func (m *failingMessenger) Prompt(_ context.Context, scope agents.Scope, request agents.PromptRequest) (agents.Operation, error) {
	m.calls++
	if scope.OwnerID != "verified-tenant" || request.AgentRef != "selected" {
		panic("unexpected MCP scope")
	}
	return agents.Operation{AgentOperation: api.AgentOperation{Ref: "accepted-operation", State: "pending"}}, &api.Error{Code: "RESULT_UNKNOWN", Detail: "private-transport-secret"}
}

func TestMCPErrorPreservesProgressWithoutSecretsOrAutomaticReplay(t *testing.T) {
	messenger := &failingMessenger{}
	handler := New(Options{Messenger: messenger, Authenticate: func(context.Context, string) (agents.Scope, error) {
		return agents.Scope{OwnerID: "verified-tenant", Principal: identity.User{ID: "verified-user"}}, nil
	}})
	body := `{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"agents_prompt","arguments":{"agent_ref":"selected","text":"task","wait_ms":1}}}`
	request := httptest.NewRequest("POST", "/agent-mcp", strings.NewReader(body))
	request.Header.Set("Authorization", "Bearer test-credential")
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("Accept", "application/json, text/event-stream")
	request.Header.Set("MCP-Protocol-Version", "2025-11-25")
	out := httptest.NewRecorder()
	handler.ServeHTTP(out, request)
	var response struct {
		Result struct {
			IsError           bool `json:"isError"`
			StructuredContent struct {
				Code   string           `json:"code"`
				Result agents.Operation `json:"result"`
			} `json:"structuredContent"`
		} `json:"result"`
	}
	if out.Code != http.StatusOK || json.Unmarshal(out.Body.Bytes(), &response) != nil || !response.Result.IsError || response.Result.StructuredContent.Code != "RESULT_UNKNOWN" || response.Result.StructuredContent.Result.Ref != "accepted-operation" || messenger.calls != 1 {
		t.Fatal("unknown result lost accepted progress or repeated the prompt", out.Code, out.Body.String(), messenger.calls)
	}
	if bytes.Contains(out.Body.Bytes(), []byte("private-transport-secret")) || bytes.Contains(out.Body.Bytes(), []byte("test-credential")) {
		t.Fatal("MCP error exposed a secret")
	}
}
