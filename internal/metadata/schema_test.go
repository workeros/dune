package metadata

import (
	"context"
	"database/sql"
	"path/filepath"
	"strings"
	"testing"

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
			if err := s.transaction(ctx, func(tx *sql.Tx) error {
				if backend == "postgres" {
					if _, err := tx.ExecContext(ctx, "DROP TABLE dune_routes"); err != nil {
						return err
					}
				}
				for i := len(localTables) - 1; i >= 0; i-- {
					if _, err := tx.ExecContext(ctx, "DROP TABLE "+localTables[i].name); err != nil {
						return err
					}
				}
				_, err := tx.ExecContext(ctx, `CREATE VIEW dune_enrollments AS SELECT 1 AS sentinel`)
				return err
			}); err != nil {
				t.Fatal(err)
			}
			if err := s.initializeSchema(ctx); err == nil {
				t.Fatal("partial initialization accepted")
			}
			for _, table := range []string{"dune_users", "dune_runners"} {
				if rows, err := s.db.QueryContext(ctx, "SELECT * FROM "+table); err == nil {
					rows.Close()
					t.Fatalf("failed initialization left %s", table)
				}
			}
			var sentinel int
			if err := s.db.QueryRowContext(ctx, `SELECT sentinel FROM dune_enrollments`).Scan(&sentinel); err != nil || sentinel != 1 {
				t.Fatal("initialization changed conflicting object", err)
			}
			if _, err := s.db.ExecContext(ctx, `DROP VIEW dune_enrollments`); err != nil {
				t.Fatal(err)
			}
			if err := s.initializeSchema(ctx); err != nil {
				t.Fatal(err)
			}
			token, _, err := s.IssueEnrollment(ctx, "owner", "runner")
			if err != nil {
				t.Fatal(err)
			}
			machine, _, err := s.Enroll(ctx, token, "linux", "amd64")
			if err != nil {
				t.Fatal(err)
			}
			if err := s.initializeSchema(ctx); err != nil {
				t.Fatal("current format cannot reopen", err)
			}
			var runner string
			if err := s.db.QueryRowContext(ctx, `SELECT id FROM dune_runners WHERE id=$1`, machine.RunnerID).Scan(&runner); err != nil {
				t.Fatal("reopen changed records", err)
			}
		})
	}
}

func TestOpenRejectsIncompatibleDuneSchema(t *testing.T) {
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
			if _, err := s.db.ExecContext(ctx, `ALTER TABLE dune_runners ADD COLUMN legacy_generation BIGINT`); err != nil {
				t.Fatal(err)
			}
			if err := s.Close(); err != nil {
				t.Fatal(err)
			}
			reopened, err := Open(ctx, config)
			if reopened != nil {
				reopened.Close()
			}
			if err == nil || !strings.Contains(err.Error(), "incompatible Dune metadata schema") {
				t.Fatal("legacy schema was not rejected", err)
			}
		})
	}
}

func TestExternalIdentityOmitsLocalTables(t *testing.T) {
	s, err := Open(context.Background(), storage.Config{SQLiteDir: filepath.Join(t.TempDir(), "metadata")}, OpenOptions{ExternalIdentity: true})
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	var total int
	if err := s.db.QueryRow(`SELECT COUNT(*) FROM sqlite_schema WHERE type='table' AND name NOT LIKE 'sqlite_%'`).Scan(&total); err != nil || total != 2 {
		t.Fatal("enterprise SQLite schema must contain only runner and enrollment tables", total, err)
	}
}

func TestSQLiteRejectsSharedClusterServices(t *testing.T) {
	s, err := Open(context.Background(), storage.Config{SQLiteDir: filepath.Join(t.TempDir(), "metadata")})
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	if _, err := s.ConnectionDirectory(context.Background()); err == nil {
		t.Fatal("SQLite accepted cluster ownership")
	}
}
