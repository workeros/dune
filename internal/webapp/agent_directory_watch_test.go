package webapp

import (
	"context"
	"io"
	"net/http"
	"strings"
	"testing"

	"github.com/aiomni/dune/pkg/agents"
)

type directoryObserverFunc func(context.Context, agents.Scope, agents.DirectoryWatch) (agents.DirectorySubscription, error)

func (f directoryObserverFunc) Subscribe(ctx context.Context, s agents.Scope, q agents.DirectoryWatch) (agents.DirectorySubscription, error) {
	return f(ctx, s, q)
}

type httpDirectorySubscription struct {
	next   func() (agents.DirectoryEvent, error)
	closed bool
}

func (s *httpDirectorySubscription) ID() string { return "http-subscription" }
func (s *httpDirectorySubscription) Next(context.Context) (agents.DirectoryEvent, error) {
	return s.next()
}
func (s *httpDirectorySubscription) Close() error { s.closed = true; return nil }

func TestAgentDirectoryHTTPSubscriptionScopeAndCredentialRevocation(t *testing.T) {
	for _, tenant := range []bool{false, true} {
		t.Run(map[bool]string{false: "personal", true: "tenant"}[tenant], func(t *testing.T) {
			f := newWorkbenchFixture(t, tenant)
			calls := 0
			sub := &httpDirectorySubscription{}
			f.server.options.AgentDirectoryObserver = directoryObserverFunc(func(_ context.Context, scope agents.Scope, q agents.DirectoryWatch) (agents.DirectorySubscription, error) {
				calls++
				if scope.OwnerID != f.owner || scope.Principal.ID != f.users[0].ID || len(q.RunnerIDs) != 1 || q.RunnerIDs[0] != "runner" {
					t.Fatal(scope, q)
				}
				return sub, nil
			})
			sub.next = func() (agents.DirectoryEvent, error) { return agents.DirectoryEvent{}, io.EOF }
			if out := f.request(t, "GET", "/agents/events?runner_id=runner", -1, nil); out.Code != http.StatusUnauthorized || calls != 0 {
				t.Fatal(out.Code, calls)
			}
			if tenant {
				if out := f.request(t, "GET", "/agents/events?runner_id=runner", 2, nil); out.Code != http.StatusForbidden || calls != 0 {
					t.Fatal(out.Code, calls)
				}
			}
			out := f.request(t, "GET", "/agents/events?runner_id=runner", 0, nil)
			if out.Code != 200 || out.Header().Get("Content-Type") != "text/event-stream" || !strings.Contains(out.Body.String(), `"kind":"ready"`) || !strings.Contains(out.Body.String(), `"kind":"invalidated"`) || !sub.closed {
				t.Fatal(out.Code, out.Body.String(), sub.closed)
			}
			sub.closed = false
			sub.next = func() (agents.DirectoryEvent, error) {
				if err := f.server.identity.Logout(t.Context(), f.tokens[0]); err != nil {
					t.Fatal(err)
				}
				return agents.DirectoryEvent{SubscriptionID: sub.ID(), Kind: agents.DirectoryMember, Agent: &agents.Agent{Ref: "must-not-leak"}}, nil
			}
			out = f.request(t, "GET", "/agents/events?runner_id=runner", 0, nil)
			if strings.Contains(out.Body.String(), "must-not-leak") || !sub.closed {
				t.Fatal("revoked credential received buffered data", out.Body.String())
			}
		})
	}
}
