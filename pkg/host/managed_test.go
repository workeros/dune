package host_test

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/aiomni/dune/pkg/fabric"
	"github.com/aiomni/dune/pkg/host"
)

type completeProvider struct {
	mu      sync.Mutex
	creates []fabric.CreateCall
	called  chan struct{}
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

func managedHostOptions(dir string, provider *completeProvider) host.Options {
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
		Destroy: map[string]fabric.DestroyProvider{"sandbox": provider},
	}
	return host.Options{
		DataDir: dir, PublicURL: "http://dune.example.test/tools/", Managed: &host.ManagedOptions{
			Templates: []fabric.Template{{FabricID: "sandbox", ID: "small", Version: "v1", Name: "Small", Fields: []fabric.Field{{Name: "cpu", Label: "CPU", Type: "integer", Required: true, Minimum: &minimum, Maximum: &maximum}}}},
			Providers: providers, Worker: worker,
		},
	}
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
	status := managedRequest(t, app, http.MethodGet, "/tools/api/managed/operations/"+created.Operation.ID, "", cookie)
	if status.Code != http.StatusOK || !bytes.Contains(status.Body.Bytes(), []byte(`"action":"create"`)) || !bytes.Contains(status.Body.Bytes(), []byte(`"stage":"creating"`)) || !bytes.Contains(status.Body.Bytes(), []byte(`"provider_outcome":"unknown"`)) || bytes.Contains(status.Body.Bytes(), []byte(`request_key`)) || bytes.Contains(status.Body.Bytes(), []byte(`action_key`)) {
		t.Fatal("operation status was unavailable", status.Code, status.Body.String())
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
	provider.mu.Lock()
	calls := append([]fabric.CreateCall(nil), provider.creates...)
	provider.mu.Unlock()
	if len(calls) != 1 || calls[0].Action.RunnerID != created.Operation.RunnerID || calls[0].Request.TemplateID != "small" {
		t.Fatal("host worker lost configured provider scope", calls)
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
