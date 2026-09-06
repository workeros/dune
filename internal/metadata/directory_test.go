package metadata

import (
	"context"
	"database/sql"
	"errors"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/aiomni/dune/internal/wire"
	"github.com/aiomni/dune/pkg/api"
	"github.com/aiomni/dune/pkg/gateway"
	"github.com/aiomni/dune/pkg/storage"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/stdlib"
)

func directoryClaim(t *testing.T, store *Store, recovery string) gateway.RouteClaim {
	t.Helper()
	intent := operationFixture(t, store)
	var target string
	if err := store.db.QueryRow(`SELECT id FROM dune_machines WHERE runner_id=$1`, intent.RunnerID).Scan(&target); err != nil {
		t.Fatal(err)
	}
	return gateway.RouteClaim{Target: target, RecoveryGeneration: recovery, OwnerBootID: wire.ID(), OwnerAddress: "https://instance-a.test/peer", Binding: api.Binding{Target: target, Version: api.Version, Incarnation: wire.ID(), Generation: 1, Capabilities: []string{"runtime.list"}}}
}

func TestPostgresConnectionDirectory(t *testing.T) {
	config, _, _ := postgresConfig(t)
	ctx := context.Background()
	first, err := Open(ctx, config)
	if err != nil {
		t.Fatal(err)
	}
	defer first.Close()
	second, err := Open(ctx, config)
	if err != nil {
		t.Fatal(err)
	}
	defer second.Close()
	recovery := wire.ID()
	a, err := first.ConnectionDirectory(ctx, recovery)
	if err != nil {
		t.Fatal(err)
	}
	b, err := second.ConnectionDirectory(ctx, recovery)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := second.ConnectionDirectory(ctx, wire.ID()); !errors.Is(err, gateway.ErrRouteStale) {
		t.Fatal("startup replaced recovery generation", err)
	}
	claim := directoryClaim(t, first, recovery)
	if _, err := a.Resolve(ctx, claim.Target); !errors.Is(err, gateway.ErrRouteNotFound) {
		t.Fatal("unconnected machine has a route", err)
	}
	var wg sync.WaitGroup
	winners := make(chan gateway.RouteLease, 8)
	for n := range 8 {
		wg.Go(func() {
			candidate := claim
			candidate.OwnerBootID = wire.ID()
			d := a
			if n%2 == 1 {
				d = b
			}
			lease, err := d.Acquire(ctx, candidate, 0)
			if err == nil {
				winners <- lease
			} else if !errors.Is(err, gateway.ErrRouteBusy) && !errors.Is(err, gateway.ErrRouteStale) {
				t.Error(err)
			}
		})
	}
	wg.Wait()
	close(winners)
	if len(winners) != 1 {
		t.Fatal("concurrent owners acquired the same machine", len(winners))
	}
	owned := <-winners
	if owned.Epoch != 1 || owned.Published || owned.ValidFor <= 0 || owned.ValidFor > connectionLeaseDuration {
		t.Fatal("invalid initial ownership", owned)
	}
	if _, err := b.Acquire(ctx, claim, owned.Epoch); !errors.Is(err, gateway.ErrRouteBusy) {
		t.Fatal("active ownership was preempted", err)
	}
	sameProcess := claim
	sameProcess.OwnerBootID = owned.OwnerBootID
	if _, err := a.Acquire(ctx, sameProcess, owned.Epoch); !errors.Is(err, gateway.ErrRouteBusy) {
		t.Fatal("same process opened a second live ownership term", err)
	}
	found, err := b.Resolve(ctx, claim.Target)
	if err != nil || found.Published || found.OwnerBootID != owned.OwnerBootID {
		t.Fatal("unconfirmed owner became routable", err)
	}
	bad := owned.Route
	bad.OwnerBootID = wire.ID()
	if err := b.Publish(ctx, bad); !errors.Is(err, gateway.ErrRouteStale) {
		t.Fatal("wrong process published route", err)
	}
	bad = owned.Route
	bad.Binding.Generation++
	if _, err := b.Renew(ctx, bad); !errors.Is(err, gateway.ErrRouteStale) {
		t.Fatal("renewal replaced execution identity", err)
	}
	if err := a.Publish(ctx, owned.Route); err != nil {
		t.Fatal(err)
	}
	found, err = b.Resolve(ctx, claim.Target)
	if err != nil || !found.Published || found.Epoch != 1 {
		t.Fatal("confirmed route not visible across pools", err)
	}
	if _, err := b.Renew(ctx, owned.Route); err != nil {
		t.Fatal(err)
	}
	if err := a.Release(ctx, owned.Route); err != nil {
		t.Fatal(err)
	}
	found, err = b.Resolve(ctx, claim.Target)
	if err != nil || found.Epoch != 1 || found.ValidFor != 0 || found.Published {
		t.Fatal("release deleted epoch or retained routing", found, err)
	}
	replacement, err := b.Acquire(ctx, claim, found.Epoch)
	if err != nil || replacement.Epoch != 2 {
		t.Fatal("replacement did not advance epoch", err)
	}
	for _, mutate := range []func(context.Context, gateway.Route) error{a.Publish, a.Release, func(ctx context.Context, route gateway.Route) error { _, err := a.Renew(ctx, route); return err }} {
		if err := mutate(ctx, owned.Route); !errors.Is(err, gateway.ErrRouteStale) {
			t.Fatal("old owner changed replacement", err)
		}
	}
	next, err := first.RotateConnectionRecovery(ctx, recovery)
	if err != nil || !validBootID(next) || next == recovery {
		t.Fatal("recovery did not establish fresh identity", err)
	}
	if _, err := second.RotateConnectionRecovery(ctx, recovery); !errors.Is(err, gateway.ErrRouteStale) {
		t.Fatal("stale operator rotated current recovery", err)
	}
	if _, err := a.Resolve(ctx, claim.Target); !errors.Is(err, gateway.ErrRouteStale) {
		t.Fatal("old service resolved after recovery", err)
	}
	if _, err := b.Renew(ctx, replacement.Route); !errors.Is(err, gateway.ErrRouteStale) {
		t.Fatal("old service renewed after recovery", err)
	}
	current, err := second.ConnectionDirectory(ctx, next)
	if err != nil {
		t.Fatal(err)
	}
	found, err = current.Resolve(ctx, claim.Target)
	if err != nil || found.ValidFor != 0 || found.Published || found.Epoch != 2 {
		t.Fatal("recovered directory exposed historical route", found, err)
	}
	claim.RecoveryGeneration = next
	last, err := current.Acquire(ctx, claim, found.Epoch)
	if err != nil || last.Epoch != 3 {
		t.Fatal("recovery lost epoch history", err)
	}
	if _, err := second.db.Exec(`UPDATE dune_routes SET expires_at=0 WHERE machine_id=$1`, claim.Target); err != nil {
		t.Fatal(err)
	}
	if _, err := current.Renew(ctx, last.Route); !errors.Is(err, gateway.ErrRouteStale) {
		t.Fatal("expired term was revived", err)
	}
	if err := current.Publish(ctx, last.Route); !errors.Is(err, gateway.ErrRouteStale) {
		t.Fatal("expired term was published", err)
	}
	if _, err := second.db.Exec(`UPDATE dune_routes SET epoch=9223372036854775807 WHERE machine_id=$1`, claim.Target); err != nil {
		t.Fatal(err)
	}
	if _, err := current.Acquire(ctx, claim, 9223372036854775807); !errors.Is(err, ErrInvalidArgument) {
		t.Fatal("epoch overflow accepted", err)
	}
}

func TestDirectoryBackendAndSchemaUpgrade(t *testing.T) {
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
			if backend == "sqlite" {
				if _, err := s.ConnectionDirectory(ctx, wire.ID()); err == nil {
					t.Fatal("SQLite accepted clustered ownership")
				}
			}
			claim := directoryClaim(t, s, wire.ID())
			if err := s.transaction(ctx, func(tx *sql.Tx) error {
				for _, query := range []string{`DROP TABLE dune_routes`, `DROP TABLE dune_cluster`, `UPDATE dune_schema SET version=9`, `CREATE TABLE dune_routes (collision TEXT)`} {
					if _, err := tx.Exec(query); err != nil {
						return err
					}
				}
				return nil
			}); err != nil {
				t.Fatal(err)
			}
			if err := s.migrate(ctx); err == nil {
				t.Fatal("partial directory migration accepted")
			}
			var version int
			if err := s.db.QueryRow(`SELECT version FROM dune_schema`).Scan(&version); err != nil || version != 9 {
				t.Fatal("failed migration advanced version", err)
			}
			if _, err := s.db.Exec(`DROP TABLE dune_routes`); err != nil {
				t.Fatal(err)
			}
			if err := s.migrate(ctx); err != nil {
				t.Fatal("migration did not roll back cluster table", err)
			}
			var target string
			if err := s.db.QueryRow(`SELECT id FROM dune_machines WHERE id=$1`, claim.Target).Scan(&target); err != nil {
				t.Fatal("upgrade lost machine identity", err)
			}
		})
	}
}

func TestPostgresDirectoryRechecksExpiredLeaseAfterLock(t *testing.T) {
	config, _, _ := postgresConfig(t)
	ctx := context.Background()
	s, err := Open(ctx, config)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	recovery := wire.ID()
	d, err := s.ConnectionDirectory(ctx, recovery)
	if err != nil {
		t.Fatal(err)
	}
	claim := directoryClaim(t, s, recovery)
	owned, err := d.Acquire(ctx, claim, 0)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.db.Exec(`UPDATE dune_routes SET expires_at=`+s.databaseClock()+`+100 WHERE machine_id=$1`, claim.Target); err != nil {
		t.Fatal(err)
	}
	lock, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer lock.Rollback()
	var target string
	if err := lock.QueryRow(`SELECT id FROM dune_machines WHERE id=$1 FOR UPDATE`, claim.Target).Scan(&target); err != nil {
		t.Fatal(err)
	}
	finished := make(chan error, 1)
	go func() { _, err := d.Renew(ctx, owned.Route); finished <- err }()
	time.Sleep(150 * time.Millisecond)
	if err := lock.Commit(); err != nil {
		t.Fatal(err)
	}
	if err := <-finished; !errors.Is(err, gateway.ErrRouteStale) {
		t.Fatal("lock delay revived expired lease", err)
	}
}

func TestPostgresDirectoryCommitLoss(t *testing.T) {
	config, _, _ := postgresConfig(t)
	ctx := context.Background()
	s, err := Open(ctx, config)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	recovery := wire.ID()
	d, err := s.ConnectionDirectory(ctx, recovery)
	if err != nil {
		t.Fatal(err)
	}
	claim := directoryClaim(t, s, recovery)
	parsed, err := pgx.ParseConfig(config.Postgres.URL)
	if err != nil {
		t.Fatal("invalid test database configuration")
	}
	if err := config.Postgres.BeforeConnect(ctx, parsed); err != nil {
		t.Fatal(err)
	}
	var commits atomic.Int32
	faulty := &Store{postgres: true, db: sql.OpenDB(lostAckConnector{Connector: stdlib.GetConnector(*parsed), commits: &commits})}
	defer faulty.Close()
	interrupted := &connectionDirectory{store: faulty, recovery: recovery}
	if lease, err := interrupted.Acquire(ctx, claim, 0); !errors.Is(err, ErrCommitUnknown) || commits.Load() != 1 || lease.Epoch != 0 {
		t.Fatal("acquire commit was replayed or returned as success", err)
	}
	owned, err := d.Resolve(ctx, claim.Target)
	if err != nil || owned.Epoch != 1 || owned.OwnerBootID != claim.OwnerBootID {
		t.Fatal("cannot reconcile committed ownership", err)
	}
	if err := interrupted.Publish(ctx, owned.Route); !errors.Is(err, ErrCommitUnknown) || commits.Load() != 2 {
		t.Fatal("publish commit was replayed", err)
	}
	found, err := d.Resolve(ctx, claim.Target)
	if err != nil || !found.Published {
		t.Fatal("cannot reconcile publication", err)
	}
	if _, err := interrupted.Renew(ctx, owned.Route); !errors.Is(err, ErrCommitUnknown) || commits.Load() != 3 {
		t.Fatal("renew commit was replayed", err)
	}
	if err := interrupted.Release(ctx, owned.Route); !errors.Is(err, ErrCommitUnknown) || commits.Load() != 4 {
		t.Fatal("release commit was replayed", err)
	}
	found, err = d.Resolve(ctx, claim.Target)
	if err != nil || found.Published || found.ValidFor != 0 || found.Epoch != 1 {
		t.Fatal("cannot reconcile release", err)
	}
	next, err := faulty.RotateConnectionRecovery(ctx, recovery)
	if !errors.Is(err, ErrCommitUnknown) || commits.Load() != 5 || !validBootID(next) {
		t.Fatal("recovery commit was replayed or cannot be reconciled", err)
	}
	if _, err := s.ConnectionDirectory(ctx, next); err != nil {
		t.Fatal("cannot reconcile recovery", err)
	}
}

func TestPostgresRecoveryRequiresExistingSchema(t *testing.T) {
	config, admin, schema := postgresConfig(t)
	ctx := context.Background()
	if source, err := OpenPostgresSource(ctx, config.Postgres); err == nil {
		source.Close()
		t.Fatal("recovery opened an uninitialized database")
	}
	var count int
	if err := admin.QueryRow(ctx, `SELECT COUNT(*) FROM information_schema.tables WHERE table_schema=$1`, schema).Scan(&count); err != nil || count != 0 {
		t.Fatal("recovery initialized an empty schema", count, err)
	}
	s, err := Open(ctx, config)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	if _, err := s.db.Exec(`UPDATE dune_schema SET version=9`); err != nil {
		t.Fatal(err)
	}
	if source, err := OpenPostgresSource(ctx, config.Postgres); err == nil {
		source.Close()
		t.Fatal("recovery accepted outdated schema")
	}
	if err := s.db.QueryRow(`SELECT version FROM dune_schema`).Scan(&count); err != nil || count != 9 {
		t.Fatal("recovery upgraded schema", count, err)
	}
}
