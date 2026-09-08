package metadata

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"github.com/aiomni/dune/internal/wire"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/aiomni/dune/pkg/storage"
)

func TestTransferSchemaCoverage(t *testing.T) {
	ctx := context.Background()
	s, err := Open(ctx, storage.Config{SQLiteDir: filepath.Join(t.TempDir(), "metadata")})
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	var total int
	if err := s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM sqlite_schema WHERE type='table' AND name NOT LIKE 'sqlite_%'`).Scan(&total); err != nil || total != len(transferTables)+1 {
		t.Fatal("transfer manifest omits a schema table", err)
	}
	for _, table := range transferTables {
		rows, err := s.db.QueryContext(ctx, "SELECT * FROM "+table.name+" LIMIT 0")
		if err != nil {
			t.Fatal(err)
		}
		columns, err := rows.Columns()
		rows.Close()
		if err != nil || strings.Join(columns, ",") != table.columns {
			t.Fatalf("transfer columns for %s: %v (%v)", table.name, columns, err)
		}
	}
}

func snapshotRecords(t *testing.T, s *Store) map[string]string {
	t.Helper()
	result := make(map[string]string)
	for _, table := range transferTables {
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
			for i, value := range values {
				if boolean, ok := value.(bool); ok {
					values[i] = int64(0)
					if boolean {
						values[i] = int64(1)
					}
				}
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

func TestSQLiteTransferTransactions(t *testing.T) {
	for _, backend := range []string{"sqlite", "postgres"} {
		t.Run(backend, func(t *testing.T) {
			ctx := context.Background()
			fixture, _, _, _ := legacyFixture(t)
			dir, _ := writeLegacy(t, fixture)
			legacy, err := ReadLegacy(ctx, dir)
			if err != nil {
				t.Fatal(err)
			}
			defer legacy.Close()
			source, err := Open(ctx, storage.Config{SQLiteDir: filepath.Join(t.TempDir(), "source")})
			if err != nil {
				t.Fatal(err)
			}
			defer source.Close()
			if _, err := source.ImportLegacy(ctx, legacy); err != nil {
				t.Fatal(err)
			}
			// These SQL fields have no JSON equivalent: transfer must preserve them.
			if _, err := source.db.ExecContext(ctx, `UPDATE dune_runners SET fabric_id='attached-review',binding_revision=7`); err != nil {
				t.Fatal(err)
			}
			if _, err := source.db.ExecContext(ctx, `UPDATE dune_principals SET enabled=FALSE,auth_version=7`); err != nil {
				t.Fatal(err)
			}
			for _, subject := range []string{"z-subject", "a-subject"} {
				if _, err := source.db.Exec(`INSERT INTO dune_external_identities(namespace,subject,principal_id) SELECT 'issuer',$1,id FROM dune_principals`, subject); err != nil {
					t.Fatal(err)
				}
			}
			if _, err := source.db.Exec(`INSERT INTO dune_identity_links(request_id,actor,principal_id,namespace,subject,reason,created_at) SELECT 'review-request','admin:review',id,'issuer','z-subject','verified migration approval',123456 FROM dune_principals`); err != nil {
				t.Fatal(err)
			}
			if _, err := source.db.Exec(`INSERT INTO dune_login_transactions(state_hash,browser_hash,namespace,redirect_url,nonce,verifier,expires_at) VALUES('state','proof','issuer','https://dune.test/callback','nonce','verifier',9999999999)`); err != nil {
				t.Fatal(err)
			}
			if _, err := source.db.Exec(`INSERT INTO dune_sessions(hash,principal_id,expires_at,auth_version,identity_namespace,kind,parent_hash) SELECT 'cli-session',principal_id,expires_at,auth_version,identity_namespace,'cli',hash FROM dune_sessions LIMIT 1`); err != nil {
				t.Fatal(err)
			}
			if _, err := source.db.Exec(`INSERT INTO dune_cli_logins(id,challenge,site,identity_namespace,expires_at,session_hash) SELECT 'cli-request','proof-hash','https://dune.test/',identity_namespace,9999999999,hash FROM dune_sessions WHERE kind='browser' LIMIT 1`); err != nil {
				t.Fatal(err)
			}
			if _, err := source.db.Exec(`INSERT INTO dune_access_tickets(hash,session_hash,principal_id,identity_namespace,machine_id,runner_id,fabric_id,binding_revision,auth_version,expires_at,owner_id) SELECT 'access-hash',s.hash,s.principal_id,s.identity_namespace,m.id,r.id,r.fabric_id,r.binding_revision,s.auth_version,9999999999,r.owner_id FROM dune_sessions s JOIN dune_runners r ON r.owner_id=s.principal_id JOIN dune_machines m ON m.runner_id=r.id LIMIT 1`); err != nil {
				t.Fatal(err)
			}
			if _, err := source.db.Exec(`INSERT INTO dune_discovery_cursors(id,principal_id,identity_namespace,operation,after_id,expires_at) SELECT 'opaque-cursor',id,'issuer','runner.list','denied-position',9999999999 FROM dune_principals`); err != nil {
				t.Fatal(err)
			}
			for _, table := range []string{"dune_sessions", "dune_access_tickets", "dune_enrollments"} {
				if _, err := source.db.Exec("UPDATE " + table + " SET identity_namespace='issuer',identity_subject='z-subject'"); err != nil {
					t.Fatal(err)
				}
			}
			if _, err := source.db.Exec(`INSERT INTO dune_operations(id,request_key,request_digest,principal_id,identity_namespace,identity_subject,runner_id,fabric_id,binding_revision,action,created_at,outcome,worker,execution_revision,lease_until) SELECT 'original-operation','original-request',$1,owner_id,'issuer','z-subject',id,fabric_id,binding_revision,'renew',123456,'unknown',$2,7,9999999999999 FROM dune_runners`, strings.Repeat("a", 64), strings.Repeat("b", 32)); err != nil {
				t.Fatal(err)
			}
			// Preserve even expired capabilities verbatim during offline transfer;
			// their session/boot/request validation still applies after restoration.
			if _, err := source.db.Exec(`INSERT INTO dune_instances(boot_id,fingerprint,recovery_generation,expires_at) VALUES($1,$2,'',0)`, wire.ID(), strings.Repeat("a", 64)); err != nil {
				t.Fatal(err)
			}
			if _, err := source.db.Exec(`INSERT INTO dune_peer_access(hash,session_hash,machine_id,source_boot_id,owner_boot_id,identity_namespace,request_digest,expires_at,context) SELECT 'peer-hash',a.session_hash,a.machine_id,'source-boot','owner-boot',a.identity_namespace,'request-digest',0,'expired-test-context' FROM dune_access_tickets a LIMIT 1`); err != nil {
				t.Fatal(err)
			}
			before := snapshotRecords(t, source)
			config := storage.Config{SQLiteDir: filepath.Join(t.TempDir(), "target")}
			if backend == "postgres" {
				config, _, _ = postgresConfig(t)
			}
			target, err := Open(ctx, config)
			if err != nil {
				t.Fatal(err)
			}
			defer func() { target.Close() }()
			fail, restore := `CREATE TRIGGER transfer_failure BEFORE INSERT ON dune_machines BEGIN SELECT RAISE(ABORT,'transfer failure'); END`, `DROP TRIGGER transfer_failure`
			if backend == "postgres" {
				fail = `ALTER TABLE dune_machines ADD CONSTRAINT transfer_failure CHECK(os='impossible')`
				restore = `ALTER TABLE dune_machines DROP CONSTRAINT transfer_failure`
			}
			if _, err := target.db.ExecContext(ctx, fail); err != nil {
				t.Fatal(err)
			}
			if _, err := source.CopySQLiteTo(ctx, target); !errors.Is(err, ErrConflict) {
				t.Fatal("late insertion failure not detected", err)
			}
			for _, records := range snapshotRecords(t, target) {
				if records != "null" {
					t.Fatal("partial transfer survived rollback")
				}
			}
			if _, err := target.db.ExecContext(ctx, restore); err != nil {
				t.Fatal(err)
			}
			counts, err := source.CopySQLiteTo(ctx, target)
			if err != nil {
				t.Fatal(err)
			}
			if counts["dune_principals"] != 1 || counts["dune_runners"] != 1 || counts["dune_sessions"] != 3 {
				t.Fatalf("copy counts: %v", counts)
			}
			if !reflect.DeepEqual(before, snapshotRecords(t, target)) {
				t.Fatal("transferred data differs from source")
			}
			if _, err := source.CopySQLiteTo(ctx, target); err == nil {
				t.Fatal("nonempty target accepted")
			}
			if err := target.Close(); err != nil {
				t.Fatal(err)
			}
			target, err = Open(ctx, config)
			if err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(before, snapshotRecords(t, target)) {
				t.Fatal("transferred data lost on restart")
			}
			if !reflect.DeepEqual(before, snapshotRecords(t, source)) {
				t.Fatal("transfer changed source records")
			}
		})
	}
}

func TestSQLiteSourceDoesNotInitialize(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "source")
	if s, err := OpenSQLiteSource(context.Background(), dir); err == nil {
		s.Close()
		t.Fatal("missing source initialized")
	}
	// A foreign SQLite database is not an empty Dune installation to initialize.
	s, err := Open(context.Background(), storage.Config{SQLiteDir: dir})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.db.Exec(`DROP TABLE dune_schema`); err != nil {
		t.Fatal(err)
	}
	s.Close()
	if s, err := OpenSQLiteSource(context.Background(), dir); err == nil {
		s.Close()
		t.Fatal("source schema initialized")
	}
	db, err := sql.Open("sqlite", filepath.Join(dir, "metadata.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	var count int
	if err := db.QueryRow(`SELECT COUNT(*) FROM sqlite_schema WHERE name='dune_schema'`).Scan(&count); err != nil || count != 0 {
		t.Fatal("failed source open changed schema", err)
	}
}
