package metadata

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"errors"
	"fmt"
	"strings"
)

// Schema is the complete current format. Development builds initialize fresh
// databases; there is no historical schema upgrade or data conversion path.
var schema = []string{
	`CREATE TABLE dune_principals (id TEXT PRIMARY KEY, email TEXT NOT NULL, enabled BOOLEAN NOT NULL DEFAULT TRUE, auth_version BIGINT NOT NULL DEFAULT 1 CHECK(auth_version > 0))`,
	`CREATE TABLE dune_local_accounts (principal_id TEXT PRIMARY KEY REFERENCES dune_principals(id), email TEXT NOT NULL UNIQUE, salt TEXT NOT NULL, password_hash TEXT NOT NULL)`,
	`CREATE TABLE dune_sessions (hash TEXT PRIMARY KEY, principal_id TEXT NOT NULL REFERENCES dune_principals(id), expires_at BIGINT NOT NULL, auth_version BIGINT NOT NULL DEFAULT 1 CHECK(auth_version > 0), identity_namespace TEXT NOT NULL DEFAULT '', kind TEXT NOT NULL DEFAULT 'browser' CHECK(kind IN ('browser','cli')), parent_hash TEXT REFERENCES dune_sessions(hash) ON DELETE CASCADE, identity_subject TEXT NOT NULL DEFAULT '')`,
	`CREATE INDEX dune_sessions_principal ON dune_sessions(principal_id, expires_at)`,
	`CREATE INDEX dune_sessions_parent ON dune_sessions(parent_hash)`,
	`CREATE TABLE dune_enrollments (hash TEXT PRIMARY KEY, principal_id TEXT NOT NULL REFERENCES dune_principals(id), name TEXT NOT NULL, expires_at BIGINT NOT NULL, identity_namespace TEXT NOT NULL DEFAULT '', identity_subject TEXT NOT NULL DEFAULT '')`,
	`CREATE INDEX dune_enrollments_principal ON dune_enrollments(principal_id, expires_at)`,
	`CREATE TABLE dune_runners (id TEXT PRIMARY KEY, owner_id TEXT NOT NULL REFERENCES dune_principals(id), name TEXT NOT NULL, kind TEXT NOT NULL CHECK(kind IN ('attached','managed')), fabric_id TEXT NOT NULL, binding_revision BIGINT NOT NULL CHECK(binding_revision > 0), created_at BIGINT NOT NULL)`,
	`CREATE INDEX dune_runners_owner ON dune_runners(owner_id,id)`,
	`CREATE TABLE dune_machines (id TEXT PRIMARY KEY, runner_id TEXT NOT NULL UNIQUE REFERENCES dune_runners(id) ON DELETE CASCADE, credential_hash TEXT NOT NULL UNIQUE, os TEXT NOT NULL, arch TEXT NOT NULL)`,
	`CREATE TABLE dune_external_identities (namespace TEXT NOT NULL, subject TEXT NOT NULL, principal_id TEXT NOT NULL REFERENCES dune_principals(id), PRIMARY KEY(namespace,subject))`,
	`CREATE INDEX dune_external_principal ON dune_external_identities(principal_id)`,
	`CREATE TABLE dune_login_transactions (state_hash TEXT PRIMARY KEY, browser_hash TEXT NOT NULL, namespace TEXT NOT NULL, redirect_url TEXT NOT NULL, nonce TEXT NOT NULL, verifier TEXT NOT NULL, expires_at BIGINT NOT NULL)`,
	`CREATE INDEX dune_login_expires ON dune_login_transactions(expires_at)`,
	`CREATE TABLE dune_identity_links (request_id TEXT PRIMARY KEY, actor TEXT NOT NULL, principal_id TEXT NOT NULL REFERENCES dune_principals(id), namespace TEXT NOT NULL, subject TEXT NOT NULL, reason TEXT NOT NULL, created_at BIGINT NOT NULL)`,
	`CREATE INDEX dune_identity_links_principal ON dune_identity_links(principal_id,created_at)`,
	`CREATE TABLE dune_access_tickets (hash TEXT PRIMARY KEY, session_hash TEXT NOT NULL REFERENCES dune_sessions(hash) ON DELETE CASCADE, principal_id TEXT NOT NULL REFERENCES dune_principals(id), identity_namespace TEXT NOT NULL, machine_id TEXT NOT NULL REFERENCES dune_machines(id) ON DELETE CASCADE, runner_id TEXT NOT NULL REFERENCES dune_runners(id) ON DELETE CASCADE, fabric_id TEXT NOT NULL, binding_revision BIGINT NOT NULL CHECK(binding_revision>0), auth_version BIGINT NOT NULL CHECK(auth_version>0), expires_at BIGINT NOT NULL, owner_id TEXT NOT NULL, identity_subject TEXT NOT NULL DEFAULT '')`,
	`CREATE INDEX dune_access_expiry ON dune_access_tickets(expires_at)`,
	`CREATE INDEX dune_access_session ON dune_access_tickets(session_hash)`,
	`CREATE TABLE dune_cli_logins (id TEXT PRIMARY KEY, challenge TEXT NOT NULL, site TEXT NOT NULL, identity_namespace TEXT NOT NULL, expires_at BIGINT NOT NULL, session_hash TEXT REFERENCES dune_sessions(hash) ON DELETE CASCADE)`,
	`CREATE INDEX dune_cli_expiry ON dune_cli_logins(expires_at)`,
	`CREATE INDEX dune_cli_parent ON dune_cli_logins(session_hash)`,
	`CREATE TABLE dune_discovery_cursors (id TEXT PRIMARY KEY,principal_id TEXT NOT NULL REFERENCES dune_principals(id) ON DELETE CASCADE,identity_namespace TEXT NOT NULL,operation TEXT NOT NULL,after_id TEXT NOT NULL,expires_at BIGINT NOT NULL,UNIQUE(principal_id,identity_namespace,operation,after_id))`,
	`CREATE INDEX dune_discovery_expiry ON dune_discovery_cursors(expires_at)`,
	`CREATE TABLE dune_operations (id TEXT PRIMARY KEY,request_key TEXT NOT NULL,request_digest TEXT NOT NULL,principal_id TEXT NOT NULL REFERENCES dune_principals(id),identity_namespace TEXT NOT NULL,identity_subject TEXT NOT NULL,runner_id TEXT NOT NULL REFERENCES dune_runners(id),fabric_id TEXT NOT NULL,binding_revision BIGINT NOT NULL CHECK(binding_revision>0),action TEXT NOT NULL CHECK(action IN ('create','renew','destroy')),created_at BIGINT NOT NULL,finished BOOLEAN NOT NULL DEFAULT FALSE,outcome TEXT NOT NULL DEFAULT '' CHECK(outcome IN ('','unknown','timed_out','succeeded','failed')),worker TEXT NOT NULL DEFAULT '',execution_revision BIGINT NOT NULL DEFAULT 0 CHECK(execution_revision>=0),lease_until BIGINT NOT NULL DEFAULT 0,exclusive BOOLEAN NOT NULL DEFAULT TRUE,UNIQUE(principal_id,request_key),CHECK(finished=FALSE OR exclusive=FALSE),CHECK((finished=FALSE AND outcome IN ('','unknown','timed_out')) OR (finished=TRUE AND outcome IN ('succeeded','failed'))))`,
	`CREATE UNIQUE INDEX dune_operations_active_runner ON dune_operations(runner_id) WHERE exclusive=TRUE`,
	`CREATE TABLE dune_managed_creations (runner_id TEXT PRIMARY KEY REFERENCES dune_runners(id),operation_id TEXT NOT NULL UNIQUE REFERENCES dune_operations(id),specification TEXT NOT NULL)`,
	`CREATE TABLE dune_managed_resources (runner_id TEXT PRIMARY KEY REFERENCES dune_runners(id),fabric_id TEXT NOT NULL,resource_ref TEXT NOT NULL,confirmed_at BIGINT NOT NULL,expires_at BIGINT NOT NULL DEFAULT 0,gone BOOLEAN NOT NULL DEFAULT FALSE,access_closed BOOLEAN NOT NULL DEFAULT FALSE,UNIQUE(fabric_id,resource_ref))`,
	`CREATE TABLE dune_managed_maintenance (runner_id TEXT PRIMARY KEY REFERENCES dune_managed_resources(runner_id) ON DELETE CASCADE,fabric_id TEXT NOT NULL,resource_ref TEXT NOT NULL,binding_revision BIGINT NOT NULL CHECK(binding_revision>0),policy_version TEXT NOT NULL DEFAULT '',reason TEXT NOT NULL DEFAULT '',facts TEXT NOT NULL DEFAULT '' CHECK(facts IN ('','confirmed','unknown','timed_out')),observed_at BIGINT NOT NULL DEFAULT 0,next_check_at BIGINT NOT NULL DEFAULT 0,renew_until BIGINT NOT NULL DEFAULT 0,worker TEXT NOT NULL DEFAULT '',execution_revision BIGINT NOT NULL DEFAULT 0 CHECK(execution_revision>=0),lease_until BIGINT NOT NULL DEFAULT 0,UNIQUE(fabric_id,resource_ref),CHECK(renew_until=0 OR next_check_at=0))`,
	`CREATE INDEX dune_managed_maintenance_due ON dune_managed_maintenance(next_check_at,lease_until)`,
	`CREATE TABLE dune_provider_actions (id TEXT PRIMARY KEY,operation_id TEXT NOT NULL REFERENCES dune_operations(id),kind TEXT NOT NULL CHECK(kind IN ('create','bootstrap','renew','destroy')),request_digest TEXT NOT NULL,resource_ref TEXT NOT NULL,renew_until BIGINT NOT NULL DEFAULT 0,worker TEXT NOT NULL,execution_revision BIGINT NOT NULL CHECK(execution_revision>0),started_at BIGINT NOT NULL,completed_at BIGINT NOT NULL DEFAULT 0,outcome TEXT NOT NULL DEFAULT '' CHECK(outcome IN ('','unknown','timed_out','succeeded','failed')),UNIQUE(operation_id,kind),CHECK((completed_at=0 AND outcome IN ('','unknown','timed_out')) OR (completed_at>0 AND outcome IN ('succeeded','failed'))))`,
	`CREATE UNIQUE INDEX dune_provider_actions_pending ON dune_provider_actions(operation_id) WHERE completed_at=0`,
	`CREATE TABLE dune_managed_enrollments (hash TEXT PRIMARY KEY,action_id TEXT NOT NULL UNIQUE REFERENCES dune_provider_actions(id),operation_id TEXT NOT NULL UNIQUE REFERENCES dune_operations(id),runner_id TEXT NOT NULL UNIQUE REFERENCES dune_runners(id),fabric_id TEXT NOT NULL,binding_revision BIGINT NOT NULL CHECK(binding_revision>0),resource_ref TEXT NOT NULL,expires_at BIGINT NOT NULL)`,
	`CREATE INDEX dune_managed_enrollments_expiry ON dune_managed_enrollments(expires_at)`,
	`CREATE TABLE dune_cluster (id INTEGER PRIMARY KEY CHECK(id=1),recovery_generation TEXT NOT NULL)`,
	`CREATE TABLE dune_routes (machine_id TEXT PRIMARY KEY REFERENCES dune_machines(id) ON DELETE CASCADE,recovery_generation TEXT NOT NULL,epoch BIGINT NOT NULL CHECK(epoch>0),owner_boot_id TEXT NOT NULL,owner_address TEXT NOT NULL,binding TEXT NOT NULL,published BOOLEAN NOT NULL DEFAULT FALSE,expires_at BIGINT NOT NULL)`,
	`CREATE TABLE dune_peer_access (hash TEXT PRIMARY KEY,session_hash TEXT NOT NULL REFERENCES dune_sessions(hash) ON DELETE CASCADE,machine_id TEXT NOT NULL REFERENCES dune_machines(id) ON DELETE CASCADE,source_boot_id TEXT NOT NULL,owner_boot_id TEXT NOT NULL,identity_namespace TEXT NOT NULL,request_digest TEXT NOT NULL,expires_at BIGINT NOT NULL,context TEXT NOT NULL)`,
	`CREATE INDEX dune_peer_access_session ON dune_peer_access(session_hash)`,
	`CREATE TABLE dune_instances (boot_id TEXT PRIMARY KEY,fingerprint TEXT NOT NULL,recovery_generation TEXT NOT NULL,expires_at BIGINT NOT NULL)`,
}

// A content fingerprint makes schema edits explicit at open time without
// maintaining a second version counter or accepting partially upgraded data.
func schemaFingerprint() string {
	return fmt.Sprintf("%x", sha256.Sum256([]byte(strings.Join(schema, "\n"))))
}

func checkSchema(fingerprint string) error {
	if fingerprint != schemaFingerprint() {
		return fmt.Errorf("metadata schema differs from this build; use a fresh development database")
	}
	return nil
}

func (s *Store) initializeSchema(ctx context.Context) error {
	return s.transaction(ctx, func(tx *sql.Tx) error {
		if s.postgres {
			// Serialize fresh initialization across processes; released with the transaction.
			if _, err := tx.ExecContext(ctx, `SELECT pg_advisory_xact_lock(1146441285)`); err != nil {
				return err
			}
		}
		if _, err := tx.ExecContext(ctx, `CREATE TABLE IF NOT EXISTS dune_schema (id INTEGER PRIMARY KEY CHECK(id=1), fingerprint TEXT NOT NULL)`); err != nil {
			return err
		}
		var fingerprint string
		err := tx.QueryRowContext(ctx, `SELECT fingerprint FROM dune_schema WHERE id=1`).Scan(&fingerprint)
		if err == nil {
			return checkSchema(fingerprint)
		}
		if !errors.Is(err, sql.ErrNoRows) {
			return err
		}
		for _, statement := range schema {
			if _, err := tx.ExecContext(ctx, statement); err != nil {
				return err
			}
		}
		_, err = tx.ExecContext(ctx, `INSERT INTO dune_schema(id,fingerprint) VALUES(1,$1)`, schemaFingerprint())
		return err
	})
}
