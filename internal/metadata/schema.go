package metadata

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
)

const schemaVersion = 10

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

var schema6 = []string{
	`ALTER TABLE dune_sessions ADD COLUMN kind TEXT NOT NULL DEFAULT 'browser' CHECK(kind IN ('browser','cli'))`,
	`ALTER TABLE dune_sessions ADD COLUMN parent_hash TEXT REFERENCES dune_sessions(hash) ON DELETE CASCADE`,
	`CREATE INDEX dune_sessions_parent ON dune_sessions(parent_hash)`,
	`CREATE TABLE dune_cli_logins (id TEXT PRIMARY KEY, challenge TEXT NOT NULL, site TEXT NOT NULL, identity_namespace TEXT NOT NULL, expires_at BIGINT NOT NULL, session_hash TEXT REFERENCES dune_sessions(hash) ON DELETE CASCADE)`,
	`CREATE INDEX dune_cli_expiry ON dune_cli_logins(expires_at)`,
	`CREATE INDEX dune_cli_parent ON dune_cli_logins(session_hash)`,
}

var schema7 = []string{
	`DROP INDEX dune_runners_owner`,
	`CREATE INDEX dune_runners_owner ON dune_runners(owner_id,id)`,
	`ALTER TABLE dune_access_tickets ADD COLUMN owner_id TEXT NOT NULL DEFAULT ''`,
	`UPDATE dune_access_tickets SET owner_id=(SELECT owner_id FROM dune_runners WHERE id=dune_access_tickets.runner_id)`,
	`CREATE TABLE dune_discovery_cursors (id TEXT PRIMARY KEY,principal_id TEXT NOT NULL REFERENCES dune_principals(id) ON DELETE CASCADE,identity_namespace TEXT NOT NULL,operation TEXT NOT NULL,after_id TEXT NOT NULL,expires_at BIGINT NOT NULL,UNIQUE(principal_id,identity_namespace,operation,after_id))`,
	`CREATE INDEX dune_discovery_expiry ON dune_discovery_cursors(expires_at)`,
}

// Prior external sessions did not record which linked subject logged in. Never
// infer one from the current mapping. Existing enrollment issuers are similarly
// unknown. Machine identities, local sessions and their tickets remain valid.
var schema8 = []string{
	`ALTER TABLE dune_sessions ADD COLUMN identity_subject TEXT NOT NULL DEFAULT ''`,
	`ALTER TABLE dune_access_tickets ADD COLUMN identity_subject TEXT NOT NULL DEFAULT ''`,
	`ALTER TABLE dune_enrollments ADD COLUMN identity_namespace TEXT NOT NULL DEFAULT ''`,
	`ALTER TABLE dune_enrollments ADD COLUMN identity_subject TEXT NOT NULL DEFAULT ''`,
	`DELETE FROM dune_sessions WHERE identity_namespace<>''`,
	`DELETE FROM dune_enrollments`,
}

var schema9 = []string{
	`CREATE TABLE dune_operations (id TEXT PRIMARY KEY,request_key TEXT NOT NULL,request_digest TEXT NOT NULL,principal_id TEXT NOT NULL REFERENCES dune_principals(id),identity_namespace TEXT NOT NULL,identity_subject TEXT NOT NULL,runner_id TEXT NOT NULL REFERENCES dune_runners(id),fabric_id TEXT NOT NULL,binding_revision BIGINT NOT NULL CHECK(binding_revision>0),action TEXT NOT NULL CHECK(action IN ('create','renew','destroy')),created_at BIGINT NOT NULL,finished BOOLEAN NOT NULL DEFAULT FALSE,outcome TEXT NOT NULL DEFAULT '' CHECK(outcome IN ('','unknown','timed_out','succeeded','failed')),worker TEXT NOT NULL DEFAULT '',execution_revision BIGINT NOT NULL DEFAULT 0 CHECK(execution_revision>=0),lease_until BIGINT NOT NULL DEFAULT 0,exclusive BOOLEAN NOT NULL DEFAULT TRUE,UNIQUE(principal_id,request_key),CHECK(finished=FALSE OR exclusive=FALSE),CHECK((finished=FALSE AND outcome IN ('','unknown','timed_out')) OR (finished=TRUE AND outcome IN ('succeeded','failed'))))`,
	`CREATE UNIQUE INDEX dune_operations_active_runner ON dune_operations(runner_id) WHERE exclusive=TRUE`,
}

var schema10 = []string{
	`CREATE TABLE dune_cluster (id INTEGER PRIMARY KEY CHECK(id=1),recovery_generation TEXT NOT NULL)`,
	`CREATE TABLE dune_routes (machine_id TEXT PRIMARY KEY REFERENCES dune_machines(id) ON DELETE CASCADE,recovery_generation TEXT NOT NULL,epoch BIGINT NOT NULL CHECK(epoch>0),owner_boot_id TEXT NOT NULL,owner_address TEXT NOT NULL,binding TEXT NOT NULL,published BOOLEAN NOT NULL DEFAULT FALSE,expires_at BIGINT NOT NULL)`,
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
			version = 5
		}
		if version == 5 {
			for _, statement := range schema6 {
				if _, err := tx.ExecContext(ctx, statement); err != nil {
					return err
				}
			}
			if _, err := tx.ExecContext(ctx, `UPDATE dune_schema SET version=6 WHERE id=1`); err != nil {
				return err
			}
			version = 6
		}
		if version == 6 {
			for _, statement := range schema7 {
				if _, err := tx.ExecContext(ctx, statement); err != nil {
					return err
				}
			}
			if _, err := tx.ExecContext(ctx, `UPDATE dune_schema SET version=7 WHERE id=1`); err != nil {
				return err
			}
			version = 7
		}
		if version == 7 {
			for _, statement := range schema8 {
				if _, err := tx.ExecContext(ctx, statement); err != nil {
					return err
				}
			}
			if _, err := tx.ExecContext(ctx, `UPDATE dune_schema SET version=8 WHERE id=1`); err != nil {
				return err
			}
			version = 8
		}
		if version == 8 {
			for _, statement := range schema9 {
				if _, err := tx.ExecContext(ctx, statement); err != nil {
					return err
				}
			}
			if _, err := tx.ExecContext(ctx, `UPDATE dune_schema SET version=9 WHERE id=1`); err != nil {
				return err
			}
			version = 9
		}
		if version == 9 {
			for _, statement := range schema10 {
				if _, err := tx.ExecContext(ctx, statement); err != nil {
					return err
				}
			}
			if _, err := tx.ExecContext(ctx, `UPDATE dune_schema SET version=10 WHERE id=1`); err != nil {
				return err
			}
		}
		return nil
	})
}
