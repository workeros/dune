package metadata

import (
	"context"
	"database/sql"
	"path/filepath"
	"testing"

	"github.com/aiomni/dune/internal/wire"
	"github.com/aiomni/dune/pkg/storage"
)

func TestSchemaInitializationIsAtomic(t *testing.T) {
	for _, backend := range []string{"sqlite", "postgres"} {
		t.Run(backend, func(t *testing.T) {
			ctx := context.Background()
			config := storage.Config{SQLiteDir: filepath.Join(t.TempDir(), "metadata")}
			if backend == "postgres" {
				config, _, _ = postgresConfig(t)
			}
			s, err := Open(ctx, config)
			if err != nil {
				t.Fatal(err)
			}
			defer s.Close()
			// Start with a conflicting object late in the current DDL. A failed first
			// initialization must not leave earlier tables behind.
			if err := s.transaction(ctx, func(tx *sql.Tx) error {
				for i := len(snapshotTables) - 1; i >= 0; i-- {
					if _, err := tx.ExecContext(ctx, "DROP TABLE "+snapshotTables[i].name); err != nil {
						return err
					}
				}
				_, err := tx.ExecContext(ctx, `CREATE VIEW dune_instances AS SELECT 1 AS sentinel`)
				return err
			}); err != nil {
				t.Fatal(err)
			}
			if err := s.initializeSchema(ctx); err == nil {
				t.Fatal("partial initialization accepted")
			}
			for _, table := range []string{"dune_principals", "dune_operations"} {
				rows, err := s.db.QueryContext(ctx, "SELECT * FROM "+table)
				if err == nil {
					rows.Close()
					t.Fatalf("failed initialization left %s", table)
				}
			}
			var sentinel int
			if err := s.db.QueryRowContext(ctx, `SELECT sentinel FROM dune_instances`).Scan(&sentinel); err != nil || sentinel != 1 {
				t.Fatal("initialization changed conflicting object", err)
			}
			if _, err := s.db.ExecContext(ctx, `DROP VIEW dune_instances`); err != nil {
				t.Fatal(err)
			}
			if err := s.initializeSchema(ctx); err != nil {
				t.Fatal(err)
			}
			intent := operationFixture(t, s)
			if err := s.initializeSchema(ctx); err != nil {
				t.Fatal("current format cannot reopen", err)
			}
			var runner string
			if err := s.db.QueryRowContext(ctx, `SELECT id FROM dune_runners WHERE id=$1`, intent.RunnerID).Scan(&runner); err != nil {
				t.Fatal("reopen changed records", err)
			}
		})
	}
}

func TestSQLiteRejectsSharedClusterServices(t *testing.T) {
	s, err := Open(context.Background(), storage.Config{SQLiteDir: filepath.Join(t.TempDir(), "metadata")})
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	if _, err := s.ConnectionDirectory(context.Background(), wire.ID()); err == nil {
		t.Fatal("SQLite accepted cluster ownership")
	}
	if _, err := s.RegisterInstance(context.Background(), InstanceConfig{BootID: wire.ID(), Fingerprint: "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"}); err == nil {
		t.Fatal("SQLite accepted shared admission")
	}
}
