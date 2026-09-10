package tests

import (
	"context"
	"os"
	"testing"
	"time"

	"github.com/aiomni/dune/internal/metadata"
	"github.com/aiomni/dune/internal/testcert"
	"github.com/aiomni/dune/internal/wire"
	"github.com/aiomni/dune/pkg/host"
	"github.com/aiomni/dune/pkg/storage"
	"github.com/aiomni/dune/pkg/transport/peer"
	"github.com/jackc/pgx/v5"
	"net/http/httptest"
	"net/url"
)

func TestPostgresWorkbenchEnrollmentAndTerminal(t *testing.T) {
	database := postgresWorkbenchConfig(t)
	testPrefixedWorkbench(t, workbenchCase{database: &database})
}

func postgresWorkbenchConfig(t *testing.T) storage.Config {
	t.Helper()
	address := os.Getenv("DUNE_TEST_POSTGRES")
	if address == "" {
		t.Skip("DUNE_TEST_POSTGRES not configured")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	admin, err := pgx.Connect(ctx, address)
	must(t, err)
	name := "dune_workbench_" + wire.ID()
	quoted := pgx.Identifier{name}.Sanitize()
	_, err = admin.Exec(ctx, "CREATE SCHEMA "+quoted)
	must(t, err)
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		_, err := admin.Exec(ctx, "DROP SCHEMA "+quoted+" CASCADE")
		if err != nil {
			t.Error(err)
		}
		admin.Close(ctx)
	})
	database := storage.Config{Postgres: &storage.Postgres{URL: address, BeforeConnect: func(ctx context.Context, c *pgx.ConnConfig) error {
		c.RuntimeParams["search_path"] = name
		return nil
	}}}
	return database
}

func TestPostgresClusterWorkbench(t *testing.T) {
	database := postgresWorkbenchConfig(t)
	testPrefixedWorkbench(t, workbenchCase{database: &database, cluster: true, runnerEntry: true, enterprise: true})
}

func TestPostgresHostClusterConfiguration(t *testing.T) {
	database := postgresWorkbenchConfig(t)
	ctx := context.Background()
	store, err := metadata.Open(ctx, database)
	must(t, err)
	defer store.Close()
	_, err = store.ConnectionDirectory(ctx)
	must(t, err)
	ca := testcert.New(t)
	cluster := &host.ClusterOptions{Peer: peer.Config{Address: "https://127.0.0.1:9443/private/peer", Certificate: ca.Issue(t, "127.0.0.1", nil), Roots: ca.Roots()}}
	options := host.Options{PublicURL: "http://example.test/", Database: &database}
	standalone, err := host.Open(ctx, options)
	must(t, err)
	if err := standalone.Close(); err != nil {
		t.Fatal(err)
	}
	options.Cluster = cluster
	app, err := host.Open(ctx, options)
	must(t, err)
	defer app.Close()
	response := httptest.NewRecorder()
	app.ServeHTTP(response, httptest.NewRequest("GET", "http://example.test/private/peer", nil))
	if response.Code != 404 {
		t.Fatal("public listener exposed peer", response.Code)
	}
}

// Preserve the test schema in child-process connection configuration without
// putting its password into arguments or command output.
func postgresWorkbenchURL(t *testing.T, database storage.Config) string {
	t.Helper()
	parsed, err := pgx.ParseConfig(database.Postgres.URL)
	must(t, err)
	must(t, database.Postgres.BeforeConnect(context.Background(), parsed))
	address, err := url.Parse(database.Postgres.URL)
	must(t, err)
	query := address.Query()
	query.Set("search_path", parsed.RuntimeParams["search_path"])
	address.RawQuery = query.Encode()
	return address.String()
}
