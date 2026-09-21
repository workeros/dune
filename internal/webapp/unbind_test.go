package webapp

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"testing"
	"time"

	"github.com/aiomni/dune/internal/authorization"
	"github.com/aiomni/dune/internal/identity"
	"github.com/aiomni/dune/pkg/access"
	"github.com/aiomni/dune/pkg/gateway"
)

type unbindPolicyFunc func(context.Context, access.Request) (access.Decision, error)

func (f unbindPolicyFunc) Check(ctx context.Context, request access.Request) (access.Decision, error) {
	return f(ctx, request)
}

func TestUserUnbindRoutesShareAuthoritativePolicy(t *testing.T) {
	ctx := context.Background()
	store, err := OpenStore(filepath.Join(t.TempDir(), "metadata"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	local := identity.NewLocal(store, true)
	creator, cookie, err := local.Register(ctx, "unbind@example.test", "unbind-policy-password")
	if err != nil {
		t.Fatal(err)
	}
	allowed, unavailable, calls := false, false, 0
	var selectedTarget, selectedRunner string
	checker := unbindPolicyFunc(func(ctx context.Context, request access.Request) (access.Decision, error) {
		if request.Operation == "runner.unbind" {
			calls++
			if request.OwnerID != creator.ID || request.CreatedBy.ID != creator.ID || request.Binding.MachineID != selectedTarget || request.Binding.RunnerID != selectedRunner || request.Suboperation != "attached" {
				t.Fatal("unbind policy did not receive authoritative resource facts", request)
			}
			if unavailable {
				<-ctx.Done()
				return access.Decision{}, ctx.Err()
			}
		}
		return access.Decision{Allowed: allowed, ID: request.RequestID, Reason: "HOST_POLICY", ValidUntil: time.Now().Add(access.MaxLease)}, nil
	})
	server, err := NewServer(ctx, Options{PublicURL: "http://dune.test/", TenantScoped: true, DialGateway: noGateway}, store, local, authorization.New(ctx, local, store, checker, nil), gateway.New())
	if err != nil {
		t.Fatal(err)
	}
	defer server.Close()
	for _, route := range []string{"runner", "tenant"} {
		t.Run(route, func(t *testing.T) {
			_, token, _, err := store.IssueEnrollment(ctx, creator.ID, route)
			if err != nil {
				t.Fatal(err)
			}
			machine, credential, err := store.Enroll(ctx, token, "linux", "amd64")
			if err != nil {
				t.Fatal(err)
			}
			selectedTarget, selectedRunner = machine.ID, machine.RunnerID
			query := url.Values{"machine_id": {machine.ID}, "fabric_id": {"attached"}, "revision": {"1"}}
			path := "/api/v1/runners/" + machine.RunnerID + "/binding?" + query.Encode()
			if route == "tenant" {
				path = fmt.Sprintf("/api/v1/tenants/%s/runners/%s/binding?%s", creator.ID, machine.RunnerID, query.Encode())
			}
			request := func() *httptest.ResponseRecorder {
				r := httptest.NewRequest(http.MethodDelete, path, nil)
				r.AddCookie(&http.Cookie{Name: cookieName, Value: cookie})
				r.Header.Set("Origin", "http://dune.test")
				r.Header.Set("X-Dune-Request", "1")
				out := httptest.NewRecorder()
				server.ServeHTTP(out, r)
				return out
			}
			allowed, unavailable, calls = false, false, 0
			if out := request(); out.Code != http.StatusForbidden || calls != 1 {
				t.Fatal("route bypassed or repeated the unbind policy", out.Code, out.Body.String(), calls)
			}
			if _, err := store.MachineCredential(ctx, credential); err != nil {
				t.Fatal("denied unbind changed the credential", err)
			}
			if route == "tenant" {
				unavailable = true
				if out := request(); out.Code != http.StatusServiceUnavailable {
					t.Fatal("policy timeout was not unavailable", out.Code, out.Body.String())
				}
				if _, err := store.MachineCredential(ctx, credential); err != nil {
					t.Fatal("policy timeout changed the credential", err)
				}
			}
			allowed, unavailable, calls = true, false, 0
			out := request()
			if (out.Code != http.StatusOK && out.Code != http.StatusNoContent) || calls != 1 {
				t.Fatal("allowed unbind failed", out.Code, out.Body.String(), calls)
			}
			if _, err := store.MachineCredential(ctx, credential); err == nil {
				t.Fatal("allowed unbind retained credential")
			}
		})
	}
}
