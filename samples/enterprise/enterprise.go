// Package enterprisehost shows how a private application composes Dune from
// public packages. The application supplies its own trusted integrations and
// keeps ownership of the HTTP server, provider credentials and database secret.
package enterprisehost

import (
	"context"
	"fmt"
	"net/http"
	"time"

	"github.com/aiomni/dune/pkg/access"
	"github.com/aiomni/dune/pkg/deployment"
	"github.com/aiomni/dune/pkg/fabric"
	"github.com/aiomni/dune/pkg/host"
	"github.com/aiomni/dune/pkg/identity"
	"github.com/aiomni/dune/pkg/renewal"
	"github.com/aiomni/dune/pkg/storage"
	"github.com/jackc/pgx/v5"
)

// FabricProvider is the complete contract required for one Managed namespace.
// A private adapter can implement the capabilities with one value or delegate
// them to smaller SDK clients internally.
type FabricProvider interface {
	fabric.AvailabilityProvider
	fabric.CreateProvider
	fabric.BootstrapProvider
	fabric.InspectProvider
	fabric.RenewProvider
	fabric.DestroyProvider
	fabric.CandidateProvider
}

// Integrations are application-owned extension points. Dune never reads their
// private configuration or credentials.
type Integrations struct {
	Identity        identity.Provider
	Access          access.Checker
	Providers       map[string]FabricProvider
	Renewal         renewal.Policy
	BeforeConnect   func(context.Context, *pgx.ConnConfig) error
	SessionLifetime time.Duration
}

// Config contains public Dune configuration plus the application's private
// integration instances. ConfigurationVersion must change when private identity,
// access, provider or database-authentication behavior changes incompatibly.
type Config struct {
	PublicURL, GatewayURL string
	Assets, Binaries      string
	PostgresURL           string
	ConfigurationVersion  string
	RenewalPolicyVersion  string
	Templates             []fabric.Template
	Worker                host.ManagedWorkerOptions
	Cluster               *host.ClusterOptions
	Integrations          Integrations
}

// Application exposes the Dune App for lifecycle and trusted administrative
// methods, and a handler ready to mount on an application-owned HTTP server.
type Application struct {
	Dune    *host.App
	Handler http.Handler
}

// Open validates the public composition and mounts the existing workbench next
// to an application-owned health route. The caller owns the HTTP server and must
// close Dune during shutdown.
func Open(ctx context.Context, config Config) (*Application, error) {
	addresses, err := deployment.NewURLs(config.PublicURL, config.GatewayURL)
	if err != nil {
		return nil, err
	}
	if config.PostgresURL == "" || config.ConfigurationVersion == "" || config.RenewalPolicyVersion == "" {
		return nil, fmt.Errorf("enterprise host requires PostgreSQL and explicit configuration versions")
	}
	if config.Integrations.Identity == nil || config.Integrations.Access == nil || config.Integrations.Renewal == nil {
		return nil, fmt.Errorf("enterprise host requires identity, access and renewal integrations")
	}
	if len(config.Integrations.Providers) == 0 {
		return nil, fmt.Errorf("enterprise host requires at least one Managed provider")
	}

	providers := host.ManagedProviders{
		Availability: make(map[string]fabric.AvailabilityProvider, len(config.Integrations.Providers)),
		Create:       make(map[string]fabric.CreateProvider, len(config.Integrations.Providers)),
		Bootstrap:    make(map[string]fabric.BootstrapProvider, len(config.Integrations.Providers)),
		Inspect:      make(map[string]fabric.InspectProvider, len(config.Integrations.Providers)),
		Renew:        make(map[string]fabric.RenewProvider, len(config.Integrations.Providers)),
		Destroy:      make(map[string]fabric.DestroyProvider, len(config.Integrations.Providers)),
		Candidate:    make(map[string]fabric.CandidateProvider, len(config.Integrations.Providers)),
	}
	for id, provider := range config.Integrations.Providers {
		providers.Availability[id] = provider
		providers.Create[id] = provider
		providers.Bootstrap[id] = provider
		providers.Inspect[id] = provider
		providers.Renew[id] = provider
		providers.Destroy[id] = provider
		providers.Candidate[id] = provider
	}
	worker := config.Worker
	if worker == (host.ManagedWorkerOptions{}) {
		worker = host.DefaultManagedWorkerOptions()
	}
	worker.RenewalPolicyVersion = config.RenewalPolicyVersion

	dune, err := host.Open(ctx, host.Options{
		Database: &storage.Config{Postgres: &storage.Postgres{
			URL:           config.PostgresURL,
			BeforeConnect: config.Integrations.BeforeConnect,
		}},
		Cluster:             config.Cluster,
		Assets:              config.Assets,
		Binaries:            config.Binaries,
		PublicURL:           addresses.PublicURL,
		GatewayURL:          addresses.GatewayURL,
		DisableRegistration: true,
		Identity: &identity.Options{
			Provider:        config.Integrations.Identity,
			SessionLifetime: config.Integrations.SessionLifetime,
		},
		AccessChecker:        config.Integrations.Access,
		ConfigurationVersion: config.ConfigurationVersion,
		Managed: &host.ManagedOptions{
			Templates:     config.Templates,
			Providers:     providers,
			Worker:        worker,
			RenewalPolicy: config.Integrations.Renewal,
		},
	})
	if err != nil {
		return nil, err
	}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /health", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	})
	mux.Handle(addresses.Path, dune)
	return &Application{Dune: dune, Handler: mux}, nil
}
