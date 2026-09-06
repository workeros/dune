package tests

import (
	"context"
	"os"
	"testing"
	"time"

	"github.com/aiomni/dune/internal/wire"
	"github.com/aiomni/dune/pkg/storage"
	"github.com/jackc/pgx/v5"
)

func TestPostgresWorkbenchEnrollmentAndTerminal(t *testing.T) {
	address := os.Getenv("DUNE_TEST_POSTGRES")
	if address == "" {
		t.Skip("DUNE_TEST_POSTGRES not configured")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	admin, err := pgx.Connect(ctx, address)
	must(t, err)
	defer admin.Close(context.Background())
	name := "dune_workbench_" + wire.ID()
	quoted := pgx.Identifier{name}.Sanitize()
	_, err = admin.Exec(ctx, "CREATE SCHEMA "+quoted)
	must(t, err)
	defer func() {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		_, err := admin.Exec(ctx, "DROP SCHEMA "+quoted+" CASCADE")
		if err != nil {
			t.Error(err)
		}
	}()
	database := storage.Config{Postgres: &storage.Postgres{URL: address, BeforeConnect: func(ctx context.Context, c *pgx.ConnConfig) error {
		c.RuntimeParams["search_path"] = name
		return nil
	}}}
	testPrefixedWorkbench(t, false, false, &database)
}
