package webapp

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"
	"time"

	"github.com/aiomni/dune/internal/authorization"
	"github.com/aiomni/dune/internal/identity"
	"github.com/aiomni/dune/internal/metadata"
	"github.com/aiomni/dune/pkg/access"
	"github.com/aiomni/dune/pkg/gateway"
	"github.com/aiomni/dune/pkg/runner"
	"github.com/aiomni/dune/pkg/workbench"
)

type workbenchFixture struct {
	server        *Server
	store         *metadata.Store
	users         []identity.User
	tokens        []string
	owner, prefix string
}

func newWorkbenchFixture(t *testing.T, tenant bool) *workbenchFixture {
	t.Helper()
	store, err := OpenStore(filepath.Join(t.TempDir(), "metadata"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { store.Close() })
	local := identity.NewLocal(store, true)
	f := &workbenchFixture{store: store, prefix: "/api/v1"}
	for _, email := range []string{"one@test.dev", "two@test.dev", "outside@test.dev"} {
		user, token, err := local.Register(t.Context(), email, "strong-test-password")
		if err != nil {
			t.Fatal(err)
		}
		f.users = append(f.users, user)
		f.tokens = append(f.tokens, token)
	}
	f.owner = f.users[0].ID
	authorizer := authorization.NewLocal(t.Context(), local, store)
	if tenant {
		f.owner, f.prefix = "tenant-a", "/api/v1/tenants/tenant-a"
		checker := unbindPolicyFunc(func(_ context.Context, request access.Request) (access.Decision, error) {
			allowed := request.OwnerID == f.owner && request.PrincipalID != f.users[2].ID
			return access.Decision{Allowed: allowed, Reason: "TENANT_MEMBERSHIP", ID: request.RequestID, ValidUntil: time.Now().Add(access.MaxLease)}, nil
		})
		authorizer = authorization.New(t.Context(), local, store, checker)
	}
	f.server, err = NewServer(t.Context(), Options{PublicURL: "http://dune.test/", DialGateway: noGateway, TenantScoped: tenant}, store, local, authorizer, gateway.New())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(f.server.Close)
	return f
}

func (f *workbenchFixture) request(t *testing.T, method, suffix string, user int, body any) *httptest.ResponseRecorder {
	t.Helper()
	encoded, err := json.Marshal(body)
	if err != nil {
		t.Fatal(err)
	}
	r := httptest.NewRequest(method, f.prefix+suffix, bytes.NewReader(encoded))
	r.Header.Set("Content-Type", "application/json")
	r.Header.Set("X-Dune-Request", "1")
	r.Header.Set("Origin", "http://dune.test")
	if user >= 0 {
		r.AddCookie(&http.Cookie{Name: cookieName, Value: f.tokens[user]})
	}
	w := httptest.NewRecorder()
	f.server.ServeHTTP(w, r)
	return w
}

func TestProjectHTTPScopesAndDirectoryBindings(t *testing.T) {
	for _, tenant := range []bool{false, true} {
		t.Run(map[bool]string{false: "personal", true: "tenant"}[tenant], func(t *testing.T) {
			f := newWorkbenchFixture(t, tenant)
			_, token, _, err := f.store.IssueEnrollment(t.Context(), f.owner, "development")
			if err != nil {
				t.Fatal(err)
			}
			machine, _, err := f.store.Enroll(t.Context(), token, "linux", "amd64")
			if err != nil {
				t.Fatal(err)
			}
			binding := runner.Binding{RunnerID: machine.RunnerID, MachineID: machine.ID, FabricID: "attached", Revision: 1}
			body := struct {
				Revision int64 `json:"revision"`
				workbench.ProjectSpec
			}{ProjectSpec: workbench.ProjectSpec{Name: "Dune", Directories: []workbench.Directory{{ID: "checkout", Path: "/src/dune", Binding: binding}}}}
			if out := f.request(t, "POST", "/projects", -1, body); out.Code != 401 {
				t.Fatal("anonymous create", out.Code)
			}
			out := f.request(t, "POST", "/projects", 0, body)
			if out.Code != 201 {
				t.Fatal(out.Code, out.Body.String())
			}
			var saved workbench.Project
			if err := json.Unmarshal(out.Body.Bytes(), &saved); err != nil {
				t.Fatal(err)
			}
			if saved.OwnerID != f.owner || saved.Revision != 1 {
				t.Fatal(saved)
			}
			for _, method := range []string{"GET", "PUT", "DELETE"} {
				body.Revision = 1
				out := f.request(t, method, "/projects/"+saved.ID+"?revision=1", 2, body)
				if out.Code != 403 && out.Code != 404 {
					t.Fatal("cross-owner access", method, out.Code, out.Body.String())
				}
			}
			out = f.request(t, "GET", "/projects", 2, nil)
			if bytes.Contains(out.Body.Bytes(), []byte(saved.ID)) {
				t.Fatal("cross-owner list")
			}
			reader := 0
			if tenant {
				reader = 1
			}
			if out := f.request(t, "GET", "/projects/"+saved.ID, reader, nil); out.Code != 200 {
				t.Fatal("shared project not visible", out.Code)
			}
			body.Revision, body.Name = 1, "Updated"
			if out := f.request(t, "PUT", "/projects/"+saved.ID, reader, body); out.Code != 200 {
				t.Fatal(out.Code, out.Body.String())
			}
			if out := f.request(t, "PUT", "/projects/"+saved.ID, 0, body); out.Code != 409 {
				t.Fatal("stale update", out.Code)
			}
			body.Revision = 2
			body.Directories[0].Binding.Revision++
			if out := f.request(t, "PUT", "/projects/"+saved.ID, 0, body); out.Code != 409 || !bytes.Contains(out.Body.Bytes(), []byte("BINDING_CHANGED")) {
				t.Fatal("stale binding", out.Code, out.Body.String())
			}
			if out := f.request(t, "DELETE", "/projects/"+saved.ID+"?revision=2", 0, nil); out.Code != 204 {
				t.Fatal(out.Code, out.Body.String())
			}
			if _, err := f.store.RunnerResource(t.Context(), machine.RunnerID); err != nil {
				t.Fatal("deleting project removed Runner", err)
			}
		})
	}
}
