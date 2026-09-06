package metadata

import (
	"context"
	"database/sql"
	"path/filepath"
	"testing"

	"github.com/aiomni/dune/internal/identity"
	"github.com/aiomni/dune/pkg/storage"
)

func TestSchemaOneUpgradePreservesIdentity(t *testing.T) {
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
			defer func() { s.Close() }()
			// Recreate the immutable S0c schema in this empty test backend.
			err = s.transaction(ctx, func(tx *sql.Tx) error {
				for i := len(transferTables) - 1; i >= 0; i-- {
					if _, err := tx.Exec("DROP TABLE " + transferTables[i].name); err != nil {
						return err
					}
				}
				for _, statement := range schema {
					if _, err := tx.Exec(statement); err != nil {
						return err
					}
				}
				_, err := tx.Exec(`UPDATE dune_schema SET version=1`)
				return err
			})
			if err != nil {
				t.Fatal(err)
			}
			fixture, cookie, credential, _ := legacyFixture(t)
			for _, a := range fixture.Accounts {
				if err := s.RegisterAccount(ctx, a, tokenHash(cookie), fixture.Sessions[tokenHash(cookie)].ExpiresAt); err != nil {
					t.Fatal(err)
				}
			}
			for _, m := range fixture.Machines {
				if _, err := s.db.Exec(`INSERT INTO dune_runners(id,owner_id,name,kind,fabric_id,binding_revision,created_at) VALUES($1,$2,$3,'attached','attached',1,$4)`, m.ID, m.OwnerID, m.Name, m.CreatedAt); err != nil {
					t.Fatal(err)
				}
				if _, err := s.db.Exec(`INSERT INTO dune_machines(id,runner_id,credential_hash,os,arch) VALUES($1,$1,$2,$3,$4)`, m.ID, m.CredentialHash, m.OS, m.Arch); err != nil {
					t.Fatal(err)
				}
			}
			// A failure midway through ALTERs must not leave a half-upgraded schema.
			if _, err := s.db.Exec(`ALTER TABLE dune_principals ADD COLUMN auth_version BIGINT`); err != nil {
				t.Fatal(err)
			}
			if err := s.migrate(ctx); err == nil {
				t.Fatal("conflicting schema upgraded")
			}
			var version int
			if err := s.db.QueryRow(`SELECT version FROM dune_schema`).Scan(&version); err != nil || version != 1 {
				t.Fatal("failed migration advanced schema", err)
			}
			rows, err := s.db.Query(`SELECT * FROM dune_principals LIMIT 0`)
			if err != nil {
				t.Fatal(err)
			}
			columns, err := rows.Columns()
			rows.Close()
			if err != nil {
				t.Fatal(err)
			}
			for _, name := range columns {
				if name == "enabled" {
					t.Fatal("failed migration retained a partial ALTER")
				}
			}
			if _, err := s.db.Exec(`ALTER TABLE dune_principals DROP COLUMN auth_version`); err != nil {
				t.Fatal(err)
			}
			if err := s.Close(); err != nil {
				t.Fatal(err)
			}
			s, err = Open(ctx, config)
			if err != nil {
				t.Fatal(err)
			}
			local := identity.NewLocal(s, true)
			user, err := local.Authenticate(ctx, cookie)
			if err != nil {
				t.Fatal("upgrade lost existing login", err)
			}
			if _, _, err := local.Login(ctx, user.Email, "legacy-test-password"); err != nil {
				t.Fatal("upgrade lost password", err)
			}
			if id, err := s.MachineCredential(ctx, credential); err != nil || fixture.Machines[id].OwnerID != user.ID {
				t.Fatal("upgrade lost machine identity", err)
			}
			if err := s.db.QueryRow(`SELECT version FROM dune_schema`).Scan(&version); err != nil || version != schemaVersion {
				t.Fatal("schema not upgraded", err)
			}
		})
	}
}
