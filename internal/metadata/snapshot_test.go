package metadata

import (
	"context"
	"path/filepath"
	"strings"
	"testing"

	"github.com/aiomni/dune/pkg/storage"
)

var localTables = []struct{ name, columns string }{
	{"dune_users", "id,email,salt,password_hash,enabled,auth_version"},
	{"dune_sessions", "hash,user_id,expires_at,auth_version"},
	{"dune_runners", "id,owner_id,name,kind,fabric_id,binding_revision,machine_id,credential_hash,os,arch,enabled,suspended,created_at"},
	{"dune_enrollments", "hash,owner_id,namespace,subject,name,runner_id,kind,fabric_id,expires_at"},
}

func TestLocalSQLiteSchemaIsExactlyFourTables(t *testing.T) {
	ctx := context.Background()
	s, err := Open(ctx, storage.Config{SQLiteDir: filepath.Join(t.TempDir(), "metadata")})
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	var total int
	if err := s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM sqlite_schema WHERE type='table' AND name NOT LIKE 'sqlite_%'`).Scan(&total); err != nil || total != len(localTables) {
		t.Fatal("local SQLite schema must contain exactly four logical tables", total, err)
	}
	for _, table := range localTables {
		rows, err := s.db.QueryContext(ctx, "SELECT * FROM "+table.name+" LIMIT 0")
		if err != nil {
			t.Fatal(err)
		}
		columns, columnErr := rows.Columns()
		rows.Close()
		if columnErr != nil || strings.Join(columns, ",") != table.columns {
			t.Fatalf("columns for %s: %v (%v)", table.name, columns, columnErr)
		}
	}
}

func TestPostgresLogicalTableCounts(t *testing.T) {
	for _, test := range []struct {
		name     string
		external bool
		want     int
	}{{"local", false, 5}, {"enterprise", true, 3}} {
		t.Run(test.name, func(t *testing.T) {
			config, _, _ := postgresConfig(t)
			s, err := Open(context.Background(), config, OpenOptions{ExternalIdentity: test.external})
			if err != nil {
				t.Fatal(err)
			}
			defer s.Close()
			var count int
			if err := s.db.QueryRow(`SELECT COUNT(*) FROM information_schema.tables WHERE table_schema=current_schema()`).Scan(&count); err != nil || count != test.want {
				t.Fatal("unexpected Dune table count", count, err)
			}
		})
	}
}
