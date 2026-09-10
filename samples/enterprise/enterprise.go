// Package enterprisehost shows how a private application composes Dune from
// public packages. The application owns identity, Managed lifecycle state,
// provider credentials and the HTTP server.
package enterprisehost

import (
	"context"
	"fmt"
	"net/http"

	"github.com/aiomni/dune/pkg/access"
	"github.com/aiomni/dune/pkg/deployment"
	"github.com/aiomni/dune/pkg/host"
	"github.com/aiomni/dune/pkg/identity"
	"github.com/aiomni/dune/pkg/managed"
	"github.com/aiomni/dune/pkg/storage"
	"github.com/jackc/pgx/v5"
)

type Integrations struct {
	Identity      identity.Service
	Access        access.Checker
	Managed       managed.Service
	BeforeConnect func(context.Context, *pgx.ConnConfig) error
}

type Config struct {
	PublicURL, GatewayURL string
	Assets, Binaries      string
	PostgresURL           string
	Cluster               *host.ClusterOptions
	Integrations          Integrations
}

type Application struct {
	Dune    *host.App
	Handler http.Handler
}

func Open(ctx context.Context, config Config) (*Application, error) {
	addresses, err := deployment.NewURLs(config.PublicURL, config.GatewayURL)
	if err != nil {
		return nil, err
	}
	if config.PostgresURL == "" {
		return nil, fmt.Errorf("enterprise host requires PostgreSQL")
	}
	if config.Integrations.Identity == nil || config.Integrations.Access == nil {
		return nil, fmt.Errorf("enterprise host requires identity and access integrations")
	}
	dune, err := host.Open(ctx, host.Options{
		Database: &storage.Config{Postgres: &storage.Postgres{
			URL:           config.PostgresURL,
			BeforeConnect: config.Integrations.BeforeConnect,
		}},
		Cluster:       config.Cluster,
		Assets:        config.Assets,
		Binaries:      config.Binaries,
		PublicURL:     addresses.PublicURL,
		GatewayURL:    addresses.GatewayURL,
		Identity:      config.Integrations.Identity,
		AccessChecker: config.Integrations.Access,
		Managed:       config.Integrations.Managed,
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
