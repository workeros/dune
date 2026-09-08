package metadata

import (
	"context"
	"database/sql"
	"errors"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/aiomni/dune/internal/identity"
	"github.com/aiomni/dune/internal/wire"
	"github.com/aiomni/dune/pkg/storage"
	"github.com/jackc/pgx/v5"
)

func postgresConfig(t *testing.T) (storage.Config, *pgx.Conn, string) {
	t.Helper()
	address := os.Getenv("DUNE_TEST_POSTGRES")
	if address == "" {
		t.Skip("DUNE_TEST_POSTGRES not configured")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	admin, err := pgx.Connect(ctx, address)
	if err != nil {
		t.Fatal(err)
	}
	name := "dune_test_" + wire.ID()
	quoted := pgx.Identifier{name}.Sanitize()
	if _, err := admin.Exec(ctx, "CREATE SCHEMA "+quoted); err != nil {
		admin.Close(ctx)
		t.Fatal(err)
	}
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		if _, err := admin.Exec(ctx, "DROP SCHEMA "+quoted+" CASCADE"); err != nil {
			t.Error(err)
		}
		admin.Close(ctx)
	})
	return storage.Config{Postgres: &storage.Postgres{URL: address, BeforeConnect: func(ctx context.Context, c *pgx.ConnConfig) error {
		c.RuntimeParams["search_path"] = name
		return nil
	}}}, admin, name
}

func TestBusinessTransactions(t *testing.T) {
	for _, backend := range []string{"sqlite", "postgres"} {
		t.Run(backend, func(t *testing.T) {
			config := storage.Config{SQLiteDir: filepath.Join(t.TempDir(), "metadata")}
			if backend == "postgres" {
				config, _, _ = postgresConfig(t)
			}
			ctx, cancel := context.WithTimeout(context.Background(), 45*time.Second)
			defer cancel()
			s, err := Open(ctx, config)
			if err != nil {
				t.Fatal(err)
			}
			defer func() { s.Close() }()
			local := identity.NewLocal(s, true)
			user, cookie, err := local.Register(ctx, "Owner@example.test", "a-strong-test-password")
			if err != nil {
				t.Fatal(err)
			}
			if user.Email != "owner@example.test" {
				t.Fatal("local identity was not normalized")
			}
			if _, _, err := local.Register(ctx, "OWNER@example.test", "a-strong-test-password"); !errors.Is(err, ErrConflict) {
				t.Fatalf("duplicate registration: %v", err)
			}
			// The third insert fails after the principal and local account inserts.
			// A shared transaction must roll back all three, not just the session.
			a := identity.Account{User: identity.User{ID: wire.ID(), Email: "failed@example.test"}, Salt: "salt", PasswordHash: "hash"}
			if err := s.RegisterAccount(ctx, a, tokenHash(cookie), time.Now().Add(time.Hour).Unix()); !errors.Is(err, ErrConflict) {
				t.Fatalf("cross-object conflict: %v", err)
			}
			var count int
			if err := s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM dune_principals WHERE id=$1`, a.ID).Scan(&count); err != nil || count != 0 {
				t.Fatal("failed registration left a principal", err)
			}
			if _, err := s.ReadAccount(ctx, a.Email); !errors.Is(err, identity.ErrUnauthorized) {
				t.Fatal("failed registration left a local account")
			}
			token, _, err := s.IssueEnrollment(ctx, user.ID, "machine")
			if err != nil {
				t.Fatal(err)
			}
			// Remove the destination table briefly to fail after Runner insertion.
			if _, err := s.db.ExecContext(ctx, `ALTER TABLE dune_machines RENAME TO dune_machines_paused`); err != nil {
				t.Fatal(err)
			}
			if _, _, err := s.Enroll(ctx, token, "linux", "amd64"); err == nil {
				t.Fatal("registration succeeded without machine persistence")
			}
			if err := s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM dune_runners`).Scan(&count); err != nil || count != 0 {
				t.Fatal("failed enrollment left a Runner", err)
			}
			if _, err := s.db.ExecContext(ctx, `ALTER TABLE dune_machines_paused RENAME TO dune_machines`); err != nil {
				t.Fatal(err)
			}
			other := s
			if backend == "postgres" {
				other, err = Open(ctx, config)
				if err != nil {
					t.Fatal(err)
				}
				defer other.Close()
			}
			var wins atomic.Int32
			var wg sync.WaitGroup
			var machine Machine
			var credential string
			var resultMu sync.Mutex
			for i := range 8 {
				store := s
				if i%2 == 1 {
					store = other
				}
				wg.Go(func() {
					m, c, err := store.Enroll(ctx, token, "linux", "amd64")
					if err == nil {
						wins.Add(1)
						resultMu.Lock()
						machine, credential = m, c
						resultMu.Unlock()
					} else if !errors.Is(err, identity.ErrUnauthorized) {
						t.Errorf("unexpected enrollment failure: %v", err)
					}
				})
			}
			wg.Wait()
			if wins.Load() != 1 {
				t.Fatalf("enrollment successes = %d", wins.Load())
			}
			var runner, kind string
			var revision int64
			if err := s.db.QueryRowContext(ctx, `SELECT r.id,r.kind,r.binding_revision FROM dune_runners r JOIN dune_machines m ON m.runner_id=r.id WHERE m.id=$1`, machine.ID).Scan(&runner, &kind, &revision); err != nil || runner != machine.RunnerID || runner == machine.ID || kind != "attached" || revision != 1 {
				t.Fatal("initial Runner binding was not committed", err)
			}
			if id, err := other.MachineCredential(ctx, credential); err != nil || id != machine.ID {
				t.Fatal("machine identity missing", err)
			}
			if _, err := s.MachineCredential(ctx, cookie); !errors.Is(err, identity.ErrUnauthorized) {
				t.Fatal("browser session authenticated fabricd")
			}
			if _, err := local.Authenticate(ctx, credential); !errors.Is(err, identity.ErrUnauthorized) {
				t.Fatal("machine credential authenticated a user")
			}
			// The account row lock serializes limits across separate PostgreSQL pools.
			wins.Store(0)
			for i := range 8 {
				store := s
				if i%2 == 1 {
					store = other
				}
				wg.Go(func() {
					err := store.CreateSession(ctx, user.ID, tokenHash(wire.ID()), time.Now().Add(time.Hour).Unix(), 2)
					if err == nil {
						wins.Add(1)
					} else if !errors.Is(err, identity.ErrSessionLimit) {
						t.Errorf("session limit: %v", err)
					}
				})
			}
			wg.Wait()
			if wins.Load() != 1 {
				t.Fatalf("concurrent session limit successes=%d", wins.Load())
			}
			if err := s.Close(); err != nil {
				t.Fatal(err)
			}
			s, err = Open(ctx, config)
			if err != nil {
				t.Fatal(err)
			}
			local = identity.NewLocal(s, true)
			if got, err := local.Authenticate(ctx, cookie); err != nil || got.ID != user.ID {
				t.Fatal("session lost on restart", err)
			}
			list, err := s.Machines(ctx, user.ID)
			if err != nil || len(list) != 1 || list[0].ID != machine.ID {
				t.Fatal("binding lost on restart", err)
			}
			if own, err := s.Owns(ctx, "another-user", machine.ID); err != nil || own {
				t.Fatal("cross-user ownership", err)
			}
			if err := s.Revoke(ctx, "another-user", machine.ID); !errors.Is(err, ErrNotFound) {
				t.Fatal("cross-user revoke accepted")
			}
			if err := s.Revoke(ctx, user.ID, machine.ID); err != nil {
				t.Fatal(err)
			}
			if _, err := s.MachineCredential(ctx, credential); !errors.Is(err, identity.ErrUnauthorized) {
				t.Fatal("machine credential survived revocation")
			}
			if err := local.Logout(ctx, cookie); err != nil {
				t.Fatal(err)
			}
			if _, err := local.Authenticate(ctx, cookie); !errors.Is(err, identity.ErrUnauthorized) {
				t.Fatal("session survived logout")
			}
		})
	}
}

func TestSQLiteExclusivitySchemaAndConstraints(t *testing.T) {
	ctx := context.Background()
	dir := filepath.Join(t.TempDir(), "metadata")
	config := storage.Config{SQLiteDir: dir}
	s, err := Open(ctx, config)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	for range 2 {
		if second, err := Open(ctx, config); err == nil {
			second.Close()
			t.Fatal("multiple SQLite instances accepted")
		}
	}
	if _, err := s.db.ExecContext(ctx, `INSERT INTO dune_sessions(hash,principal_id,expires_at) VALUES('hash','missing',1)`); err == nil {
		t.Fatal("foreign keys disabled on a pooled connection")
	}
	if _, err := s.db.ExecContext(ctx, `UPDATE dune_schema SET fingerprint='unknown'`); err != nil {
		t.Fatal(err)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	if bad, err := Open(ctx, config); err == nil {
		bad.Close()
		t.Fatal("unknown schema opened")
	}
	// Failed opens must release the directory lock, too.
	lock, err := lockDirectory(dir)
	if err != nil {
		t.Fatal("failed open retained its lock", err)
	}
	lock.Close()
	info, err := os.Stat(filepath.Join(dir, "metadata.sqlite"))
	if err != nil || info.Mode().Perm() != 0600 {
		t.Fatal("SQLite file is not private", err)
	}
	if err := s.db.PingContext(ctx); !errors.Is(err, sql.ErrConnDone) && err == nil {
		t.Fatal("closed pool stayed open")
	}
}

func TestSQLiteRejectsUnsafeData(t *testing.T) {
	ctx := context.Background()
	for _, config := range []storage.Config{{}, {SQLiteDir: "relative"}, {SQLiteDir: "/"}, {SQLiteDir: t.TempDir(), Postgres: &storage.Postgres{URL: "postgres://localhost/postgres"}}} {
		if store, err := Open(ctx, config); err == nil {
			store.Close()
			t.Fatal("invalid database combination or directory accepted")
		}
	}
	dir := filepath.Join(t.TempDir(), "unsafe")
	if err := os.Mkdir(dir, 0700); err != nil {
		t.Fatal(err)
	}
	outside := filepath.Join(t.TempDir(), "untouched")
	if err := os.WriteFile(outside, []byte("preserve"), 0600); err != nil {
		t.Fatal(err)
	}
	for _, link := range []func(string, string) error{os.Symlink, os.Link} {
		file := filepath.Join(dir, "metadata.sqlite")
		if err := link(outside, file); err != nil {
			t.Fatal(err)
		}
		if store, err := Open(ctx, storage.Config{SQLiteDir: dir}); err == nil {
			store.Close()
			t.Fatal("linked SQLite file accepted")
		}
		os.Remove(file)
	}
	content, err := os.ReadFile(outside)
	if err != nil || string(content) != "preserve" {
		t.Fatal("unsafe open modified another file", err)
	}
}
