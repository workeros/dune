package tests

import (
	"context"
	"encoding/json"
	"errors"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"testing"

	"github.com/aiomni/dune/internal/metadata"
	"github.com/aiomni/dune/internal/wire"
	"github.com/aiomni/dune/pkg/gateway"
	"github.com/jackc/pgx/v5"
)

func TestPostgresClusterRecoveryCommand(t *testing.T) {
	ctx := context.Background()
	database := postgresWorkbenchConfig(t)
	store, err := metadata.Open(ctx, database)
	must(t, err)
	defer store.Close()
	original := wire.ID()
	_, err = store.ConnectionDirectory(ctx, original)
	must(t, err)
	parsed, err := pgx.ParseConfig(database.Postgres.URL)
	if err != nil {
		t.Fatal("invalid test database configuration")
	}
	must(t, database.Postgres.BeforeConnect(ctx, parsed))
	address, err := url.Parse(database.Postgres.URL)
	if err != nil {
		t.Fatal("invalid test database URL")
	}
	query := address.Query()
	query.Set("search_path", parsed.RuntimeParams["search_path"])
	address.RawQuery = query.Encode()
	encoded, err := json.Marshal(map[string]any{"postgres": map[string]string{"url": address.String()}})
	must(t, err)
	file := filepath.Join(t.TempDir(), "database.json")
	must(t, os.WriteFile(file, encoded, 0600))
	run := func(args ...string) (recoveryReport, error) {
		command := exec.CommandContext(ctx, binary, append([]string{"metadata", "cluster-recovery", "--database-config", file}, args...)...)
		output, err := command.Output()
		var report recoveryReport
		if err == nil {
			err = json.Unmarshal(output, &report)
		}
		return report, err
	}
	current, err := run()
	must(t, err)
	if current.Generation != original || current.Outcome != "current" {
		t.Fatal("read modified recovery", current)
	}
	rotated, err := run("--rotate-from", original)
	must(t, err)
	if rotated.Generation == original || rotated.Generation == "" || rotated.Outcome != "changed" {
		t.Fatal("rotation did not produce fresh generation", rotated)
	}
	if _, err := run("--rotate-from", original); err == nil {
		t.Fatal("stale operator repeated recovery")
	}
	current, err = run()
	must(t, err)
	if current.Generation != rotated.Generation {
		t.Fatal("stale recovery changed current generation")
	}
	if _, err := store.ConnectionDirectory(ctx, original); !errors.Is(err, gateway.ErrRouteStale) {
		t.Fatal("old service identity survived recovery rotation", err)
	}
}

type recoveryReport struct {
	Generation string `json:"generation"`
	Outcome    string `json:"outcome"`
}
