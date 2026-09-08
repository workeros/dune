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
	"github.com/aiomni/dune/internal/lifecycle"
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

type availabilityProvider struct {
	mu     sync.Mutex
	status fabric.Availability
	err    error
	calls  int
}

func (p *availabilityProvider) Availability(context.Context) (fabric.Availability, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.calls++
	return p.status, p.err
}

func (p *availabilityProvider) set(status fabric.Availability, err error) {
	p.mu.Lock()
	p.status, p.err = status, err
	p.mu.Unlock()
}

func (p *availabilityProvider) callCount() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.calls
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
	return newServiceWithAvailability(t, fixture, checker, sessions, &availabilityProvider{status: fabric.Availability{Available: true}})
}

func newServiceWithAvailability(t *testing.T, fixture serviceFixture, checker access.Checker, sessions authorization.Sessions, provider fabric.AvailabilityProvider) *Service {
	t.Helper()
	if sessions == nil {
		sessions = fixture.session
	}
	service, err := New(fixture.catalog, map[string]fabric.AvailabilityProvider{"sandbox": provider}, sessions, authorization.New(fixture.ctx, sessions, fixture.store, checker), fixture.store, wire.ID())
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
	provider := &availabilityProvider{status: fabric.Availability{Available: true}}
	service := newServiceWithAvailability(t, fixture, checker, nil, provider)

	templates, err := service.Templates(fixture.ctx, fixture.cookie)
	if err != nil {
		t.Fatal(err)
	}
	if len(templates) != 1 || templates[0].ID != "visible" {
		t.Fatal("discovery disclosed a denied or disabled template", templates)
	}
	if !templates[0].Available || templates[0].Reason != "" {
		t.Fatal("discovery lost provider availability", templates[0])
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
	if calls := provider.callCount(); calls != 1 {
		t.Fatal("denied or disabled template disclosed provider state", calls)
	}
}

func TestProviderAvailabilityGatesOnlyNewManagedIntent(t *testing.T) {
	fixture := newServiceFixture(t)
	provider := &availabilityProvider{status: fabric.Availability{Reason: fabric.AvailabilityCapacity}}
	service := newServiceWithAvailability(t, fixture, checkFunc(func(context.Context, access.Request) (access.Decision, error) {
		return allowDecision(), nil
	}), nil, provider)

	templates, err := service.Templates(fixture.ctx, fixture.cookie)
	if err != nil {
		t.Fatal(err)
	}
	if len(templates) != 2 || templates[0].Available || templates[0].Reason != fabric.AvailabilityCapacity || templates[1].Available || templates[1].Reason != fabric.AvailabilityCapacity {
		t.Fatal("unavailable authorized templates were hidden or mislabeled", templates)
	}
	if calls := provider.callCount(); calls != 1 {
		t.Fatal("template discovery checked one Fabric more than once", calls)
	}
	if _, err := service.Create(fixture.ctx, fixture.cookie, "unavailable", createRequest("visible")); !errors.Is(err, fabric.ErrProviderUnavailable) {
		t.Fatal("creation ignored current provider capacity", err)
	}
	if _, err := fixture.store.ManagedCreation(fixture.ctx, fixture.user.ID, "unavailable"); !errors.Is(err, metadata.ErrNotFound) {
		t.Fatal("unavailable provider left a durable creation intent", err)
	}

	provider.set(fabric.Availability{Available: true}, nil)
	if _, err := service.Create(fixture.ctx, fixture.cookie, "recovered", createRequest("visible")); err != nil {
		t.Fatal("provider recovery required a host restart", err)
	}
	provider.set(fabric.Availability{Available: true}, errors.New("private provider error"))
	status, err := service.Template(fixture.ctx, fixture.cookie, "sandbox", "visible", "v1")
	if err != nil || status.Available || status.Reason != fabric.AvailabilityUnreachable {
		t.Fatal("provider error was not normalized", status, err)
	}
	provider.set(fabric.Availability{Reason: "private-reason"}, nil)
	status, err = service.Template(fixture.ctx, fixture.cookie, "sandbox", "visible", "v1")
	if err != nil || status.Available || status.Reason != fabric.AvailabilityUnknown {
		t.Fatal("invalid provider reason escaped the fixed vocabulary", status, err)
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
		catalog      *fabric.Catalog
		availability map[string]fabric.AvailabilityProvider
		sessions     authorization.Sessions
		checks       *authorization.Service
		store        *metadata.Store
	}{
		"catalog":      {nil, map[string]fabric.AvailabilityProvider{"sandbox": &availabilityProvider{}}, fixture.session, checks, fixture.store},
		"availability": {fixture.catalog, nil, fixture.session, checks, fixture.store},
		"sessions":     {fixture.catalog, map[string]fabric.AvailabilityProvider{"sandbox": &availabilityProvider{}}, nil, checks, fixture.store},
		"checks":       {fixture.catalog, map[string]fabric.AvailabilityProvider{"sandbox": &availabilityProvider{}}, fixture.session, nil, fixture.store},
		"store":        {fixture.catalog, map[string]fabric.AvailabilityProvider{"sandbox": &availabilityProvider{}}, fixture.session, checks, nil},
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := New(args.catalog, args.availability, args.sessions, args.checks, args.store, wire.ID()); err == nil {
				t.Fatal("incomplete assembly accepted")
			}
		})
	}
	availability := map[string]fabric.AvailabilityProvider{"sandbox": &availabilityProvider{}}
	if _, err := New(fixture.catalog, availability, fixture.session, checks, fixture.store, "bad-instance"); err == nil {
		t.Fatal("invalid application instance accepted")
	}
	availability["sandbox"] = nil
	if _, err := New(fixture.catalog, availability, fixture.session, checks, fixture.store, wire.ID()); err == nil {
		t.Fatal("nil availability provider accepted")
	}
}

func TestDestroyChecksManagedRunnerAndClosesAccess(t *testing.T) {
	fixture := newServiceFixture(t)
	created := readyResourceForInspection(t, fixture)
	var mu sync.Mutex
	var requests []access.Request
	service := newService(t, fixture, checkFunc(func(_ context.Context, request access.Request) (access.Decision, error) {
		mu.Lock()
		requests = append(requests, request)
		mu.Unlock()
		return allowDecision(), nil
	}), nil)
	destroyed, err := service.Destroy(fixture.ctx, fixture.cookie, "destroy-1", created.RunnerID, time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	if destroyed.Action != "destroy" || destroyed.RunnerID != created.RunnerID || !destroyed.Exclusive || destroyed.AccessCloseOutcome != lifecycle.AccessCloseWaiting {
		t.Fatal("destroy service did not return durable acceptance", destroyed)
	}
	mu.Lock()
	got := append([]access.Request(nil), requests...)
	mu.Unlock()
	if len(got) != 1 || got[0].Operation != "runner.destroy" || got[0].Suboperation != "managed" || got[0].PrincipalID != fixture.user.ID || got[0].OwnerID != fixture.user.ID || got[0].Binding.RunnerID != created.RunnerID || got[0].Binding.FabricID != "sandbox" || got[0].Binding.Revision != 1 || got[0].Binding.MachineID == "" {
		t.Fatal("destroy access decision lost its fixed Managed scope", got)
	}
	if resource, err := fixture.store.RunnerResource(fixture.ctx, created.RunnerID); err != nil || resource.Runner.Binding != nil {
		t.Fatal("destroy did not revoke the current machine", resource, err)
	}
	status, err := service.RunnerStatus(fixture.ctx, fixture.cookie, created.RunnerID)
	if err != nil || status.Stage != "closing_access" || !status.AccessClosed || status.AccessCloseOutcome != lifecycle.AccessCloseWaiting || !status.AccessCloseDeadline.Equal(destroyed.CloseDeadline) {
		t.Fatal("destroy status did not preserve access-close facts", status, err)
	}
}

func TestDestroyRejectsDeniedStaleAndAttachedRequests(t *testing.T) {
	fixture := newServiceFixture(t)
	created := readyResourceForInspection(t, fixture)
	denied := newService(t, fixture, checkFunc(func(context.Context, access.Request) (access.Decision, error) {
		return denyDecision(), nil
	}), nil)
	if _, err := denied.Destroy(fixture.ctx, fixture.cookie, "denied-destroy", created.RunnerID, time.Minute); !errors.Is(err, authorization.ErrNotFound) {
		t.Fatal("denied destroy disclosed or changed the Runner", err)
	}
	if _, err := fixture.store.ManagedDestroy(fixture.ctx, fixture.user.ID, "denied-destroy"); !errors.Is(err, metadata.ErrNotFound) {
		t.Fatal("denied destroy left durable state", err)
	}

	stale := newService(t, fixture, checkFunc(func(context.Context, access.Request) (access.Decision, error) {
		return allowDecision(), nil
	}), fixedSessions{user: fixture.user})
	if _, err := stale.Destroy(fixture.ctx, "missing-browser-session", "stale-destroy", created.RunnerID, time.Minute); !errors.Is(err, identity.ErrUnauthorized) {
		t.Fatal("destroy transaction trusted stale authentication", err)
	}
	if _, err := stale.Destroy(fixture.ctx, "missing-browser-session", "bad-timeout", created.RunnerID, -time.Second); !errors.Is(err, metadata.ErrInvalidArgument) {
		t.Fatal("destroy accepted a negative close timeout", err)
	}

	token, _, err := fixture.store.IssueEnrollment(fixture.ctx, fixture.user.ID, "attached destroy target")
	if err != nil {
		t.Fatal(err)
	}
	attached, _, err := fixture.store.Enroll(fixture.ctx, token, "linux", "arm64")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := stale.Destroy(fixture.ctx, "missing-browser-session", "attached-destroy", attached.RunnerID, time.Minute); !errors.Is(err, authorization.ErrNotFound) {
		t.Fatal("Managed destroy accepted an Attached Runner", err)
	}
}

func TestStatusShowsDurableStageWithoutProviderActionKeys(t *testing.T) {
	fixture := newServiceFixture(t)
	service := newService(t, fixture, checkFunc(func(context.Context, access.Request) (access.Decision, error) {
		return allowDecision(), nil
	}), nil)
	created, err := service.Create(fixture.ctx, fixture.cookie, "status-create", createRequest("visible"))
	if err != nil {
		t.Fatal(err)
	}
	status, err := service.Status(fixture.ctx, fixture.cookie, created.Operation.ID)
	if err != nil || status.Stage != "queued" || status.ID != created.Operation.ID || status.ResourceRef != "" {
		t.Fatal("queued status was inaccurate", status, err)
	}
	claimed, err := fixture.store.ClaimOperation(fixture.ctx, created.Operation.ID, wire.ID(), time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	action, dispatch, err := fixture.store.BeginProviderAction(fixture.ctx, claimed, lifecycle.ActionRequest{Kind: "create", Digest: claimed.Digest})
	if err != nil || !dispatch {
		t.Fatal("create action setup", dispatch, err)
	}
	if err := fixture.store.RecordProviderAction(fixture.ctx, claimed, action.ID, lifecycle.ActionObservation{Outcome: "unknown", ResourceRef: "possible-resource"}); err != nil {
		t.Fatal(err)
	}
	status, err = service.Status(fixture.ctx, fixture.cookie, created.Operation.ID)
	if err != nil || status.Stage != "creating" || status.ProviderOutcome != "unknown" || status.ResourceRef != "possible-resource" || status.Finished {
		t.Fatal("unknown provider status was inaccurate", status, err)
	}
	runnerStatus, err := service.RunnerStatus(fixture.ctx, fixture.cookie, created.Runner.ID)
	if err != nil || runnerStatus.ID != status.ID || runnerStatus.Stage != status.Stage || runnerStatus.ProviderOutcome != status.ProviderOutcome {
		t.Fatal("Runner status did not select the active lifecycle", runnerStatus, err)
	}

	other, _, err := fixture.session.Register(fixture.ctx, "status-other@example.test", "status-other-password")
	if err != nil {
		t.Fatal(err)
	}
	otherSession := fixedSessions{user: other}
	otherService := newService(t, fixture, checkFunc(func(context.Context, access.Request) (access.Decision, error) {
		return allowDecision(), nil
	}), otherSession)
	if _, err := otherService.Status(fixture.ctx, "ignored", created.Operation.ID); !errors.Is(err, authorization.ErrNotFound) {
		t.Fatal("status disclosed another actor's operation", err)
	}
	sharedStatus, err := otherService.RunnerStatus(fixture.ctx, "ignored", created.Runner.ID)
	if err != nil || sharedStatus.ID != created.Operation.ID || sharedStatus.Stage != "creating" {
		t.Fatal("authorized collaborator could not read the Runner lifecycle", sharedStatus, err)
	}
}
