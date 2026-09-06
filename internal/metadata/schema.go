package metadata

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
)

const schemaVersion = 5

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

var schema3 = []string{
	`ALTER TABLE dune_sessions ADD COLUMN identity_namespace TEXT NOT NULL DEFAULT ''`,
	`CREATE TABLE dune_external_identities (namespace TEXT NOT NULL, subject TEXT NOT NULL, principal_id TEXT NOT NULL REFERENCES dune_principals(id), PRIMARY KEY(namespace,subject))`,
	`CREATE INDEX dune_external_principal ON dune_external_identities(principal_id)`,
	`CREATE TABLE dune_login_transactions (state_hash TEXT PRIMARY KEY, browser_hash TEXT NOT NULL, namespace TEXT NOT NULL, redirect_url TEXT NOT NULL, nonce TEXT NOT NULL, verifier TEXT NOT NULL, expires_at BIGINT NOT NULL)`,
	`CREATE INDEX dune_login_expires ON dune_login_transactions(expires_at)`,
}

var schema4 = []string{
	`CREATE TABLE dune_identity_links (request_id TEXT PRIMARY KEY, actor TEXT NOT NULL, principal_id TEXT NOT NULL REFERENCES dune_principals(id), namespace TEXT NOT NULL, subject TEXT NOT NULL, reason TEXT NOT NULL, created_at BIGINT NOT NULL)`,
	`CREATE INDEX dune_identity_links_principal ON dune_identity_links(principal_id,created_at)`,
}

var schema5 = []string{
	`CREATE TABLE dune_access_tickets (hash TEXT PRIMARY KEY, session_hash TEXT NOT NULL REFERENCES dune_sessions(hash) ON DELETE CASCADE, principal_id TEXT NOT NULL REFERENCES dune_principals(id), identity_namespace TEXT NOT NULL, machine_id TEXT NOT NULL REFERENCES dune_machines(id) ON DELETE CASCADE, runner_id TEXT NOT NULL REFERENCES dune_runners(id) ON DELETE CASCADE, fabric_id TEXT NOT NULL, binding_revision BIGINT NOT NULL CHECK(binding_revision>0), auth_version BIGINT NOT NULL CHECK(auth_version>0), expires_at BIGINT NOT NULL)`,
	`CREATE INDEX dune_access_expiry ON dune_access_tickets(expires_at)`,
	`CREATE INDEX dune_access_session ON dune_access_tickets(session_hash)`,
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
			version = 2
		}
		if version == 2 {
			for _, statement := range schema3 {
				if _, err := tx.ExecContext(ctx, statement); err != nil {
					return err
				}
			}
			if _, err := tx.ExecContext(ctx, `UPDATE dune_schema SET version=3 WHERE id=1`); err != nil {
				return err
			}
			version = 3
		}
		if version == 3 {
			for _, statement := range schema4 {
				if _, err := tx.ExecContext(ctx, statement); err != nil {
					return err
				}
			}
			if _, err := tx.ExecContext(ctx, `UPDATE dune_schema SET version=4 WHERE id=1`); err != nil {
				return err
			}
			version = 4
		}
		if version == 4 {
			for _, statement := range schema5 {
				if _, err := tx.ExecContext(ctx, statement); err != nil {
					return err
				}
			}
			if _, err := tx.ExecContext(ctx, `UPDATE dune_schema SET version=5 WHERE id=1`); err != nil {
				return err
			}
		}
		return nil
	})
}
