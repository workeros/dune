package metadata

import (
	"context"
	"encoding/json"
	"path/filepath"
	"strings"
	"testing"

	"github.com/aiomni/dune/pkg/storage"
)

func TestSnapshotSchemaCoverage(t *testing.T) {
	ctx := context.Background()
	s, err := Open(ctx, storage.Config{SQLiteDir: filepath.Join(t.TempDir(), "metadata")})
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	var total int
	if err := s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM sqlite_schema WHERE type='table' AND name NOT LIKE 'sqlite_%'`).Scan(&total); err != nil || total != len(snapshotTables)+1 {
		t.Fatal("snapshot manifest omits a schema table", err)
	}
	for _, table := range snapshotTables {
		rows, err := s.db.QueryContext(ctx, "SELECT * FROM "+table.name+" LIMIT 0")
		if err != nil {
			t.Fatal(err)
		}
		columns, err := rows.Columns()
		rows.Close()
		if err != nil || strings.Join(columns, ",") != table.columns {
			t.Fatalf("snapshot columns for %s: %v (%v)", table.name, columns, err)
		}
	}
}

func snapshotRecords(t *testing.T, s *Store) map[string]string {
	t.Helper()
	result := make(map[string]string)
	for _, table := range snapshotTables {
		rows, err := s.db.Query("SELECT " + table.columns + " FROM " + table.name + " ORDER BY " + table.columns)
		if err != nil {
			t.Fatal(err)
		}
		var records [][]any
		for rows.Next() {
			values := make([]any, len(strings.Split(table.columns, ",")))
			pointers := make([]any, len(values))
			for i := range values {
				pointers[i] = &values[i]
			}
			if err := rows.Scan(pointers...); err != nil {
				t.Fatal(err)
			}
			records = append(records, values)
		}
		err = rows.Err()
		rows.Close()
		if err != nil {
			t.Fatal(err)
		}
		encoded, err := json.Marshal(records)
		if err != nil {
			t.Fatal(err)
		}
		result[table.name] = string(encoded)
	}
	return result
}

// Snapshot every domain field for rollback and native restore assertions.
var snapshotTables = []struct{ name, columns string }{
	{"dune_principals", "id,email,enabled,auth_version"},
	{"dune_external_identities", "namespace,subject,principal_id"},
	{"dune_identity_links", "request_id,actor,principal_id,namespace,subject,reason,created_at"},
	{"dune_discovery_cursors", "id,principal_id,identity_namespace,operation,after_id,expires_at"},
	{"dune_local_accounts", "principal_id,email,salt,password_hash"},
	{"dune_sessions", "hash,principal_id,expires_at,auth_version,identity_namespace,kind,parent_hash,identity_subject"},
	{"dune_cli_logins", "id,challenge,site,identity_namespace,expires_at,session_hash"},
	{"dune_login_transactions", "state_hash,browser_hash,namespace,redirect_url,nonce,verifier,expires_at"},
	{"dune_enrollments", "hash,principal_id,name,expires_at,identity_namespace,identity_subject"},
	{"dune_runners", "id,owner_id,name,kind,fabric_id,binding_revision,created_at"},
	{"dune_operations", operationColumns},
	{"dune_managed_creations", "runner_id,operation_id,specification"},
	{"dune_managed_resources", "runner_id,fabric_id,resource_ref,confirmed_at,expires_at,gone,access_closed"},
	{"dune_managed_maintenance", maintenanceColumns},
	{"dune_provider_actions", actionColumns},
	{"dune_managed_enrollments", "hash,action_id,operation_id,runner_id,fabric_id,binding_revision,resource_ref,expires_at"},
	{"dune_machines", "id,runner_id,credential_hash,os,arch"},
	{"dune_cluster", "id,recovery_generation"},
	{"dune_instances", instanceColumns},
	{"dune_routes", routeColumns},
	{"dune_access_tickets", "hash,session_hash,principal_id,identity_namespace,machine_id,runner_id,fabric_id,binding_revision,auth_version,expires_at,owner_id,identity_subject"},
	{"dune_peer_access", "hash,session_hash,machine_id,source_boot_id,owner_boot_id,identity_namespace,request_digest,expires_at,context"},
}
