package metadata

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"errors"
	"io"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/aiomni/dune/internal/wire"
	"github.com/aiomni/dune/pkg/api"
	"github.com/aiomni/dune/pkg/gateway"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/stdlib"
)

type lostAckConnector struct {
	driver.Connector
	commits *atomic.Int32
}

func (c lostAckConnector) Connect(ctx context.Context) (driver.Conn, error) {
	conn, err := c.Connector.Connect(ctx)
	if err != nil {
		return nil, err
	}
	return lostAckConn{Conn: conn, commits: c.commits}, nil
}

type lostAckConn struct {
	driver.Conn
	commits *atomic.Int32
}

func (c lostAckConn) Begin() (driver.Tx, error) {
	tx, err := c.Conn.Begin()
	if err != nil {
		return nil, err
	}
	return lostAckTx{Tx: tx, commits: c.commits}, nil
}

type lostAckTx struct {
	driver.Tx
	commits *atomic.Int32
}

func (t lostAckTx) Commit() error {
	if err := t.Tx.Commit(); err != nil {
		return err
	}
	t.commits.Add(1)
	return io.ErrUnexpectedEOF
}

func directoryClaim(t *testing.T, store *Store) gateway.RouteClaim {
	t.Helper()
	token, _, err := store.IssueEnrollment(context.Background(), "route-owner", "route target")
	if err != nil {
		t.Fatal(err)
	}
	machine, _, err := store.Enroll(context.Background(), token, "linux", "amd64")
	if err != nil {
		t.Fatal(err)
	}
	return gateway.RouteClaim{Target: machine.ID, OwnerBootID: wire.ID(), OwnerAddress: "https://instance-a.test/peer", Binding: api.Binding{Target: machine.ID, Version: api.Version, Incarnation: wire.ID(), Generation: 1, Capabilities: []string{"runtime.list"}}}
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
	a, err := first.ConnectionDirectory(ctx)
	if err != nil {
		t.Fatal(err)
	}
	b, err := second.ConnectionDirectory(ctx)
	if err != nil {
		t.Fatal(err)
	}
	claim := directoryClaim(t, first)
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
	found, err := b.Resolve(ctx, claim.Target)
	if err != nil || !found.Published || found.Epoch != 1 {
		t.Fatal("confirmed route not visible across pools", found, err)
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
		t.Fatal("replacement did not advance epoch", replacement, err)
	}
	for _, mutate := range []func(context.Context, gateway.Route) error{a.Publish, a.Release, func(ctx context.Context, route gateway.Route) error { _, err := a.Renew(ctx, route); return err }} {
		if err := mutate(ctx, owned.Route); !errors.Is(err, gateway.ErrRouteStale) {
			t.Fatal("old owner changed replacement", err)
		}
	}
	if _, err := second.db.Exec(`UPDATE dune_routes SET expires_at=0 WHERE machine_id=$1`, claim.Target); err != nil {
		t.Fatal(err)
	}
	if _, err := b.Renew(ctx, replacement.Route); !errors.Is(err, gateway.ErrRouteStale) {
		t.Fatal("expired term was revived", err)
	}
	if _, err := second.db.Exec(`UPDATE dune_routes SET epoch=9223372036854775807 WHERE machine_id=$1`, claim.Target); err != nil {
		t.Fatal(err)
	}
	if _, err := b.Acquire(ctx, claim, 9223372036854775807); !errors.Is(err, ErrInvalidArgument) {
		t.Fatal("epoch overflow accepted", err)
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
	d, err := s.ConnectionDirectory(ctx)
	if err != nil {
		t.Fatal(err)
	}
	claim := directoryClaim(t, s)
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
	var runner string
	if err := lock.QueryRow(`SELECT id FROM dune_runners WHERE machine_id=$1 FOR UPDATE`, claim.Target).Scan(&runner); err != nil {
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
	d, err := s.ConnectionDirectory(ctx)
	if err != nil {
		t.Fatal(err)
	}
	claim := directoryClaim(t, s)
	parsed, err := pgx.ParseConfig(config.Postgres.URL)
	if err != nil {
		t.Fatal(err)
	}
	if err := config.Postgres.BeforeConnect(ctx, parsed); err != nil {
		t.Fatal(err)
	}
	var commits atomic.Int32
	faulty := &Store{postgres: true, db: sql.OpenDB(lostAckConnector{Connector: stdlib.GetConnector(*parsed), commits: &commits})}
	defer faulty.Close()
	interrupted := &connectionDirectory{store: faulty}
	if lease, err := interrupted.Acquire(ctx, claim, 0); !errors.Is(err, ErrCommitUnknown) || commits.Load() != 1 || lease.Epoch != 0 {
		t.Fatal("acquire commit was replayed or returned as success", lease, err)
	}
	owned, err := d.Resolve(ctx, claim.Target)
	if err != nil || owned.Epoch != 1 || owned.OwnerBootID != claim.OwnerBootID {
		t.Fatal("cannot reconcile committed ownership", owned, err)
	}
	if err := interrupted.Publish(ctx, owned.Route); !errors.Is(err, ErrCommitUnknown) || commits.Load() != 2 {
		t.Fatal("publish commit was replayed", err)
	}
	if _, err := interrupted.Renew(ctx, owned.Route); !errors.Is(err, ErrCommitUnknown) || commits.Load() != 3 {
		t.Fatal("renew commit was replayed", err)
	}
	if err := interrupted.Release(ctx, owned.Route); !errors.Is(err, ErrCommitUnknown) || commits.Load() != 4 {
		t.Fatal("release commit was replayed", err)
	}
}
