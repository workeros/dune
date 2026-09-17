package webapp

import (
	"bytes"
	"context"
	"net/http"
	"testing"

	"github.com/aiomni/dune/pkg/agents"
	"github.com/aiomni/dune/pkg/api"
)

type nativeSessionFixture struct {
	open func(agents.Scope, agents.OpenSessionRequest) (agents.Operation, error)
}

func (f nativeSessionFixture) OpenSession(_ context.Context, scope agents.Scope, request agents.OpenSessionRequest) (agents.Operation, error) {
	return f.open(scope, request)
}

func TestNativeSessionHTTPAuthenticatesScopeAndKeepsAcceptedReference(t *testing.T) {
	for _, tenant := range []bool{false, true} {
		t.Run(map[bool]string{false: "personal", true: "tenant"}[tenant], func(t *testing.T) {
			f := newWorkbenchFixture(t, tenant)
			calls := 0
			request := agents.OpenSessionRequest{AgentRef: "selected", Action: "load", SessionID: "native", Cwd: "/repo", WaitMS: 1000}
			f.server.options.AgentNativeSessions = nativeSessionFixture{open: func(scope agents.Scope, actual agents.OpenSessionRequest) (agents.Operation, error) {
				calls++
				if scope.OwnerID != f.owner || scope.Principal.ID != f.users[0].ID || actual != request {
					t.Fatal("native session request changed authenticated scope")
				}
				return agents.Operation{AgentOperation: api.AgentOperation{Ref: "accepted-load", State: "pending"}}, &api.Error{Code: "RESULT_UNKNOWN", Detail: "wait interrupted"}
			}}
			out := f.request(t, "POST", "/agents/open-session", 0, request)
			if out.Code != http.StatusServiceUnavailable || !bytes.Contains(out.Body.Bytes(), []byte("accepted-load")) || calls != 1 {
				t.Fatal("accepted operation lost or repeated", out.Code, out.Body.String(), calls)
			}
			if out := f.request(t, "POST", "/agents/open-session", -1, request); out.Code != http.StatusUnauthorized || calls != 1 {
				t.Fatal("anonymous lifecycle request reached service", out.Code)
			}
			if tenant {
				if out := f.request(t, "POST", "/agents/open-session", 2, request); out.Code != http.StatusForbidden || calls != 1 {
					t.Fatal("cross Tenant lifecycle request reached service", out.Code)
				}
			}
			if out := f.request(t, "POST", "/agents/open-session", 0, map[string]string{"agent_ref": "selected", "action": "new", "owner_id": "other"}); out.Code != http.StatusBadRequest || calls != 1 {
				t.Fatal("lifecycle payload supplied scope", out.Code)
			}
		})
	}
}
