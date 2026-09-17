package profiles_test

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"testing"

	"github.com/aiomni/dune/pkg/api"
	"github.com/aiomni/dune/pkg/profiles"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/stdlib"
	_ "modernc.org/sqlite"
)

func openStore(t *testing.T, backend string) (*profiles.Store, *sql.DB) {
	t.Helper()
	ctx := t.Context()
	var db *sql.DB
	var err error
	if backend == "postgres" {
		address := os.Getenv("DUNE_TEST_POSTGRES")
		if address == "" {
			t.Skip("DUNE_TEST_POSTGRES not configured")
		}
		config, err := pgx.ParseConfig(address)
		if err != nil {
			t.Fatal(err)
		}
		admin := stdlib.OpenDB(*config)
		schema := fmt.Sprintf("dune_profiles_%d", os.Getpid()) + fmt.Sprintf("_%x", []byte(t.Name()))
		// PostgreSQL identifiers are limited to 63 bytes.
		if len(schema) > 63 {
			schema = schema[:63]
		}
		quoted := pgx.Identifier{schema}.Sanitize()
		if _, err := admin.ExecContext(ctx, "CREATE SCHEMA "+quoted); err != nil {
			admin.Close()
			t.Fatal(err)
		}
		t.Cleanup(func() {
			_, err := admin.ExecContext(context.Background(), "DROP SCHEMA "+quoted+" CASCADE")
			if err != nil {
				t.Error(err)
			}
			admin.Close()
		})
		config.RuntimeParams["search_path"] = schema
		db = stdlib.OpenDB(*config)
	} else {
		db, err = sql.Open("sqlite", "file:"+filepath.Join(t.TempDir(), "profiles.sqlite")+"?_pragma=foreign_keys(1)&_pragma=busy_timeout(5000)&_txlock=immediate")
		if err != nil {
			t.Fatal(err)
		}
		db.SetMaxOpenConns(1)
	}
	t.Cleanup(func() { db.Close() })
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := profiles.CreateSchema(ctx, tx); err != nil {
		tx.Rollback()
		t.Fatal(err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}
	return profiles.NewStore(db), db
}

func example() profiles.Record {
	p := api.Profile{Version: 1, Kind: "agent", WorkingDirectory: "/workspace", Adapter: "acp", Start: api.Command{Argv: []string{"/missing/agent", "--acp"}, TimeoutSeconds: 42}, Env: map[string]string{"TOKEN": "test-secret"}}
	p.Setup.Steps = []api.Command{{Name: "prepare", Run: "echo prepare", Shell: "/bin/sh", TimeoutSeconds: 8}}
	return profiles.Record{OwnerID: "owner-a", Name: "Agent", Description: "full input", Profile: p, CreatedBy: profiles.Actor{Type: "user", Subject: "alice"}}
}

func TestProfileRevisionsAndOwnerIsolation(t *testing.T) {
	for _, backend := range []string{"sqlite", "postgres"} {
		t.Run(backend, func(t *testing.T) {
			store, db := openStore(t, backend)
			ctx := t.Context()
			first, err := store.Create(ctx, example())
			if err != nil {
				t.Fatal(err)
			}
			if first.Revision != 1 || first.ID == "" {
				t.Fatal("missing identity", first)
			}
			loaded, err := store.Get(ctx, first.OwnerID, profiles.Selection{ID: first.ID, Revision: 1})
			if err != nil {
				t.Fatal(err)
			}
			if loaded.Profile.Start.TimeoutSeconds != 42 || loaded.Profile.Setup.Steps[0].TimeoutSeconds != 8 || loaded.Profile.Env["TOKEN"] != "test-secret" {
				t.Fatal("complete Profile lost fields")
			}
			for _, selection := range []profiles.Selection{{ID: first.ID}, {ID: first.ID, Revision: 1}} {
				if _, err := store.Get(ctx, "owner-b", selection); !errors.Is(err, profiles.ErrNotFound) {
					t.Fatal("cross-owner read", err)
				}
			}
			wrong := first
			wrong.OwnerID = "owner-b"
			if _, err := store.Update(ctx, wrong); !errors.Is(err, profiles.ErrNotFound) {
				t.Fatal("cross-owner update", err)
			}
			if err := store.Delete(ctx, "owner-b", profiles.Selection{ID: first.ID, Revision: 1}); !errors.Is(err, profiles.ErrNotFound) {
				t.Fatal("cross-owner delete", err)
			}
			if list, err := store.List(ctx, "owner-b"); err != nil || len(list) != 0 {
				t.Fatal("cross-owner list", err)
			}
			edited := first
			edited.Name = "Changed"
			edited.Profile.Start = api.Command{Argv: []string{"second"}}
			second, err := store.Update(ctx, edited)
			if err != nil || second.Revision != 2 {
				t.Fatal("update", err)
			}
			old, err := store.Get(ctx, first.OwnerID, profiles.Selection{ID: first.ID, Revision: 1})
			if err != nil || old.Name != "Agent" || old.Profile.Start.TimeoutSeconds != 42 {
				t.Fatal("old revision changed", err)
			}
			if _, err := store.Update(ctx, first); !errors.Is(err, profiles.ErrConflict) {
				t.Fatal("stale edit", err)
			}
			if err := store.Delete(ctx, first.OwnerID, profiles.Selection{ID: first.ID, Revision: 1}); !errors.Is(err, profiles.ErrConflict) {
				t.Fatal("stale delete", err)
			}
			if list, err := store.List(ctx, first.OwnerID); err != nil || len(list) != 1 || list[0].Revision != 2 {
				t.Fatal("latest list", err)
			}
			if err := store.Delete(ctx, first.OwnerID, profiles.Selection{ID: first.ID, Revision: 2}); err != nil {
				t.Fatal(err)
			}
			var remaining int
			if err := db.QueryRowContext(ctx, "SELECT COUNT(*) FROM dune_profile_revisions").Scan(&remaining); err != nil || remaining != 0 {
				t.Fatal("orphaned revisions", remaining, err)
			}
		})
	}
}

func TestConcurrentProfileUpdatesHaveOneWinner(t *testing.T) {
	for _, backend := range []string{"sqlite", "postgres"} {
		t.Run(backend, func(t *testing.T) {
			store, _ := openStore(t, backend)
			first, err := store.Create(t.Context(), example())
			if err != nil {
				t.Fatal(err)
			}
			results := make(chan error, 8)
			var group sync.WaitGroup
			for range 8 {
				group.Go(func() { _, err := store.Update(t.Context(), first); results <- err })
			}
			group.Wait()
			close(results)
			wins := 0
			for err := range results {
				if err == nil {
					wins++
				} else if !errors.Is(err, profiles.ErrConflict) {
					t.Error(err)
				}
			}
			if wins != 1 {
				t.Fatal("concurrent winners", wins)
			}
		})
	}
}

func TestHostTransactionAndValidation(t *testing.T) {
	store, db := openStore(t, "sqlite")
	ctx := t.Context()
	empty := example()
	empty.Profile.Setup.Steps = nil
	saved, err := store.Create(ctx, empty)
	if err != nil || saved.Profile.Setup.Steps == nil {
		t.Fatal("empty setup must be an array", err)
	}
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	record, err := profiles.NewTransaction(tx).Create(ctx, example())
	if err != nil {
		tx.Rollback()
		t.Fatal(err)
	}
	if err := tx.Rollback(); err != nil {
		t.Fatal(err)
	}
	if _, err := store.Get(ctx, record.OwnerID, profiles.Selection{ID: record.ID}); !errors.Is(err, profiles.ErrNotFound) {
		t.Fatal("host rollback left Profile", err)
	}
	for _, change := range []func(*profiles.Record){
		func(r *profiles.Record) { r.OwnerID = "" }, func(r *profiles.Record) { r.Name = "" }, func(r *profiles.Record) { r.CreatedBy.Subject = "" }, func(r *profiles.Record) { r.Profile.Kind = "environment" }, func(r *profiles.Record) { r.Profile.WorkingDirectory = "relative" }, func(r *profiles.Record) { r.Profile.Start = api.Command{} },
	} {
		invalid := example()
		change(&invalid)
		if _, err := store.Create(ctx, invalid); !errors.Is(err, profiles.ErrInvalid) {
			t.Fatal("invalid input accepted", err)
		}
	}
}
