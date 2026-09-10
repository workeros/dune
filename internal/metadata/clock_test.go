package metadata

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/aiomni/dune/internal/wire"
	"github.com/aiomni/dune/pkg/gateway"
	"github.com/aiomni/dune/pkg/storage"
	"github.com/jackc/pgx/v5"
)

type postgresTestClock struct {
	admin *pgx.Conn
	table string
}

// postgresClockConfig shadows clock_timestamp only in this test schema. The
// production expression and PostgreSQL transaction behavior remain unchanged.
func postgresClockConfig(t *testing.T) (storage.Config, *postgresTestClock) {
	t.Helper()
	config, admin, schema := postgresConfig(t)
	quoted := pgx.Identifier{schema}.Sanitize()
	table := quoted + ".test_clock"
	if _, err := admin.Exec(context.Background(), fmt.Sprintf(`
		CREATE TABLE %s(offset_millis BIGINT NOT NULL);
		INSERT INTO %s VALUES(0);
		CREATE FUNCTION %s.clock_timestamp() RETURNS TIMESTAMPTZ
		LANGUAGE SQL VOLATILE AS $$
			SELECT pg_catalog.clock_timestamp() + offset_millis * INTERVAL '1 millisecond'
			FROM %s
		$$`, table, table, quoted, table)); err != nil {
		t.Fatal(err)
	}
	before := config.Postgres.BeforeConnect
	config.Postgres.BeforeConnect = func(ctx context.Context, connection *pgx.ConnConfig) error {
		if err := before(ctx, connection); err != nil {
			return err
		}
		// pg_catalog is explicit and follows the isolated schema so the test
		// function takes precedence over PostgreSQL's built-in function.
		connection.RuntimeParams["search_path"] = schema + ",pg_catalog"
		return nil
	}
	return config, &postgresTestClock{admin: admin, table: table}
}

func (c *postgresTestClock) set(t *testing.T, offset time.Duration) {
	t.Helper()
	if _, err := c.admin.Exec(context.Background(), "UPDATE "+c.table+" SET offset_millis=$1", offset.Milliseconds()); err != nil {
		t.Fatal(err)
	}
}

func TestPostgresDirectoryDatabaseClockDisturbance(t *testing.T) {
	config, clock := postgresClockConfig(t)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
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
	owner, err := first.ConnectionDirectory(ctx)
	if err != nil {
		t.Fatal(err)
	}
	peer, err := second.ConnectionDirectory(ctx)
	if err != nil {
		t.Fatal(err)
	}
	claim := directoryClaim(t, first)
	original, err := owner.Acquire(ctx, claim, 0)
	if err != nil {
		t.Fatal(err)
	}
	if err := owner.Publish(ctx, original.Route); err != nil {
		t.Fatal(err)
	}

	// A backward step retains the database's previously promised expiry. It
	// can delay failover, but it cannot enlarge the duration granted to the
	// local monotonic owner or let a competitor acquire the same epoch.
	clock.set(t, -time.Minute)
	backward, err := peer.Renew(ctx, original.Route)
	if err != nil {
		t.Fatal(err)
	}
	if backward.ExpiresAt.Before(original.ExpiresAt) || backward.ValidFor <= 0 || backward.ValidFor > connectionLeaseDuration {
		t.Fatal("backward step shortened persistence or enlarged local authority", backward)
	}
	contender := claim
	contender.OwnerBootID = wire.ID()
	contender.OwnerAddress = "https://instance-b.test/peer"
	if _, err := peer.Acquire(ctx, contender, original.Epoch); !errors.Is(err, gateway.ErrRouteBusy) {
		t.Fatal("backward step let another owner preempt a live term", err)
	}

	// A forward step expires the old database term. Its next renewal fails;
	// the successor advances the epoch, and every late old mutation is fenced.
	clock.set(t, time.Minute)
	if _, err := owner.Renew(ctx, original.Route); !errors.Is(err, gateway.ErrRouteStale) {
		t.Fatal("forward step renewed the old term", err)
	}
	expired, err := peer.Resolve(ctx, claim.Target)
	if err != nil || expired.Published || expired.ValidFor != 0 || expired.Epoch != original.Epoch {
		t.Fatal("forward step exposed an expired route", expired, err)
	}
	replacement, err := peer.Acquire(ctx, contender, original.Epoch)
	if err != nil || replacement.Epoch != original.Epoch+1 || replacement.ValidFor <= 0 || replacement.ValidFor > connectionLeaseDuration {
		t.Fatal("replacement did not establish a bounded new epoch", replacement, err)
	}
	if err := peer.Publish(ctx, replacement.Route); err != nil {
		t.Fatal(err)
	}
	for _, mutate := range []func() error{
		func() error { return owner.Publish(ctx, original.Route) },
		func() error { _, err := owner.Renew(ctx, original.Route); return err },
		func() error { return owner.Release(ctx, original.Route) },
	} {
		if err := mutate(); !errors.Is(err, gateway.ErrRouteStale) {
			t.Fatal("old epoch changed its successor", err)
		}
	}

	// Returning the database clock to normal leaves the future expiry in
	// place. This deliberately fails closed: the route stays on the new epoch,
	// and callers still receive no more than one local lease interval.
	clock.set(t, 0)
	retained, err := owner.Resolve(ctx, claim.Target)
	if err != nil || !retained.Published || retained.Epoch != replacement.Epoch || retained.ValidFor <= 0 || retained.ValidFor > connectionLeaseDuration {
		t.Fatal("clock correction lost the fenced replacement", retained, err)
	}
	third := contender
	third.OwnerBootID = wire.ID()
	third.OwnerAddress = "https://instance-c.test/peer"
	if _, err := owner.Acquire(ctx, third, replacement.Epoch); !errors.Is(err, gateway.ErrRouteBusy) {
		t.Fatal("clock correction preempted retained ownership", err)
	}
	kept, err := owner.Renew(ctx, replacement.Route)
	if err != nil || kept.ExpiresAt.Before(replacement.ExpiresAt) || kept.ValidFor <= 0 || kept.ValidFor > connectionLeaseDuration {
		t.Fatal("clock correction shortened or enlarged the replacement", kept, err)
	}
}
