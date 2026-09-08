package tests

import (
	"context"
	"errors"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/aiomni/dune/internal/metadata"
	"github.com/aiomni/dune/pkg/access"
	"github.com/aiomni/dune/pkg/host"
	"github.com/jackc/pgx/v5"
)

func TestPostgresHostAdmission(t *testing.T) {
	database := postgresWorkbenchConfig(t)
	ctx, cancel := context.WithTimeout(context.Background(), 35*time.Second)
	defer cancel()
	parsed, err := pgx.ParseConfig(database.Postgres.URL)
	must(t, err)
	must(t, database.Postgres.BeforeConnect(ctx, parsed))
	inspect, err := pgx.ConnectConfig(ctx, parsed)
	must(t, err)
	defer inspect.Close(context.Background())
	options := host.Options{PublicURL: "http://instance-a.test/dune/", Database: &database}
	first, err := host.Open(ctx, options)
	must(t, err)
	defer first.Close()
	options.PublicURL = "http://instance-b.test/dune/"
	second, err := host.Open(ctx, options)
	must(t, err)
	defer second.Close()
	for _, mutate := range []func(*host.Options){
		func(o *host.Options) { o.ConfigurationVersion = "different" },
		func(o *host.Options) { o.DisableRegistration = true },
		func(o *host.Options) { o.PublicURL = "http://instance-b.test/another-prefix/" },
	} {
		conflicting := options
		mutate(&conflicting)
		if other, err := host.Open(ctx, conflicting); !errors.Is(err, metadata.ErrConfigurationConflict) {
			if other != nil {
				other.Close()
			}
			t.Fatal("conflicting host admitted", err)
		}
	}
	custom := options
	custom.AccessChecker = access.Owner{}
	if other, err := host.Open(ctx, custom); err == nil || !strings.Contains(err.Error(), "ConfigurationVersion") {
		if other != nil {
			other.Close()
		}
		t.Fatal("custom policy did not require a declared configuration version", err)
	}
	var initial int64
	must(t, inspect.QueryRow(ctx, `SELECT min(expires_at) FROM dune_instances`).Scan(&initial))
	// Observe an actual renewal instead of assuming that background work started.
	for {
		var renewed int64
		must(t, inspect.QueryRow(ctx, `SELECT min(expires_at) FROM dune_instances`).Scan(&renewed))
		if renewed > initial {
			break
		}
		select {
		case <-ctx.Done():
			t.Fatal("hosts did not renew admission")
		case <-time.After(25 * time.Millisecond):
		}
	}
	must(t, first.Close())
	must(t, second.Close())
	changed := options
	changed.DisableRegistration = true
	if other, err := host.Open(ctx, changed); !errors.Is(err, metadata.ErrConfigurationConflict) {
		if other != nil {
			other.Close()
		}
		t.Fatal("close released outstanding input reservation early", err)
	}
	for {
		var live int
		must(t, inspect.QueryRow(ctx, `SELECT count(*) FROM dune_instances WHERE expires_at>CAST(EXTRACT(EPOCH FROM clock_timestamp())*1000 AS BIGINT)`).Scan(&live))
		if live == 0 {
			break
		}
		select {
		case <-ctx.Done():
			t.Fatal("stopped hosts kept renewing admission")
		case <-time.After(50 * time.Millisecond):
		}
	}
	replacement, err := host.Open(ctx, changed)
	must(t, err)
	defer replacement.Close()
	response := httptest.NewRecorder()
	replacement.ServeHTTP(response, httptest.NewRequest("GET", changed.PublicURL+"api/bootstrap", nil))
	if response.Code != 200 {
		t.Fatal("new configuration could not serve after old grants expired", response.Code)
	}
}

func TestPostgresHostStopsWhenAdmissionRenewalFails(t *testing.T) {
	database := postgresWorkbenchConfig(t)
	ctx, cancel := context.WithTimeout(context.Background(), 12*time.Second)
	defer cancel()
	parsed, err := pgx.ParseConfig(database.Postgres.URL)
	must(t, err)
	must(t, database.Postgres.BeforeConnect(ctx, parsed))
	inspect, err := pgx.ConnectConfig(ctx, parsed)
	must(t, err)
	defer inspect.Close(context.Background())
	options := host.Options{PublicURL: "http://instance.test/dune/", Database: &database}
	app, err := host.Open(ctx, options)
	must(t, err)
	defer app.Close()
	// A database-side invalidation is failure injection, not an admin API.
	_, err = inspect.Exec(ctx, `UPDATE dune_instances SET expires_at=0`)
	must(t, err)
	select {
	case <-app.Done():
	case <-ctx.Done():
		t.Fatal("failed admission renewal did not close host")
	}
	response := httptest.NewRecorder()
	app.ServeHTTP(response, httptest.NewRequest("GET", options.PublicURL+"api/bootstrap", nil))
	if response.Code != 503 {
		t.Fatal("closed host admitted another request", response.Code)
	}
}
