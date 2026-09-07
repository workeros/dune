package fabricd

import (
	"context"
	"errors"
	"net"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/aiomni/dune/internal/authorization"
	"github.com/aiomni/dune/internal/identity"
	"github.com/aiomni/dune/internal/metadata"
	"github.com/aiomni/dune/internal/wire"
	"github.com/aiomni/dune/pkg/access"
	"github.com/aiomni/dune/pkg/api"
	"github.com/aiomni/dune/pkg/client"
	"github.com/aiomni/dune/pkg/gateway"
	"github.com/aiomni/dune/pkg/storage"
	"github.com/jackc/pgx/v5"
)

func TestPostgresOwnedReverseConnections(t *testing.T) {
	address := os.Getenv("DUNE_TEST_POSTGRES")
	if address == "" {
		t.Skip("DUNE_TEST_POSTGRES not configured")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 45*time.Second)
	defer cancel()
	admin, err := pgx.Connect(ctx, address)
	if err != nil {
		t.Fatal(err)
	}
	schema := "dune_owner_" + wire.ID()
	if _, err := admin.Exec(ctx, "CREATE SCHEMA "+pgx.Identifier{schema}.Sanitize()); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		cleanup, stop := context.WithTimeout(context.Background(), 5*time.Second)
		defer stop()
		_, err := admin.Exec(cleanup, "DROP SCHEMA "+pgx.Identifier{schema}.Sanitize()+" CASCADE")
		if err != nil {
			t.Error(err)
		}
		admin.Close(cleanup)
	})
	config := storage.Config{Postgres: &storage.Postgres{URL: address, BeforeConnect: func(_ context.Context, c *pgx.ConnConfig) error { c.RuntimeParams["search_path"] = schema; return nil }}}
	first, err := metadata.Open(ctx, config)
	if err != nil {
		t.Fatal(err)
	}
	defer first.Close()
	second, err := metadata.Open(ctx, config)
	if err != nil {
		t.Fatal(err)
	}
	defer second.Close()
	user, cookie, err := identity.NewLocal(first, true).Register(ctx, "owner@routing.test", "routing-test-password")
	if err != nil {
		t.Fatal(err)
	}
	token, _, err := first.IssueEnrollment(ctx, user.ID, "owned execution")
	if err != nil {
		t.Fatal(err)
	}
	machine, _, err := first.Enroll(ctx, token, "linux", "amd64")
	if err != nil {
		t.Fatal(err)
	}
	recovery := wire.ID()
	d1, err := first.ConnectionDirectory(ctx, recovery)
	if err != nil {
		t.Fatal(err)
	}
	d2, err := second.ConnectionDirectory(ctx, recovery)
	if err != nil {
		t.Fatal(err)
	}
	g1, err := gateway.NewWithDirectory(d1, "https://owner-a.test/peer", recovery)
	if err != nil {
		t.Fatal(err)
	}
	g2, err := gateway.NewWithDirectory(d2, "https://owner-b.test/peer", recovery)
	if err != nil {
		t.Fatal(err)
	}
	engine, err := Open(ctx, filepath.Join(t.TempDir(), "fabricd"))
	if err != nil {
		t.Fatal(err)
	}
	competitor := newEngine(ctx)
	var wg sync.WaitGroup
	var peerDials atomic.Int32
	ownerChecker := &peerOwnerChecker{}
	auth1, err := authorization.New(ctx, identity.NewLocal(first, true), first, ownerChecker).WithPeers(g1.BootID())
	if err != nil {
		t.Fatal(err)
	}
	auth2, err := authorization.New(ctx, identity.NewLocal(second, true), second, ownerChecker).WithPeers(g2.BootID())
	if err != nil {
		t.Fatal(err)
	}
	entry, err := gateway.NewWithPeers(d2, "https://entry.test/peer", recovery, func(_ context.Context, source string, destination gateway.Route) (net.Conn, error) {
		peerDials.Add(1)
		owner := g1
		authorizer := auth1
		if destination.OwnerBootID == g2.BootID() {
			owner = g2
			authorizer = auth2
		} else if destination.OwnerBootID != g1.BootID() {
			return nil, gateway.ErrRouteStale
		}
		binding, handler, err := authorizer.Peer(source, machine.ID)
		if err != nil {
			return nil, err
		}
		left, right := net.Pipe()
		wg.Go(func() {
			_ = owner.ServeConn(ctx, left, binding, handler)
		})
		return right, nil
	})
	if err != nil {
		t.Fatal(err)
	}
	entryRepository := &peerEntryRepository{Repository: second}
	entryAuth, err := authorization.NewLocal(ctx, identity.NewLocal(second, true), entryRepository).WithPeers(entry.BootID())
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		cancel()
		entry.Close()
		g1.Close()
		g2.Close()
		wg.Wait()
		engine.Close()
		competitor.Close()
		engine.tmux.Close()
	}()
	connectFabric := func(g *gateway.Gateway, e *Engine) <-chan error {
		left, right := net.Pipe()
		binding, handler, err := (access.Grant{Target: machine.ID, Role: gateway.RoleDaemon}).Bind()
		if err != nil {
			t.Fatal(err)
		}
		done := make(chan error, 1)
		wg.Go(func() { _ = g.ServeConn(ctx, left, binding, handler) })
		wg.Go(func() { done <- e.ServeConn(ctx, right, machine.ID) })
		return done
	}
	waitOnline := func(g *gateway.Gateway) {
		t.Helper()
		for !g.Online(machine.ID) {
			select {
			case <-ctx.Done():
				t.Fatal("owner did not become online")
			case <-time.After(time.Millisecond):
			}
		}
	}
	connectClient := func(g *gateway.Gateway) *client.Client {
		t.Helper()
		left, right := net.Pipe()
		binding, handler, err := (access.Grant{Target: machine.ID, Role: gateway.RoleSDK}).Bind()
		if err != nil {
			t.Fatal(err)
		}
		if g == entry {
			grant, err := entryAuth.Client(ctx, cookie, machine.ID)
			if err != nil {
				t.Fatal(err)
			}
			defer grant.Close()
			binding, handler, err = entryAuth.Authorize(grant.Token())
			if err != nil {
				t.Fatal(err)
			}
		}
		wg.Go(func() { _ = g.ServeConn(ctx, left, binding, handler) })
		c, err := client.Connect(ctx, right, machine.ID)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { c.Close() })
		return c
	}
	originalDone := connectFabric(g1, engine)
	waitOnline(g1)
	original := connectClient(entry)
	ownerChecker.denied.Store(true)
	if _, err := original.List(ctx); err == nil {
		t.Fatal("entry allow bypassed owner policy")
	}
	ownerChecker.denied.Store(false)
	if original.Binding.RouteEpoch != 1 || original.Binding.RouteRecovery != recovery {
		t.Fatal("SDK did not receive fixed ownership", original.Binding)
	}
	owned, err := d2.Resolve(ctx, machine.ID)
	if err != nil || !owned.Published || owned.Binding.Incarnation != original.Binding.Incarnation {
		t.Fatal("confirmed binding not published in PostgreSQL", owned, err)
	}
	runtime, terminal, err := original.Start(ctx, api.Profile{Version: 1, Kind: "agent", WorkingDirectory: t.TempDir(), Adapter: "pty", Start: api.Command{Argv: []string{"/bin/sh"}}})
	if err != nil {
		t.Fatal(err)
	}
	defer terminal.Close()
	select {
	case err := <-connectFabric(g2, competitor):
		var denied *api.Error
		if !errors.As(err, &denied) || denied.Code != "ROUTE_STALE" {
			t.Fatal("live owner was not rejected", err)
		}
	case <-ctx.Done():
		t.Fatal("competing owner did not finish handshake")
	}
	if g2.Online(machine.ID) {
		t.Fatal("competing Gateway published a route")
	}
	if _, err := original.List(ctx); err != nil {
		t.Fatal("competitor interrupted original owner", err)
	}
	// Same execution binding with a different route epoch must be rejected before
	// a business request reaches the fabric. Restore only the explicit test edit.
	snapshot := original.Binding
	original.Binding.RouteEpoch++
	if _, err := original.List(ctx); err == nil {
		t.Fatal("SDK followed another ownership term")
	}
	original.Binding = snapshot
	// Normal control renewals must carry the same epoch for more than a full
	// original lease, using the real shared PostgreSQL directory.
	select {
	case <-ctx.Done():
		t.Fatal(ctx.Err())
	case <-time.After(wire.InputLeaseDuration + time.Second):
	}
	if _, err := original.List(ctx); err != nil {
		t.Fatal("database renewal broke execution", err)
	}
	g1.Close()
	select {
	case <-originalDone:
	case <-ctx.Done():
		t.Fatal("old connection did not stop")
	}
	// Close returns before ServeConn's conditional directory cleanup finishes.
	for {
		old, err := d2.Resolve(ctx, machine.ID)
		if err != nil {
			t.Fatal(err)
		}
		if old.ValidFor == 0 {
			break
		}
		select {
		case <-ctx.Done():
			t.Fatal("old ownership not released")
		case <-time.After(time.Millisecond):
		}
	}
	connectFabric(g2, engine)
	waitOnline(g2)
	if _, err := original.List(ctx); err == nil {
		t.Fatal("old peer connection followed the new owner")
	}
	current := connectClient(entry)
	if peerDials.Load() != 2 {
		t.Fatal("peer route was retried instead of explicitly reconnected", peerDials.Load())
	}
	if current.Binding.RouteEpoch != 2 || current.Binding.RouteRecovery != recovery || current.Binding.Incarnation != snapshot.Incarnation || current.Binding.Generation <= snapshot.Generation {
		t.Fatal("reconnection did not advance ownership independently", current.Binding)
	}
	if err := d1.Release(ctx, owned.Route); !errors.Is(err, gateway.ErrRouteStale) {
		t.Fatal("old owner erased replacement", err)
	}
	got, err := current.Get(ctx, runtime)
	if err != nil || got.State != "running" || got.Incarnation != runtime.Incarnation {
		t.Fatal("ownership transfer destroyed PTY", got, err)
	}
	var reattached *client.Stream
	attachDeadline := time.Now().Add(5 * time.Second)
	for {
		reattached, err = current.Attach(ctx, runtime, false)
		if err == nil {
			break
		}
		if time.Now().After(attachDeadline) {
			t.Fatal(err)
		}
		time.Sleep(10 * time.Millisecond)
	}
	defer reattached.Close()
	if _, err := reattached.Input([]byte("printf 'OWNER_%s_OK\\n' TRANSFER\n")); err != nil {
		t.Fatal(err)
	}
	for {
		message, err := reattached.Recv()
		if err != nil {
			t.Fatal(err)
		}
		if message.Kind == "data" && strings.Contains(string(message.Data), "OWNER_TRANSFER_OK") {
			break
		}
	}
	// Logout on the other pool must close an idle peer user stream.
	// Deliberately hold the entry's validity answer stale to prove the owner's
	// independent watchdog, rather than relying on the entry to close the path.
	entryRepository.stale.Store(true)
	if err := identity.NewLocal(first, true).Logout(ctx, cookie); err != nil {
		t.Fatal(err)
	}
	idleDone := make(chan error, 1)
	go func() {
		for {
			if _, err := reattached.Recv(); err != nil {
				idleDone <- err
				return
			}
		}
	}()
	select {
	case <-idleDone:
	case <-time.After(3 * time.Second):
		t.Fatal("revoked peer user kept idle terminal access")
	}
	if _, err := current.List(ctx); err == nil {
		t.Fatal("revoked peer user opened another request")
	}
	// Restore fencing must reach a running owner via its next directory renewal.
	if _, err := second.RotateConnectionRecovery(ctx, recovery); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(7 * time.Second)
	for g2.Online(machine.ID) {
		if time.Now().After(deadline) {
			t.Fatal("old recovery kept its live tunnel")
		}
		time.Sleep(10 * time.Millisecond)
	}
	if _, err := current.List(ctx); err == nil {
		t.Fatal("old recovery still executes")
	}
}

type peerOwnerChecker struct{ denied atomic.Bool }

func (c *peerOwnerChecker) Check(ctx context.Context, r access.Request) (access.Decision, error) {
	if c.denied.Load() {
		return access.Decision{Allowed: false, Reason: "OWNER_TEST_DENY", ID: wire.ID()}, nil
	}
	return (access.Owner{}).Check(ctx, r)
}

type peerEntryRepository struct {
	authorization.Repository
	stale atomic.Bool
}

func (r *peerEntryRepository) CheckAccess(ctx context.Context, c authorization.ConnectionAccess, now int64) (bool, error) {
	if r.stale.Load() {
		return true, nil
	}
	return r.Repository.CheckAccess(ctx, c, now)
}
