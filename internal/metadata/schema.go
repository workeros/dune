package metadata

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
)

const schemaVersion = 2

// Version 1 is retained verbatim for coordinated upgrades and upgrade tests.
var schema = []string{
	`CREATE TABLE dune_principals (id TEXT PRIMARY KEY, email TEXT NOT NULL)`,
	`CREATE TABLE dune_local_accounts (principal_id TEXT PRIMARY KEY REFERENCES dune_principals(id), email TEXT NOT NULL UNIQUE, salt TEXT NOT NULL, password_hash TEXT NOT NULL)`,
	`CREATE TABLE dune_sessions (hash TEXT PRIMARY KEY, principal_id TEXT NOT NULL REFERENCES dune_principals(id), expires_at BIGINT NOT NULL)`,
	`CREATE INDEX dune_sessions_principal ON dune_sessions(principal_id, expires_at)`,
	`CREATE TABLE dune_enrollments (hash TEXT PRIMARY KEY, principal_id TEXT NOT NULL REFERENCES dune_principals(id), name TEXT NOT NULL, expires_at BIGINT NOT NULL)`,
	`CREATE INDEX dune_enrollments_principal ON dune_enrollments(principal_id, expires_at)`,
	`CREATE TABLE dune_runners (id TEXT PRIMARY KEY, owner_id TEXT NOT NULL REFERENCES dune_principals(id), name TEXT NOT NULL, kind TEXT NOT NULL CHECK(kind = 'attached'), fabric_id TEXT NOT NULL, binding_revision BIGINT NOT NULL CHECK(binding_revision > 0), created_at BIGINT NOT NULL)`,
	`CREATE INDEX dune_runners_owner ON dune_runners(owner_id)`,
	`CREATE TABLE dune_machines (id TEXT PRIMARY KEY, runner_id TEXT NOT NULL UNIQUE REFERENCES dune_runners(id) ON DELETE CASCADE, credential_hash TEXT NOT NULL UNIQUE, os TEXT NOT NULL, arch TEXT NOT NULL)`,
}

var schema2 = []string{
	`ALTER TABLE dune_principals ADD COLUMN enabled BOOLEAN NOT NULL DEFAULT TRUE`,
	`ALTER TABLE dune_principals ADD COLUMN auth_version BIGINT NOT NULL DEFAULT 1 CHECK(auth_version > 0)`,
	`ALTER TABLE dune_sessions ADD COLUMN auth_version BIGINT NOT NULL DEFAULT 1 CHECK(auth_version > 0)`,
}

func (s *Store) migrate(ctx context.Context) error {
	return s.transaction(ctx, func(tx *sql.Tx) error {
		if s.postgres {
			// Transaction-scoped and released on connection loss. All Dune schema
			// migrations in this database use the same stable namespace key.
			if _, err := tx.ExecContext(ctx, `SELECT pg_advisory_xact_lock(1146441285)`); err != nil {
				return err
			}
		}
		if _, err := tx.ExecContext(ctx, `CREATE TABLE IF NOT EXISTS dune_schema (id INTEGER PRIMARY KEY CHECK(id = 1), version INTEGER NOT NULL)`); err != nil {
			return err
		}
		var version int
		err := tx.QueryRowContext(ctx, `SELECT version FROM dune_schema WHERE id = 1`).Scan(&version)
		if err != nil && !errors.Is(err, sql.ErrNoRows) {
			return err
		}
		if errors.Is(err, sql.ErrNoRows) {
			for _, statement := range schema {
				if _, err := tx.ExecContext(ctx, statement); err != nil {
					return err
				}
			}
			if _, err := tx.ExecContext(ctx, `INSERT INTO dune_schema(id,version) VALUES(1,1)`); err != nil {
				return err
			}
			version = 1
		}
		if version < 1 || version > schemaVersion {
			return fmt.Errorf("unsupported metadata schema version %d", version)
		}
		if version == 1 {
			for _, statement := range schema2 {
				if _, err := tx.ExecContext(ctx, statement); err != nil {
					return err
				}
			}
			if _, err := tx.ExecContext(ctx, `UPDATE dune_schema SET version=2 WHERE id=1`); err != nil {
				return err
			}
		}
		return nil
	})
}
