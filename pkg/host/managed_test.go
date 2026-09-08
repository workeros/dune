package host_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/aiomni/dune/pkg/fabric"
	"github.com/aiomni/dune/pkg/host"
	"github.com/aiomni/dune/pkg/observe"
)

type completeProvider struct {
	mu      sync.Mutex
	creates []fabric.CreateCall
	called  chan struct{}
}

type completeLifecycleProvider interface {
	fabric.CreateProvider
	fabric.BootstrapProvider
	fabric.InspectProvider
	fabric.RenewProvider
	fabric.DestroyProvider
	fabric.CandidateProvider
}

func (p *completeProvider) Create(_ context.Context, call fabric.CreateCall) (fabric.Observation, error) {
	p.mu.Lock()
	p.creates = append(p.creates, call)
	called := p.called
	p.mu.Unlock()
	if called != nil {
		select {
		case <-called:
		default:
			close(called)
		}
	}
	return fabric.Observation{Outcome: fabric.OutcomeUnknown}, nil
}
func (*completeProvider) ReconcileCreate(context.Context, fabric.ReconcileCall) (fabric.Observation, error) {
	return fabric.Observation{Outcome: fabric.OutcomeUnknown}, nil
}
func (*completeProvider) Bootstrap(context.Context, fabric.BootstrapCall) (fabric.Observation, error) {
	return fabric.Observation{Outcome: fabric.OutcomeUnknown}, nil
}
func (*completeProvider) ReconcileBootstrap(context.Context, fabric.BootstrapReconcileCall) (fabric.Observation, error) {
	return fabric.Observation{Outcome: fabric.OutcomeUnknown}, nil
}
func (*completeProvider) Inspect(context.Context, fabric.InspectCall) (fabric.Inspection, error) {
	return fabric.Inspection{Status: fabric.InspectionUnknown}, nil
}
func (*completeProvider) Renew(context.Context, fabric.RenewCall) (fabric.Observation, error) {
	return fabric.Observation{Outcome: fabric.OutcomeUnknown}, nil
}
func (*completeProvider) ReconcileRenew(context.Context, fabric.RenewReconcileCall) (fabric.Observation, error) {
	return fabric.Observation{Outcome: fabric.OutcomeUnknown}, nil
}
func (*completeProvider) Destroy(context.Context, fabric.DestroyCall) (fabric.Observation, error) {
	return fabric.Observation{Outcome: fabric.OutcomeUnknown}, nil
}
func (*completeProvider) ReconcileDestroy(context.Context, fabric.DestroyReconcileCall) (fabric.Observation, error) {
	return fabric.Observation{Outcome: fabric.OutcomeUnknown}, nil
}
func (*completeProvider) VerifyCandidate(context.Context, fabric.CandidateCall) (fabric.Observation, error) {
	return fabric.Observation{Outcome: fabric.OutcomeUnknown}, nil
}

func managedHostOptions(dir string, provider completeLifecycleProvider) host.Options {
	minimum, maximum := int64(1), int64(4)
	worker := host.DefaultManagedWorkerOptions()
	worker.PollInterval = 10 * time.Millisecond
	worker.LeaseTTL = time.Second
	worker.CallTimeout = time.Second
	worker.EnrollmentLifetime = time.Minute
	worker.BootstrapVersion = "test-v1"
	providers := host.ManagedProviders{
		Create: map[string]fabric.CreateProvider{"sandbox": provider}, Bootstrap: map[string]fabric.BootstrapProvider{"sandbox": provider},
		Inspect: map[string]fabric.InspectProvider{"sandbox": provider}, Renew: map[string]fabric.RenewProvider{"sandbox": provider},
		Destroy:   map[string]fabric.DestroyProvider{"sandbox": provider},
		Candidate: map[string]fabric.CandidateProvider{"sandbox": provider},
	}
	return host.Options{
		DataDir: dir, PublicURL: "http://dune.example.test/tools/", Managed: &host.ManagedOptions{
			Templates: []fabric.Template{{FabricID: "sandbox", ID: "small", Version: "v1", Name: "Small", Fields: []fabric.Field{{Name: "cpu", Label: "CPU", Type: "integer", Required: true, Minimum: &minimum, Maximum: &maximum}}}},
			Providers: providers, Worker: worker,
		},
	}
}

type drainingCompleteProvider struct {
	*completeProvider
	callMu     sync.Mutex
	calls      int
	started    chan struct{}
	release    chan struct{}
	cancelled  chan struct{}
	startOnce  sync.Once
	cancelOnce sync.Once
}

func (p *drainingCompleteProvider) Create(ctx context.Context, _ fabric.CreateCall) (fabric.Observation, error) {
	p.callMu.Lock()
	p.calls++
	p.callMu.Unlock()
	p.startOnce.Do(func() { close(p.started) })
	select {
	case <-p.release:
		return fabric.Observation{Outcome: fabric.OutcomeUnknown}, nil
	case <-ctx.Done():
		p.cancelOnce.Do(func() { close(p.cancelled) })
		return fabric.Observation{}, ctx.Err()
	}
}

type observedCompleteProvider struct {
	*completeProvider
}

func (*observedCompleteProvider) Create(context.Context, fabric.CreateCall) (fabric.Observation, error) {
	return fabric.Observation{Outcome: fabric.OutcomeSucceeded, ResourceRef: "ignored-provider-resource"}, errors.New("private provider failure")
}

func managedRequest(t *testing.T, app http.Handler, method, path, body string, cookie *http.Cookie) *httptest.ResponseRecorder {
	t.Helper()
	request := httptest.NewRequest(method, path, bytes.NewBufferString(body))
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("X-Dune-Request", "1")
	request.Header.Set("Origin", "http://dune.example.test")
	if cookie != nil {
		request.AddCookie(cookie)
	}
	response := httptest.NewRecorder()
	app.ServeHTTP(response, request)
	return response
}

func TestManagedHostPublishesAPIAndRunsRecoveryWorker(t *testing.T) {
	provider := &completeProvider{called: make(chan struct{})}
	app, err := host.Open(context.Background(), managedHostOptions(filepath.Join(t.TempDir(), "metadata"), provider))
	if err != nil {
		t.Fatal(err)
	}
	defer app.Close()
	bootstrap := managedRequest(t, app, http.MethodGet, "/tools/api/bootstrap", "", nil)
	if bootstrap.Code != http.StatusOK || !bytes.Contains(bootstrap.Body.Bytes(), []byte(`"managed":true`)) {
		t.Fatal("startup did not advertise assembled Managed capability", bootstrap.Code, bootstrap.Body.String())
	}
	registration := managedRequest(t, app, http.MethodPost, "/tools/api/auth/register", `{"email":"managed-host@example.test","password":"managed-host-password"}`, nil)
	if registration.Code != http.StatusOK || len(registration.Result().Cookies()) != 1 {
		t.Fatal("registration failed", registration.Code, registration.Body.String())
	}
	cookie := registration.Result().Cookies()[0]
	templates := managedRequest(t, app, http.MethodGet, "/tools/api/managed/templates", "", cookie)
	if templates.Code != http.StatusOK || !bytes.Contains(templates.Body.Bytes(), []byte(`"fabric_id":"sandbox"`)) || !bytes.Contains(templates.Body.Bytes(), []byte(`"id":"small"`)) {
		t.Fatal("authorized templates were not exposed", templates.Code, templates.Body.String())
	}
	creation := managedRequest(t, app, http.MethodPost, "/tools/api/managed/runners", `{"request_key":"host-create-1","request":{"name":"Development","fabric_id":"sandbox","template_id":"small","template_version":"v1","parameters":{"cpu":2}}}`, cookie)
	if creation.Code != http.StatusAccepted {
		t.Fatal("Managed creation was not accepted", creation.Code, creation.Body.String())
	}
	var created struct {
		Operation struct {
			ID       string `json:"id"`
			RunnerID string `json:"runner_id"`
		} `json:"operation"`
	}
	if err := json.Unmarshal(creation.Body.Bytes(), &created); err != nil || created.Operation.ID == "" || created.Operation.RunnerID == "" {
		t.Fatal("creation response omitted durable identity", created, err)
	}
	select {
	case <-provider.called:
	case <-time.After(3 * time.Second):
		t.Fatal("host did not run the Managed recovery worker")
	}
	var status *httptest.ResponseRecorder
	statusDeadline := time.Now().Add(3 * time.Second)
	for {
		status = managedRequest(t, app, http.MethodGet, "/tools/api/managed/operations/"+created.Operation.ID, "", cookie)
		if status.Code == http.StatusOK && bytes.Contains(status.Body.Bytes(), []byte(`"provider_outcome":"unknown"`)) {
			break
		}
		if status.Code != http.StatusOK || time.Now().After(statusDeadline) {
			t.Fatal("operation status did not reach the persisted provider result", status.Code, status.Body.String())
		}
		time.Sleep(10 * time.Millisecond)
	}
	if !bytes.Contains(status.Body.Bytes(), []byte(`"action":"create"`)) || !bytes.Contains(status.Body.Bytes(), []byte(`"stage":"creating"`)) || bytes.Contains(status.Body.Bytes(), []byte(`request_key`)) || bytes.Contains(status.Body.Bytes(), []byte(`action_key`)) {
		t.Fatal("operation status was unavailable", status.Code, status.Body.String())
	}
	review := managedRequest(t, app, http.MethodPost, "/tools/api/managed/operations/"+created.Operation.ID+"/reviews", `{"request_key":"host-review-1","mode":"reconcile","reason":"operator checked provider activity"}`, cookie)
	if review.Code != http.StatusAccepted || !bytes.Contains(review.Body.Bytes(), []byte(`"mode":"reconcile"`)) || !bytes.Contains(review.Body.Bytes(), []byte(`"reason":"operator checked provider activity"`)) || bytes.Contains(review.Body.Bytes(), []byte(`principal`)) || bytes.Contains(review.Body.Bytes(), []byte(`action_id`)) || bytes.Contains(review.Body.Bytes(), []byte(`worker`)) {
		t.Fatal("manual review was not safely accepted", review.Code, review.Body.String())
	}
	var acceptedReview struct {
		ID string `json:"id"`
	}
	if err := json.Unmarshal(review.Body.Bytes(), &acceptedReview); err != nil || acceptedReview.ID == "" {
		t.Fatal("review response omitted durable identity", acceptedReview, err)
	}
	reviewStatus := managedRequest(t, app, http.MethodGet, "/tools/api/managed/reviews/"+acceptedReview.ID, "", cookie)
	if reviewStatus.Code != http.StatusOK || !bytes.Contains(reviewStatus.Body.Bytes(), []byte(`"operation_id":"`+created.Operation.ID+`"`)) {
		t.Fatal("review status was unavailable", reviewStatus.Code, reviewStatus.Body.String())
	}
	latestReview := managedRequest(t, app, http.MethodGet, "/tools/api/managed/operations/"+created.Operation.ID+"/reviews", "", cookie)
	if latestReview.Code != http.StatusOK || !bytes.Contains(latestReview.Body.Bytes(), []byte(`"id":"`+acceptedReview.ID+`"`)) {
		t.Fatal("review was not recoverable after a page refresh", latestReview.Code, latestReview.Body.String())
	}
	runnerStatus := managedRequest(t, app, http.MethodGet, "/tools/api/managed/runners/"+created.Operation.RunnerID, "", cookie)
	if runnerStatus.Code != http.StatusOK || !bytes.Contains(runnerStatus.Body.Bytes(), []byte(`"id":"`+created.Operation.ID+`"`)) || !bytes.Contains(runnerStatus.Body.Bytes(), []byte(`"stage":"creating"`)) {
		t.Fatal("Runner lifecycle status was unavailable", runnerStatus.Code, runnerStatus.Body.String())
	}
	otherRegistration := managedRequest(t, app, http.MethodPost, "/tools/api/auth/register", `{"email":"managed-other@example.test","password":"managed-other-password"}`, nil)
	if otherRegistration.Code != http.StatusOK || len(otherRegistration.Result().Cookies()) != 1 {
		t.Fatal("second registration failed", otherRegistration.Code, otherRegistration.Body.String())
	}
	if hidden := managedRequest(t, app, http.MethodGet, "/tools/api/managed/operations/"+created.Operation.ID, "", otherRegistration.Result().Cookies()[0]); hidden.Code != http.StatusNotFound {
		t.Fatal("operation status disclosed another owner", hidden.Code, hidden.Body.String())
	}
	if hidden := managedRequest(t, app, http.MethodGet, "/tools/api/managed/reviews/"+acceptedReview.ID, "", otherRegistration.Result().Cookies()[0]); hidden.Code != http.StatusNotFound {
		t.Fatal("review status disclosed another actor", hidden.Code, hidden.Body.String())
	}
	if hidden := managedRequest(t, app, http.MethodGet, "/tools/api/managed/operations/"+created.Operation.ID+"/reviews", "", otherRegistration.Result().Cookies()[0]); hidden.Code != http.StatusNotFound {
		t.Fatal("operation review disclosed another actor", hidden.Code, hidden.Body.String())
	}
	provider.mu.Lock()
	calls := append([]fabric.CreateCall(nil), provider.creates...)
	provider.mu.Unlock()
	if len(calls) != 1 || calls[0].Action.RunnerID != created.Operation.RunnerID || calls[0].Request.TemplateID != "small" {
		t.Fatal("host worker lost configured provider scope", calls)
	}
}

func TestManagedProviderCallsEmitBoundedObservations(t *testing.T) {
	provider := &observedCompleteProvider{completeProvider: &completeProvider{}}
	events := make(chan observe.Event, 32)
	options := managedHostOptions(filepath.Join(t.TempDir(), "metadata"), provider)
	options.Observer = observe.SinkFunc(func(_ context.Context, event observe.Event) { events <- event })
	app, err := host.Open(context.Background(), options)
	if err != nil {
		t.Fatal(err)
	}
	defer app.Close()

	registration := managedRequest(t, app, http.MethodPost, "/tools/api/auth/register", `{"email":"managed-observe@example.test","password":"managed-observe-password"}`, nil)
	if registration.Code != http.StatusOK || len(registration.Result().Cookies()) != 1 {
		t.Fatal("registration failed", registration.Code, registration.Body.String())
	}
	creation := managedRequest(t, app, http.MethodPost, "/tools/api/managed/runners", `{"request_key":"host-observe-1","request":{"name":"Development","fabric_id":"sandbox","template_id":"small","template_version":"v1","parameters":{"cpu":2}}}`, registration.Result().Cookies()[0])
	if creation.Code != http.StatusAccepted {
		t.Fatal("Managed creation was not accepted", creation.Code, creation.Body.String())
	}
	var created struct {
		Operation struct {
			ID       string `json:"id"`
			RunnerID string `json:"runner_id"`
		} `json:"operation"`
	}
	if err := json.Unmarshal(creation.Body.Bytes(), &created); err != nil || created.Operation.ID == "" || created.Operation.RunnerID == "" {
		t.Fatal("creation response omitted durable identity", created, err)
	}

	deadline := time.After(3 * time.Second)
	for {
		select {
		case event := <-events:
			if event.Name != observe.ManagedProviderCall {
				continue
			}
			if event.Outcome != "unknown" || event.Operation != "create" || event.Suboperation != "dispatch" || event.OperationID != created.Operation.ID || event.ActionID == "" || event.RunnerID != created.Operation.RunnerID || event.FabricID != "sandbox" || event.Revision < 1 || event.DurationMicros < 1 {
				t.Fatal("Managed provider event lost authoritative call identity", event)
			}
			if event.ResourceRef != "" || event.PrincipalID != "" || event.RequestID != "" {
				t.Fatal("Managed provider event exposed unvalidated or unrelated data", event)
			}
			encoded, err := json.Marshal(event)
			if err != nil || bytes.Contains(encoded, []byte("ignored-provider-resource")) || bytes.Contains(encoded, []byte("private provider failure")) {
				t.Fatal("Managed provider event exposed adapter output", string(encoded), err)
			}
			statusDeadline := time.Now().Add(3 * time.Second)
			for {
				status, err := app.ManagedStatusSnapshot(context.Background())
				if err != nil {
					t.Fatal(err)
				}
				if status.Enabled && !status.CheckedAt.IsZero() && status.Runners == 1 && status.UnknownOperations == 1 {
					break
				}
				if time.Now().After(statusDeadline) {
					t.Fatal("host status did not expose persisted uncertainty", status)
				}
				time.Sleep(10 * time.Millisecond)
			}
			return
		case <-deadline:
			t.Fatal("host did not emit the Managed provider call")
		}
	}
}

func TestManagedStatusSnapshotReportsDisabledConfiguration(t *testing.T) {
	app, err := host.Open(context.Background(), host.Options{DataDir: filepath.Join(t.TempDir(), "metadata"), PublicURL: "http://dune.example.test/"})
	if err != nil {
		t.Fatal(err)
	}
	defer app.Close()
	status, err := app.ManagedStatusSnapshot(context.Background())
	if err != nil || status.Enabled || !status.CheckedAt.IsZero() || status.Runners != 0 {
		t.Fatal("plain host reported Managed lifecycle state", status, err)
	}
}

func TestManagedWorkerParticipatesInHostShutdown(t *testing.T) {
	for _, finish := range []bool{true, false} {
		t.Run(fmt.Sprintf("finish=%v", finish), func(t *testing.T) {
			dir := filepath.Join(t.TempDir(), "metadata")
			provider := &drainingCompleteProvider{
				completeProvider: &completeProvider{},
				started:          make(chan struct{}),
				release:          make(chan struct{}),
				cancelled:        make(chan struct{}),
			}
			options := managedHostOptions(dir, provider)
			options.Managed.Worker.CallTimeout = 5 * time.Second
			app, err := host.Open(context.Background(), options)
			if err != nil {
				t.Fatal(err)
			}
			defer app.Close()
			registration := managedRequest(t, app, http.MethodPost, "/tools/api/auth/register", `{"email":"managed-drain@example.test","password":"managed-drain-password"}`, nil)
			if registration.Code != http.StatusOK || len(registration.Result().Cookies()) != 1 {
				t.Fatal("registration failed", registration.Code, registration.Body.String())
			}
			cookie := registration.Result().Cookies()[0]
			create := func(key string) {
				t.Helper()
				response := managedRequest(t, app, http.MethodPost, "/tools/api/managed/runners", `{"request_key":"`+key+`","request":{"name":"Development","fabric_id":"sandbox","template_id":"small","template_version":"v1","parameters":{"cpu":2}}}`, cookie)
				if response.Code != http.StatusAccepted {
					t.Fatal("Managed creation was not accepted", response.Code, response.Body.String())
				}
			}
			create("managed-drain-1")
			select {
			case <-provider.started:
			case <-time.After(2 * time.Second):
				t.Fatal("Managed worker did not start the provider call")
			}
			if finish {
				create("managed-drain-2")
			}
			budget := 200 * time.Millisecond
			if finish {
				budget = 2 * time.Second
			}
			ctx, cancel := context.WithTimeout(context.Background(), budget)
			defer cancel()
			done := make(chan error, 1)
			go func() { done <- app.Shutdown(ctx) }()
			waitReadiness(t, app, func(readiness host.Readiness) bool { return readiness.Draining })
			if finish {
				select {
				case err := <-done:
					t.Fatal("shutdown interrupted the accepted provider call", err)
				case <-time.After(30 * time.Millisecond):
				}
				close(provider.release)
			}
			select {
			case err := <-done:
				if finish && err != nil {
					t.Fatal("completed drain failed", err)
				}
				if !finish && !errors.Is(err, context.DeadlineExceeded) {
					t.Fatal("deadline did not bound worker drain", err)
				}
			case <-time.After(3 * time.Second):
				t.Fatal("worker drain did not finish")
			}
			if !finish {
				select {
				case <-provider.cancelled:
				case <-time.After(time.Second):
					t.Fatal("deadline did not cancel the provider call")
				}
			}
			provider.callMu.Lock()
			calls := provider.calls
			provider.callMu.Unlock()
			if calls != 1 {
				t.Fatal("draining host started another provider call", calls)
			}
			await(t, app.Done())
		})
	}
}

func TestManagedHostRejectsIncompleteConfigurationAndReleasesStore(t *testing.T) {
	provider := &completeProvider{}
	dir := filepath.Join(t.TempDir(), "metadata")
	options := managedHostOptions(dir, provider)
	delete(options.Managed.Providers.Destroy, "sandbox")
	if app, err := host.Open(context.Background(), options); err == nil {
		app.Close()
		t.Fatal("host accepted a provider without Destroy")
	}
	missingReview := managedHostOptions(filepath.Join(t.TempDir(), "metadata-review"), provider)
	delete(missingReview.Managed.Providers.Candidate, "sandbox")
	if app, err := host.Open(context.Background(), missingReview); err == nil {
		app.Close()
		t.Fatal("host accepted a provider without candidate verification")
	}
	invalidRetention := managedHostOptions(filepath.Join(t.TempDir(), "metadata-retention"), provider)
	invalidRetention.Managed.Worker.HistoryRetention = 23 * time.Hour
	if app, err := host.Open(context.Background(), invalidRetention); err == nil {
		app.Close()
		t.Fatal("host accepted a recovery window shorter than one day")
	}
	valid := managedHostOptions(dir, provider)
	valid.Managed.Worker.BootstrapVersion = ""
	if app, err := host.Open(context.Background(), valid); err == nil {
		app.Close()
		t.Fatal("host accepted Managed without a bootstrap artifact version")
	}
	// A failed assembly must release the SQLite ownership lock.
	plain, err := host.Open(context.Background(), host.Options{DataDir: dir, PublicURL: "http://dune.example.test/"})
	if err != nil {
		t.Fatal("failed Managed assembly retained the metadata store", err)
	}
	plain.Close()
}
