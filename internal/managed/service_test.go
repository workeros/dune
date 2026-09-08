package managed

import (
	"context"
	"encoding/json"
	"errors"
	"path/filepath"
	"reflect"
	"sync"
	"testing"
	"time"

	"github.com/aiomni/dune/internal/authorization"
	"github.com/aiomni/dune/internal/identity"
	"github.com/aiomni/dune/internal/metadata"
	"github.com/aiomni/dune/internal/wire"
	"github.com/aiomni/dune/pkg/access"
	"github.com/aiomni/dune/pkg/fabric"
	"github.com/aiomni/dune/pkg/storage"
)

type checkFunc func(context.Context, access.Request) (access.Decision, error)

func (f checkFunc) Check(ctx context.Context, request access.Request) (access.Decision, error) {
	return f(ctx, request)
}

func allowDecision() access.Decision {
	return access.Decision{Allowed: true, Reason: "ALLOW", ID: wire.ID(), ValidUntil: time.Now().Add(time.Minute)}
}

func denyDecision() access.Decision {
	return access.Decision{Reason: "DENY", ID: wire.ID(), ValidUntil: time.Now().Add(time.Minute)}
}

func templateFixture(id string) fabric.Template {
	minimum, maximum := int64(1), int64(8)
	return fabric.Template{
		FabricID: "sandbox", ID: id, Version: "v1", Name: id + " environment",
		Fields: []fabric.Field{
			{Name: "cpu", Label: "CPU cores", Type: "integer", Required: true, Minimum: &minimum, Maximum: &maximum},
			{Name: "region", Label: "Region", Type: "string", Required: true, MaxLength: 16, Choices: []string{"east", "west"}},
		},
	}
}

func createRequest(id string) fabric.CreateRequest {
	return fabric.CreateRequest{
		Name: " Development ", FabricID: "sandbox", TemplateID: id, TemplateVersion: "v1",
		Parameters: map[string]json.RawMessage{"cpu": json.RawMessage(`2`), "region": json.RawMessage(`"east"`)},
	}
}

type serviceFixture struct {
	ctx     context.Context
	cancel  context.CancelFunc
	store   *metadata.Store
	session *identity.Local
	user    identity.User
	cookie  string
	catalog *fabric.Catalog
}

func newServiceFixture(t *testing.T) serviceFixture {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	store, err := metadata.Open(ctx, storage.Config{SQLiteDir: filepath.Join(t.TempDir(), "metadata")})
	if err != nil {
		cancel()
		t.Fatal(err)
	}
	session := identity.NewLocal(store, true)
	user, cookie, err := session.Register(ctx, "managed-service@example.test", "managed-service-password")
	if err != nil {
		store.Close()
		cancel()
		t.Fatal(err)
	}
	disabled := templateFixture("disabled")
	disabled.Disabled = true
	catalog, err := fabric.NewCatalog([]fabric.Template{templateFixture("visible"), templateFixture("hidden"), disabled})
	if err != nil {
		store.Close()
		cancel()
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := store.Close(); err != nil {
			t.Error(err)
		}
		cancel()
	})
	return serviceFixture{ctx: ctx, cancel: cancel, store: store, session: session, user: user, cookie: cookie, catalog: catalog}
}

func newService(t *testing.T, fixture serviceFixture, checker access.Checker, sessions authorization.Sessions) *Service {
	t.Helper()
	if sessions == nil {
		sessions = fixture.session
	}
	service, err := New(fixture.catalog, sessions, authorization.New(fixture.ctx, sessions, fixture.store, checker), fixture.store)
	if err != nil {
		t.Fatal(err)
	}
	return service
}

func TestTemplateDiscoveryFiltersDeniedEntries(t *testing.T) {
	fixture := newServiceFixture(t)
	var mu sync.Mutex
	var requests []access.Request
	checker := checkFunc(func(_ context.Context, request access.Request) (access.Decision, error) {
		mu.Lock()
		requests = append(requests, request)
		mu.Unlock()
		if request.Resource.TemplateID == "hidden" {
			return denyDecision(), nil
		}
		return allowDecision(), nil
	})
	service := newService(t, fixture, checker, nil)

	templates, err := service.Templates(fixture.ctx, fixture.cookie)
	if err != nil {
		t.Fatal(err)
	}
	if len(templates) != 1 || templates[0].ID != "visible" {
		t.Fatal("discovery disclosed a denied or disabled template", templates)
	}
	mu.Lock()
	gotRequests := append([]access.Request(nil), requests...)
	mu.Unlock()
	if len(gotRequests) != 2 {
		t.Fatal("discovery did not check every available candidate", len(gotRequests))
	}
	for _, request := range gotRequests {
		if request.Operation != "template.list" || request.Suboperation != "managed" || request.PrincipalID != fixture.user.ID || request.OwnerID != fixture.user.ID || request.Resource.FabricID != "sandbox" || request.Resource.TemplateVersion != "v1" {
			t.Fatal("access request did not use fixed identity and configured template", request)
		}
	}
	if _, err := service.Template(fixture.ctx, fixture.cookie, "sandbox", "hidden", "v1"); !errors.Is(err, fabric.ErrTemplateNotFound) {
		t.Fatal("denied template was distinguishable from a missing template", err)
	}
	if _, err := service.Template(fixture.ctx, fixture.cookie, "sandbox", "disabled", "v1"); !errors.Is(err, fabric.ErrTemplateNotFound) {
		t.Fatal("disabled template remained available", err)
	}
}

func TestCreateChecksConfiguredTemplateAndPersistsOnlyValidatedIntent(t *testing.T) {
	fixture := newServiceFixture(t)
	var mu sync.Mutex
	var requests []access.Request
	checker := checkFunc(func(_ context.Context, request access.Request) (access.Decision, error) {
		mu.Lock()
		requests = append(requests, request)
		mu.Unlock()
		return allowDecision(), nil
	})
	service := newService(t, fixture, checker, nil)
	request := createRequest("visible")
	created, err := service.Create(fixture.ctx, fixture.cookie, "create-1", request)
	if err != nil {
		t.Fatal(err)
	}
	if created.Runner.Name != "Development" || created.Runner.Kind != "managed" || created.Runner.Binding != nil || created.Operation.Action != "create" {
		t.Fatal("creation was not persisted as an unbound managed intent", created)
	}
	request.Parameters["cpu"][0] = '7'
	delete(request.Parameters, "region")
	recovered, err := fixture.store.ManagedCreation(fixture.ctx, fixture.user.ID, "create-1")
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(recovered, created) || string(recovered.Spec.Parameters["cpu"]) != "2" {
		t.Fatal("durable intent aliases or differs from validated input", recovered)
	}
	mu.Lock()
	gotRequests := append([]access.Request(nil), requests...)
	mu.Unlock()
	if len(gotRequests) != 2 || gotRequests[0].Operation != "template.get" || gotRequests[1].Operation != "runner.create" {
		t.Fatal("creation did not perform both access decisions in order", gotRequests)
	}
	for _, checked := range gotRequests {
		if checked.Resource.FabricID != "sandbox" || checked.Resource.TemplateID != "visible" || checked.Resource.TemplateVersion != "v1" || checked.Resource.Path != "" || checked.Resource.Destination != "" || checked.Resource.ConfigID != "" {
			t.Fatal("access checker received input outside the configured selection", checked)
		}
	}

	invalid := createRequest("visible")
	invalid.Parameters["cpu"] = json.RawMessage(`9`)
	if _, err := service.Create(fixture.ctx, fixture.cookie, "invalid", invalid); !errors.Is(err, fabric.ErrInvalidParameters) {
		t.Fatal("out-of-range input was accepted", err)
	}
	if _, err := fixture.store.ManagedCreation(fixture.ctx, fixture.user.ID, "invalid"); !errors.Is(err, metadata.ErrNotFound) {
		t.Fatal("invalid input left a durable intent", err)
	}
}

func TestTemplateDiscoveryFailsClosedWhenPolicyIsUnavailable(t *testing.T) {
	fixture := newServiceFixture(t)
	service := newService(t, fixture, checkFunc(func(_ context.Context, request access.Request) (access.Decision, error) {
		if request.Resource.TemplateID == "visible" {
			return access.Decision{}, errors.New("policy backend unavailable")
		}
		return allowDecision(), nil
	}), nil)
	if templates, err := service.Templates(fixture.ctx, fixture.cookie); !errors.Is(err, access.ErrUnavailable) || templates != nil {
		t.Fatal("policy failure was hidden as an ordinary filtered entry", templates, err)
	}
}

func TestCreateRejectsPolicyAndRechecksBrowserSessionInTransaction(t *testing.T) {
	fixture := newServiceFixture(t)
	denied := newService(t, fixture, checkFunc(func(_ context.Context, request access.Request) (access.Decision, error) {
		if request.Operation == "runner.create" {
			return denyDecision(), nil
		}
		return allowDecision(), nil
	}), nil)
	if _, err := denied.Create(fixture.ctx, fixture.cookie, "denied", createRequest("visible")); !errors.Is(err, access.ErrDenied) {
		t.Fatal("creation ignored the dedicated policy decision", err)
	}
	if _, err := fixture.store.ManagedCreation(fixture.ctx, fixture.user.ID, "denied"); !errors.Is(err, metadata.ErrNotFound) {
		t.Fatal("denied creation left a durable intent", err)
	}

	stale := fixedSessions{user: fixture.user}
	service := newService(t, fixture, checkFunc(func(context.Context, access.Request) (access.Decision, error) {
		return allowDecision(), nil
	}), stale)
	if _, err := service.Create(fixture.ctx, "missing-browser-session", "stale", createRequest("visible")); !errors.Is(err, identity.ErrUnauthorized) {
		t.Fatal("transaction trusted the service's earlier authentication result", err)
	}
	if _, err := fixture.store.ManagedCreation(fixture.ctx, fixture.user.ID, "stale"); !errors.Is(err, metadata.ErrNotFound) {
		t.Fatal("stale session left a durable intent", err)
	}
}

type fixedSessions struct{ user identity.User }

func (s fixedSessions) Authenticate(context.Context, string) (identity.User, error) {
	return s.user, nil
}
func (s fixedSessions) AuthenticateCLI(context.Context, string) (identity.User, error) {
	return identity.User{}, identity.ErrUnauthorized
}
func (s fixedSessions) Namespace() string { return s.user.Namespace }

func TestNewRequiresCompleteManagedAssembly(t *testing.T) {
	fixture := newServiceFixture(t)
	checks := authorization.New(fixture.ctx, fixture.session, fixture.store, access.Owner{})
	for name, args := range map[string]struct {
		catalog  *fabric.Catalog
		sessions authorization.Sessions
		checks   *authorization.Service
		store    *metadata.Store
	}{
		"catalog":  {nil, fixture.session, checks, fixture.store},
		"sessions": {fixture.catalog, nil, checks, fixture.store},
		"checks":   {fixture.catalog, fixture.session, nil, fixture.store},
		"store":    {fixture.catalog, fixture.session, checks, nil},
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := New(args.catalog, args.sessions, args.checks, args.store); err == nil {
				t.Fatal("incomplete assembly accepted")
			}
		})
	}
}
