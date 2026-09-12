package metadata

import (
	"context"
	"database/sql"
	"fmt"
	"slices"
)

// Dune initializes only fresh schemas. There is intentionally no migration,
// backup-restore generation or historical rollback compatibility path.
var localIdentitySchema = []string{
	`CREATE TABLE dune_users (id TEXT PRIMARY KEY,email TEXT NOT NULL UNIQUE,salt TEXT NOT NULL,password_hash TEXT NOT NULL,enabled BOOLEAN NOT NULL DEFAULT TRUE,auth_version BIGINT NOT NULL DEFAULT 1 CHECK(auth_version>0))`,
	`CREATE TABLE dune_sessions (hash TEXT PRIMARY KEY,user_id TEXT NOT NULL REFERENCES dune_users(id) ON DELETE CASCADE,expires_at BIGINT NOT NULL,auth_version BIGINT NOT NULL CHECK(auth_version>0))`,
	`CREATE INDEX dune_sessions_user ON dune_sessions(user_id,expires_at)`,
}

var executionSchema = []string{
	`CREATE TABLE dune_runners (id TEXT PRIMARY KEY,owner_id TEXT NOT NULL,created_by_id TEXT NOT NULL,created_by_namespace TEXT NOT NULL,created_by_subject TEXT NOT NULL,name TEXT NOT NULL,kind TEXT NOT NULL CHECK(kind IN ('attached','managed')),fabric_id TEXT NOT NULL,binding_revision BIGINT NOT NULL CHECK(binding_revision>0),machine_id TEXT UNIQUE,credential_hash TEXT UNIQUE,os TEXT NOT NULL DEFAULT '',arch TEXT NOT NULL DEFAULT '',enabled BOOLEAN NOT NULL DEFAULT TRUE,suspended BOOLEAN NOT NULL DEFAULT FALSE,created_at BIGINT NOT NULL)`,
	`CREATE INDEX dune_runners_owner ON dune_runners(owner_id,id)`,
	`CREATE TABLE dune_enrollments (hash TEXT PRIMARY KEY,owner_id TEXT NOT NULL,issued_to_id TEXT NOT NULL,issued_to_kind TEXT NOT NULL,namespace TEXT NOT NULL,subject TEXT NOT NULL,name TEXT NOT NULL,runner_id TEXT UNIQUE,kind TEXT NOT NULL CHECK(kind IN ('attached','managed')),fabric_id TEXT NOT NULL,expires_at BIGINT NOT NULL)`,
	`CREATE INDEX dune_enrollments_owner ON dune_enrollments(owner_id,expires_at)`,
}

var postgresSchema = []string{
	`CREATE TABLE dune_routes (machine_id TEXT PRIMARY KEY,epoch BIGINT NOT NULL CHECK(epoch>0),owner_boot_id TEXT NOT NULL,owner_address TEXT NOT NULL,binding TEXT NOT NULL,published BOOLEAN NOT NULL DEFAULT FALSE,expires_at BIGINT NOT NULL)`,
}

type schemaQueryer interface {
	QueryContext(context.Context, string, ...any) (*sql.Rows, error)
	QueryRowContext(context.Context, string, ...any) *sql.Row
}

var schemaColumns = map[string][]string{
	"dune_users":       {"id", "email", "salt", "password_hash", "enabled", "auth_version"},
	"dune_sessions":    {"hash", "user_id", "expires_at", "auth_version"},
	"dune_runners":     {"id", "owner_id", "created_by_id", "created_by_namespace", "created_by_subject", "name", "kind", "fabric_id", "binding_revision", "machine_id", "credential_hash", "os", "arch", "enabled", "suspended", "created_at"},
	"dune_enrollments": {"hash", "owner_id", "issued_to_id", "issued_to_kind", "namespace", "subject", "name", "runner_id", "kind", "fabric_id", "expires_at"},
	"dune_routes":      {"machine_id", "epoch", "owner_boot_id", "owner_address", "binding", "published", "expires_at"},
}

func (s *Store) schemaExists(ctx context.Context, queryer schemaQueryer) (bool, error) {
	query := `SELECT EXISTS(SELECT 1 FROM sqlite_schema WHERE type='table' AND name='dune_runners')`
	if s.postgres {
		query = `SELECT EXISTS(SELECT 1 FROM information_schema.tables WHERE table_schema=current_schema() AND table_name='dune_runners')`
	}
	var exists bool
	if err := queryer.QueryRowContext(ctx, query).Scan(&exists); err != nil {
		return false, err
	}
	return exists, nil
}

func (s *Store) initializeSchema(ctx context.Context) error {
	return s.transaction(ctx, func(tx *sql.Tx) error {
		if s.postgres {
			// Serialize fresh initialization across application replicas.
			if _, err := tx.ExecContext(ctx, `SELECT pg_advisory_xact_lock(1146441285)`); err != nil {
				return err
			}
		}
		exists, err := s.schemaExists(ctx, tx)
		if err != nil {
			return err
		}
		if !exists {
			statements := make([]string, 0, len(localIdentitySchema)+len(executionSchema)+len(postgresSchema))
			if s.localIdentity {
				statements = append(statements, localIdentitySchema...)
			}
			statements = append(statements, executionSchema...)
			if s.postgres {
				statements = append(statements, postgresSchema...)
			}
			for _, statement := range statements {
				if _, err := tx.ExecContext(ctx, statement); err != nil {
					return err
				}
			}
		}
		return s.validateSchema(ctx, tx)
	})
}

func (s *Store) expectedTables() []string {
	tables := []string{"dune_enrollments", "dune_runners"}
	if s.localIdentity {
		tables = append(tables, "dune_sessions", "dune_users")
	}
	if s.postgres {
		tables = append(tables, "dune_routes")
	}
	slices.Sort(tables)
	return tables
}

func (s *Store) validateSchema(ctx context.Context, queryer schemaQueryer) error {
	query := `SELECT name FROM sqlite_schema WHERE type='table' AND name GLOB 'dune_*' ORDER BY name`
	if s.postgres {
		query = `SELECT table_name FROM information_schema.tables WHERE table_schema=current_schema() AND table_name LIKE 'dune\_%' ESCAPE '\' ORDER BY table_name`
	}
	rows, err := queryer.QueryContext(ctx, query)
	if err != nil {
		return err
	}
	var actual []string
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			rows.Close()
			return err
		}
		actual = append(actual, name)
	}
	if err := rows.Close(); err != nil {
		return err
	}
	if err := rows.Err(); err != nil {
		return err
	}
	expected := s.expectedTables()
	if !slices.Equal(actual, expected) {
		return fmt.Errorf("incompatible Dune metadata schema: got tables %v, want %v; automatic migration and recovery are unsupported", actual, expected)
	}
	for _, table := range expected {
		columns, err := s.tableColumns(ctx, queryer, table)
		if err != nil {
			return err
		}
		if !slices.Equal(columns, schemaColumns[table]) {
			return fmt.Errorf("incompatible Dune metadata schema: table %s has columns %v, want %v; automatic migration and recovery are unsupported", table, columns, schemaColumns[table])
		}
	}
	return nil
}

func (s *Store) tableColumns(ctx context.Context, queryer schemaQueryer, table string) ([]string, error) {
	query := `SELECT name FROM pragma_table_info('` + table + `') ORDER BY cid`
	if s.postgres {
		query = `SELECT column_name FROM information_schema.columns WHERE table_schema=current_schema() AND table_name=$1 ORDER BY ordinal_position`
	}
	var (
		rows *sql.Rows
		err  error
	)
	if s.postgres {
		rows, err = queryer.QueryContext(ctx, query, table)
	} else {
		rows, err = queryer.QueryContext(ctx, query)
	}
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var columns []string
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			return nil, err
		}
		columns = append(columns, name)
	}
	return columns, rows.Err()
}
