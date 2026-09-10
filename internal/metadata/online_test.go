package metadata

import (
	"context"
	"errors"
	"testing"
)

func TestPostgresOnlineConnections(t *testing.T) {
	config, _, _ := postgresConfig(t)
	ctx := context.Background()
	store, err := Open(ctx, config)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	directory, err := store.ConnectionDirectory(ctx)
	if err != nil {
		t.Fatal(err)
	}
	claim := directoryClaim(t, store)
	assertOnline := func(expected bool) {
		t.Helper()
		rows, err := store.OnlineConnections(ctx, []string{claim.Target, "absent"})
		if err != nil || rows[claim.Target] != expected || rows["absent"] || len(rows) > 1 {
			t.Fatal("incorrect bounded online facts", rows, err)
		}
	}
	assertOnline(false)
	lease, err := directory.Acquire(ctx, claim, 0)
	if err != nil {
		t.Fatal(err)
	}
	assertOnline(false)
	if err := directory.Publish(ctx, lease.Route); err != nil {
		t.Fatal(err)
	}
	assertOnline(true)
	if rows, err := store.OnlineConnections(ctx, []string{"absent"}); err != nil || len(rows) != 0 {
		t.Fatal("lookup disclosed an unrequested machine", rows, err)
	}
	if _, err := store.OnlineConnections(ctx, make([]string, 101)); !errors.Is(err, ErrInvalidArgument) {
		t.Fatal("unbounded page accepted", err)
	}
	if _, err := store.db.Exec(`UPDATE dune_routes SET expires_at=0 WHERE machine_id=$1`, claim.Target); err != nil {
		t.Fatal(err)
	}
	assertOnline(false)
}
