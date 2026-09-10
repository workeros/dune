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
			ctx := context.Background()
			s, err := Open(ctx, config)
			if err != nil {
				t.Fatal(err)
			}
			defer s.Close()
			local := identity.NewLocal(s, true)
			user, cookie, err := local.Register(ctx, "Owner@example.test", "a-strong-test-password")
			if err != nil || user.Email != "owner@example.test" {
				t.Fatal("local registration", user, err)
			}
			if _, _, err := local.Register(ctx, "OWNER@example.test", "a-strong-test-password"); !errors.Is(err, ErrConflict) {
				t.Fatal("duplicate registration", err)
			}
			failed := identity.Account{User: identity.User{ID: wire.ID(), Email: "failed@example.test"}, Salt: "salt", PasswordHash: "hash"}
			if err := s.RegisterAccount(ctx, failed, tokenHash(cookie), time.Now().Add(time.Hour).Unix()); !errors.Is(err, ErrConflict) {
				t.Fatal("cross-object conflict", err)
			}
			if _, err := s.ReadAccount(ctx, failed.Email); !errors.Is(err, identity.ErrUnauthorized) {
				t.Fatal("failed registration left an account")
			}
			token, _, err := s.IssueEnrollment(ctx, user.ID, "machine")
			if err != nil {
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
						t.Error(err)
					}
				})
			}
			wg.Wait()
			if wins.Load() != 1 {
				t.Fatal("enrollment was not single-use", wins.Load())
			}
			var runner, kind string
			var revision int64
			if err := s.db.QueryRowContext(ctx, `SELECT id,kind,binding_revision FROM dune_runners WHERE machine_id=$1`, machine.ID).Scan(&runner, &kind, &revision); err != nil || runner != machine.RunnerID || kind != "attached" || revision != 1 {
				t.Fatal("runner binding not committed", err)
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
						t.Error(err)
					}
				})
			}
			wg.Wait()
			if wins.Load() != 1 {
				t.Fatal("session limit race", wins.Load())
			}
			if err := s.Revoke(ctx, user.ID, machine.ID); err != nil {
				t.Fatal(err)
			}
			if _, err := s.MachineCredential(ctx, credential); !errors.Is(err, identity.ErrUnauthorized) {
				t.Fatal("credential survived persistent revocation")
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
	if second, err := Open(ctx, config); err == nil {
		second.Close()
		t.Fatal("multiple SQLite instances accepted")
	}
	if _, err := s.db.ExecContext(ctx, `INSERT INTO dune_sessions(hash,user_id,expires_at,auth_version) VALUES('hash','missing',1,1)`); err == nil {
		t.Fatal("foreign keys disabled")
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	if reopened, err := Open(ctx, config); err != nil {
		t.Fatal(err)
	} else if err := reopened.Close(); err != nil {
		t.Fatal(err)
	}
	lock, err := lockDirectory(dir)
	if err != nil {
		t.Fatal(err)
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
			t.Fatal("invalid database combination accepted")
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
		if err := os.Remove(file); err != nil {
			t.Fatal(err)
		}
	}
	content, err := os.ReadFile(outside)
	if err != nil || string(content) != "preserve" {
		t.Fatal("unsafe open modified another file", err)
	}
}
