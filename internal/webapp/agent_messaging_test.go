package webapp

import (
	"bytes"
	"context"
	"net/http"
	"testing"

	"github.com/aiomni/dune/pkg/agents"
	"github.com/aiomni/dune/pkg/api"
)

type messengerFixture struct {
	agents.Messenger
	prompt func(agents.Scope, agents.PromptRequest) (agents.Operation, error)
}

func (f messengerFixture) Prompt(_ context.Context, scope agents.Scope, request agents.PromptRequest) (agents.Operation, error) {
	return f.prompt(scope, request)
}

func TestAgentMessagingHTTPPreservesAcceptedOperationAndAuthenticatesScope(t *testing.T) {
	for _, tenant := range []bool{false, true} {
		t.Run(map[bool]string{false: "personal", true: "tenant"}[tenant], func(t *testing.T) {
			f := newWorkbenchFixture(t, tenant)
			calls := 0
			f.server.options.AgentMessenger = messengerFixture{prompt: func(scope agents.Scope, request agents.PromptRequest) (agents.Operation, error) {
				calls++
				if scope.OwnerID != f.owner || scope.Principal.ID != f.users[0].ID || request.ExpectedConversationID != "caller-observed-c1" || request.AgentRef != "selected" || request.Text != "task" || request.WaitMS != 1000 {
					t.Fatal("request changed authenticated scope")
				}
				return agents.Operation{AgentOperation: api.AgentOperation{Ref: "accepted-operation", State: "pending"}}, &api.Error{Code: "RESULT_UNKNOWN", Detail: "optional wait interrupted"}
			}}
			request := agents.PromptRequest{ExpectedConversationID: "caller-observed-c1", AgentRef: "selected", Text: "task", WaitMS: 1000}
			out := f.request(t, "POST", "/agents/prompt", 0, request)
			if out.Code != http.StatusServiceUnavailable || !bytes.Contains(out.Body.Bytes(), []byte("accepted-operation")) || calls != 1 {
				t.Fatal("accepted reference lost or prompt replayed", out.Code, out.Body.String(), calls)
			}
			if out := f.request(t, "POST", "/agents/prompt", -1, request); out.Code != http.StatusUnauthorized || calls != 1 {
				t.Fatal("anonymous prompt reached service", out.Code)
			}
			if tenant {
				if out := f.request(t, "POST", "/agents/prompt", 2, request); out.Code != http.StatusForbidden || calls != 1 {
					t.Fatal("cross-Tenant prompt reached service", out.Code)
				}
			}
			if out := f.request(t, "POST", "/agents/prompt", 0, map[string]string{"agent_ref": "selected", "text": "task", "owner_id": "other"}); out.Code != http.StatusBadRequest || calls != 1 {
				t.Fatal("payload injected scope", out.Code)
			}
		})
	}
}
