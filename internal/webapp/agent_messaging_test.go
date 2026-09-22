package webapp

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"testing"

	"github.com/aiomni/dune/pkg/agents"
	"github.com/aiomni/dune/pkg/api"
)

type messengerFixture struct {
	agents.Messenger
	prompt func(agents.Scope, agents.PromptRequest) (agents.Operation, error)
	submit func(agents.Scope, agents.SubmissionRequest) (api.SubmissionReceipt, error)
}

func (f messengerFixture) Prompt(_ context.Context, scope agents.Scope, request agents.PromptRequest) (agents.Operation, error) {
	return f.prompt(scope, request)
}

func (f messengerFixture) Submit(_ context.Context, scope agents.Scope, request agents.SubmissionRequest) (api.SubmissionReceipt, error) {
	return f.submit(scope, request)
}

type nativeSessionFixture struct {
	open func(agents.Scope, agents.OpenSessionRequest) (agents.Operation, error)
}

func (f nativeSessionFixture) OpenSession(_ context.Context, scope agents.Scope, request agents.OpenSessionRequest) (agents.Operation, error) {
	return f.open(scope, request)
}

func TestAgentOperationsHTTPAuthenticateScopeAndKeepAcceptedReference(t *testing.T) {
	for _, tenant := range []bool{false, true} {
		t.Run(map[bool]string{false: "personal", true: "tenant"}[tenant], func(t *testing.T) {
			f := newWorkbenchFixture(t, tenant)
			prompt := agents.PromptRequest{ExpectedConversationID: "caller-observed-c1", AgentRef: "selected", Text: "task", WaitMS: 1000}
			open := agents.OpenSessionRequest{AgentRef: "selected", Action: "load", SessionID: "native", Cwd: "/repo", WaitMS: 1000}
			for _, tc := range []struct {
				path    string
				request any
				forged  map[string]string
			}{
				{"/agents/prompt", prompt, map[string]string{"agent_ref": "selected", "text": "task", "owner_id": "other"}},
				{"/agents/open-session", open, map[string]string{"agent_ref": "selected", "action": "new", "owner_id": "other"}},
			} {
				t.Run(tc.path, func(t *testing.T) {
					calls := 0
					accept := func(scope agents.Scope, matches bool) (agents.Operation, error) {
						calls++
						if scope.OwnerID != f.owner || scope.Principal.ID != f.users[0].ID || !matches {
							t.Fatal("request changed authenticated scope or operation input")
						}
						return agents.Operation{AgentOperation: api.AgentOperation{Ref: "accepted-operation", State: "pending"}}, &api.Error{Code: "RESULT_UNKNOWN", Detail: "optional wait interrupted"}
					}
					f.server.options.AgentMessenger = messengerFixture{prompt: func(scope agents.Scope, request agents.PromptRequest) (agents.Operation, error) {
						return accept(scope, request == prompt)
					}}
					f.server.options.AgentNativeSessions = nativeSessionFixture{open: func(scope agents.Scope, request agents.OpenSessionRequest) (agents.Operation, error) {
						return accept(scope, request == open)
					}}
					out := f.request(t, "POST", tc.path, 0, tc.request)
					if out.Code != http.StatusServiceUnavailable || !bytes.Contains(out.Body.Bytes(), []byte("accepted-operation")) || calls != 1 {
						t.Fatal("accepted operation lost or repeated", out.Code, out.Body.String(), calls)
					}
					if out := f.request(t, "POST", tc.path, -1, tc.request); out.Code != http.StatusUnauthorized || calls != 1 {
						t.Fatal("anonymous request reached service", out.Code)
					}
					if tenant {
						if out := f.request(t, "POST", tc.path, 2, tc.request); out.Code != http.StatusForbidden || calls != 1 {
							t.Fatal("cross-Tenant request reached service", out.Code)
						}
					}
					if out := f.request(t, "POST", tc.path, 0, tc.forged); out.Code != http.StatusBadRequest || calls != 1 {
						t.Fatal("payload injected scope", out.Code)
					}
				})
			}
		})
	}
}

func TestAgentSubmissionHTTPAllowsBoundedAttachments(t *testing.T) {
	for _, tenant := range []bool{false, true} {
		t.Run(map[bool]string{false: "personal", true: "tenant"}[tenant], func(t *testing.T) {
			f := newWorkbenchFixture(t, tenant)
			for _, test := range []struct {
				name               string
				imageBytes, status int
			}{
				{name: "above default body limit", imageBytes: 220 * 1024, status: http.StatusOK},
				{name: "near prompt limit", imageBytes: (api.MaxACPPromptBytes - 1024) / 4 * 3, status: http.StatusOK},
				{name: "above submission limit", imageBytes: maxAgentSubmissionBytes / 4 * 3, status: http.StatusBadRequest},
			} {
				t.Run(test.name, func(t *testing.T) {
					attachment := api.Payload(map[string]string{"type": "image", "mimeType": "image/png", "data": base64.StdEncoding.EncodeToString(bytes.Repeat([]byte("x"), test.imageBytes))})
					expected := agents.SubmissionRequest{SubmissionID: "attachment", AgentRef: "selected", ACPAction: api.ACPAction{Action: "prompt", ExpectedConversationID: "observed-conversation", Text: "Inspect this image", Attachments: []json.RawMessage{attachment}}}
					calls := 0
					f.server.options.AgentMessenger = messengerFixture{submit: func(scope agents.Scope, request agents.SubmissionRequest) (api.SubmissionReceipt, error) {
						calls++
						if scope.OwnerID != f.owner || scope.Principal.ID != f.users[0].ID || !bytes.Equal(api.Payload(request), api.Payload(expected)) {
							t.Fatal("attachment or submission scope changed before reaching service")
						}
						return api.SubmissionReceipt{Admission: api.SubmissionAccepted}, nil
					}}
					out := f.request(t, "POST", "/agents/submit", 0, expected)
					if out.Code != test.status || (calls == 1) != (test.status == http.StatusOK) {
						t.Fatalf("status=%d calls=%d: %s", out.Code, calls, out.Body.String())
					}
				})
			}
			// The enlarged budget belongs only to structured ACP submissions.
			out := f.request(t, "POST", "/agents/prompt", 0, agents.PromptRequest{AgentRef: "selected", Text: string(bytes.Repeat([]byte("x"), defaultJSONBodyBytes))})
			if out.Code != http.StatusBadRequest {
				t.Fatal("ordinary JSON endpoint lost its body limit", out.Code)
			}
		})
	}
}
