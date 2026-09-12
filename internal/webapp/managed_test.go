package webapp

import (
	"bytes"
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"
	"time"

	internalidentity "github.com/aiomni/dune/internal/identity"
	"github.com/aiomni/dune/internal/metadata"
	"github.com/aiomni/dune/pkg/fabric"
	"github.com/aiomni/dune/pkg/identity"
	"github.com/aiomni/dune/pkg/managed"
	"github.com/aiomni/dune/pkg/runner"
)

type managedMutationStub struct {
	managed.Service
	store *metadata.Store
	owner string
}

type managedCreateStub struct {
	managedMutationStub
	store *metadata.Store
	skip  bool
}

func (s managedCreateStub) Create(ctx context.Context, user identity.User, tenantID, _ string, _ fabric.CreateRequest) (managed.Creation, error) {
	logical := runner.Runner{ID: "created-runner", Name: "service-output", Kind: "managed"}
	if !s.skip {
		if _, _, err := s.store.IssueManagedEnrollment(ctx, user, tenantID, runner.Runner{ID: logical.ID, Name: "authoritative", Kind: "managed"}, "provider"); err != nil {
			return managed.Creation{}, err
		}
	}
	return managed.Creation{Runner: logical, Operation: managed.Operation{ID: "create", OwnerID: tenantID, RunnerID: logical.ID, FabricID: "provider", Action: "create"}}, nil
}

func (s managedMutationStub) Pause(ctx context.Context, _ identity.User, _ string, runnerID string) (managed.Mutation, error) {
	_, err := s.store.SetManagedSuspended(ctx, s.owner, runnerID, true)
	return managed.Mutation{Operation: managed.Operation{ID: "pause", OwnerID: s.owner, RunnerID: runnerID, Action: "pause"}}, err
}

func (s managedMutationStub) Resume(ctx context.Context, _ identity.User, _ string, runnerID string) (managed.Mutation, error) {
	_, err := s.store.SetManagedSuspended(ctx, s.owner, runnerID, false)
	return managed.Mutation{Operation: managed.Operation{ID: "resume", OwnerID: s.owner, RunnerID: runnerID, Action: "resume"}}, err
}

func (s managedMutationStub) Destroy(ctx context.Context, _ identity.User, _ string, runnerID string) (managed.Destruction, error) {
	_, err := s.store.RevokeManaged(ctx, s.owner, runnerID)
	return managed.Destruction{Operation: managed.Operation{ID: "destroy", OwnerID: s.owner, RunnerID: runnerID, Action: "destroy"}}, err
}

func TestStatusResponseIncludesRenewalDecision(t *testing.T) {
	now := time.Now().UTC().Truncate(time.Millisecond)
	view := statusResponse(managed.Status{
		Operation:            managed.Operation{ID: "operation", RunnerID: "runner", FabricID: "cloud", BindingRevision: 1, Action: "create", CreatedAt: now},
		RenewalPolicyVersion: "enterprise-v4", RenewalReason: "ENTERPRISE_WAIT",
		RenewalObservedAt: now, RenewalNextCheckAt: now.Add(time.Minute),
		AccessSuspended: true, ResourceState: string(fabric.ResourcePaused), Capabilities: &fabric.ResourceCapabilities{PauseResume: true},
	})
	if view.RenewalPolicyVersion != "enterprise-v4" || view.RenewalReason != "ENTERPRISE_WAIT" || view.RenewalObservedAt == nil || !view.RenewalObservedAt.Equal(now) || view.RenewalNextCheckAt == nil || !view.RenewalNextCheckAt.Equal(now.Add(time.Minute)) || view.RenewalUntil != nil || !view.AccessSuspended || view.ResourceState != string(fabric.ResourcePaused) || view.Capabilities == nil || !view.Capabilities.PauseResume {
		t.Fatal("renewal decision was omitted from the public status view", view)
	}
}

func TestManagedCreateReturnsAuthoritativeDuneRunner(t *testing.T) {
	for _, test := range []struct {
		name       string
		skipRecord bool
		want       int
	}{
		{name: "bound", want: http.StatusAccepted},
		{name: "missing Dune record", skipRecord: true, want: http.StatusNotFound},
	} {
		t.Run(test.name, func(t *testing.T) {
			ctx := context.Background()
			store, err := OpenStore(filepath.Join(t.TempDir(), "metadata"))
			if err != nil {
				t.Fatal(err)
			}
			defer store.Close()
			local := internalidentity.NewLocal(store, true)
			_, session, err := local.Register(ctx, "owner@example.test", "a-strong-test-password")
			if err != nil {
				t.Fatal(err)
			}
			service := managedCreateStub{managedMutationStub: managedMutationStub{store: store, owner: "tenant-a"}, store: store, skip: test.skipRecord}
			app, err := newTestServer(ctx, Options{DialGateway: noGateway, PublicURL: "http://dune.example.test", Managed: service}, store, local)
			if err != nil {
				t.Fatal(err)
			}
			defer app.Close()
			r := httptest.NewRequest(http.MethodPost, "/api/managed/tenants/tenant-a/runners", bytes.NewBufferString(`{"request_key":"request","request":{}}`))
			r.Header.Set("Content-Type", "application/json")
			r.Header.Set("Origin", "http://dune.example.test")
			r.Header.Set("X-Dune-Request", "1")
			r.AddCookie(&http.Cookie{Name: cookieName, Value: session})
			out := httptest.NewRecorder()
			app.ServeHTTP(out, r)
			if out.Code != test.want {
				t.Fatal(out.Code, out.Body.String())
			}
			if !test.skipRecord && (!bytes.Contains(out.Body.Bytes(), []byte(`"name":"authoritative"`)) || bytes.Contains(out.Body.Bytes(), []byte(`"name":"service-output"`))) {
				t.Fatal("response did not use the Dune-owned Runner", out.Body.String())
			}
		})
	}
}

func TestManagedMutationsPersistDuneAccessState(t *testing.T) {
	ctx := context.Background()
	store, err := OpenStore(filepath.Join(t.TempDir(), "metadata"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	local := internalidentity.NewLocal(store, true)
	user, session, err := local.Register(ctx, "owner@example.test", "a-strong-test-password")
	if err != nil {
		t.Fatal(err)
	}
	token, _, err := store.IssueManagedEnrollment(ctx, user, user.ID, runner.Runner{ID: "managed-runner", Name: "managed", Kind: "managed"}, "provider")
	if err != nil {
		t.Fatal(err)
	}
	machine, credential, err := store.Enroll(ctx, token, "linux", "amd64")
	if err != nil {
		t.Fatal(err)
	}
	app, err := newTestServer(ctx, Options{DialGateway: noGateway, PublicURL: "http://dune.example.test", Managed: managedMutationStub{store: store, owner: user.ID}}, store, local)
	if err != nil {
		t.Fatal(err)
	}
	defer app.Close()

	request := func(method, path string) *httptest.ResponseRecorder {
		r := httptest.NewRequest(method, path, bytes.NewBufferString(`{"request_key":"request"}`))
		r.Header.Set("Content-Type", "application/json")
		r.Header.Set("Origin", "http://dune.example.test")
		r.Header.Set("X-Dune-Request", "1")
		r.AddCookie(&http.Cookie{Name: cookieName, Value: session})
		out := httptest.NewRecorder()
		app.ServeHTTP(out, r)
		if out.Code != http.StatusAccepted {
			t.Fatalf("%s %s: %d %s", method, path, out.Code, out.Body.String())
		}
		return out
	}

	request(http.MethodPost, "/api/managed/runners/managed-runner/pause")
	if got, err := store.MachineCredential(ctx, credential); err != nil || got != machine.ID {
		t.Fatal("pause prevented fabricd from reconnecting behind the user gate", got, err)
	}
	request(http.MethodPost, "/api/managed/runners/managed-runner/resume")
	if got, err := store.MachineCredential(ctx, credential); err != nil || got != machine.ID {
		t.Fatal("resume did not restore the persisted machine credential", got, err)
	}
	request(http.MethodDelete, "/api/managed/runners/managed-runner")
	if _, err := store.RevokeManaged(ctx, user.ID, "managed-runner"); err != nil {
		t.Fatal("managed revoke was not idempotent for provider cleanup retry", err)
	}
	if _, err := store.MachineCredential(ctx, credential); !errors.Is(err, identity.ErrUnauthorized) {
		t.Fatal("destroy did not revoke the persisted machine credential", err)
	}
	if _, err := store.Runner(ctx, user.ID, "managed-runner"); err == nil {
		t.Fatal("destroy left the logical Runner enabled")
	}
}

func TestManagedDestroyRevokesPendingEnrollment(t *testing.T) {
	ctx := context.Background()
	store, err := OpenStore(filepath.Join(t.TempDir(), "metadata"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	local := internalidentity.NewLocal(store, true)
	user, session, err := local.Register(ctx, "owner@example.test", "a-strong-test-password")
	if err != nil {
		t.Fatal(err)
	}
	user.Kind = "email"
	token, _, err := store.IssueManagedEnrollment(ctx, user, user.ID, runner.Runner{ID: "pending-runner", Name: "pending", Kind: "managed"}, "provider")
	if err != nil {
		t.Fatal(err)
	}
	enrollmentUser, ownerID, _, err := store.EnrollmentIdentity(ctx, token)
	if err != nil || ownerID != user.ID || enrollmentUser.Kind != "email" {
		t.Fatalf("enrollment lost its principal kind: user=%+v owner=%q error=%v", enrollmentUser, ownerID, err)
	}
	app, err := newTestServer(ctx, Options{DialGateway: noGateway, PublicURL: "http://dune.example.test", Managed: managedMutationStub{store: store, owner: user.ID}}, store, local)
	if err != nil {
		t.Fatal(err)
	}
	defer app.Close()
	r := httptest.NewRequest(http.MethodDelete, "/api/managed/runners/pending-runner", bytes.NewBufferString(`{"request_key":"request"}`))
	r.Header.Set("Content-Type", "application/json")
	r.Header.Set("Origin", "http://dune.example.test")
	r.Header.Set("X-Dune-Request", "1")
	r.AddCookie(&http.Cookie{Name: cookieName, Value: session})
	out := httptest.NewRecorder()
	app.ServeHTTP(out, r)
	if out.Code != http.StatusAccepted {
		t.Fatal(out.Code, out.Body.String())
	}
	if _, _, err := store.Enroll(ctx, token, "linux", "amd64"); !errors.Is(err, identity.ErrUnauthorized) {
		t.Fatal("destroy did not revoke the pending enrollment", err)
	}
}
