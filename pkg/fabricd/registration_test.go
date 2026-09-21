package fabricd

import (
	"context"
	"maps"
	"net"
	"path/filepath"
	"slices"
	"sync"
	"testing"
	"time"

	"github.com/aiomni/dune/internal/metadata"
	"github.com/aiomni/dune/pkg/access"
	"github.com/aiomni/dune/pkg/client"
	"github.com/aiomni/dune/pkg/gateway"
)

func TestPostgresConnectorRegistration(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	store, err := metadata.Open(ctx, ownershipPostgresConfig(t, ctx), metadata.OpenOptions{})
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	_, token, _, err := store.IssueEnrollment(ctx, "registration-owner", "connector registration")
	if err != nil {
		t.Fatal(err)
	}
	machine, _, err := store.Enroll(ctx, token, "linux", "amd64")
	if err != nil {
		t.Fatal(err)
	}
	directory, err := store.ConnectionDirectory(ctx)
	if err != nil {
		t.Fatal(err)
	}
	g, err := gateway.NewWithDirectory(directory, "https://owner.test/api/v1/ws/peer")
	if err != nil {
		t.Fatal(err)
	}
	defer g.Close()
	engine, err := Open(ctx, filepath.Join(t.TempDir(), "fabricd"))
	if err != nil {
		t.Fatal(err)
	}
	var workers sync.WaitGroup
	defer func() { cancel(); g.Close(); workers.Wait(); engine.Close() }()
	connect := func(role string) net.Conn {
		t.Helper()
		binding, handler, err := (access.Grant{Target: machine.ID, Role: role}).Bind()
		if err != nil {
			t.Fatal(err)
		}
		left, right := net.Pipe()
		workers.Go(func() { _ = g.ServeConn(ctx, left, binding, handler) })
		return right
	}
	daemon := connect(gateway.RoleDaemon)
	done := make(chan error, 1)
	workers.Go(func() { done <- engine.ServeConn(ctx, daemon, machine.ID) })
	for !g.Online(machine.ID) {
		select {
		case err := <-done:
			t.Fatal("connector registration failed", err)
		case <-ctx.Done():
			t.Fatal("connector did not register", ctx.Err())
		case <-time.After(time.Millisecond):
		}
	}
	connected, err := client.Connect(ctx, connect(gateway.RoleSDK), machine.ID)
	if err != nil {
		t.Fatal(err)
	}
	defer connected.Close()
	if _, err := connected.List(ctx); err != nil {
		t.Fatal("registered connector cannot serve a request", err)
	}
	binding := connected.Binding
	if !slices.Contains(binding.Capabilities, "acp.persistent") || binding.Limits["submission_ordinary_keys"] == 0 {
		t.Fatal("registration did not use the persistent ACP connector binding", binding)
	}
	if len(binding.Limits) > 64 {
		t.Fatalf("connector advertises %d limits, exceeding the registration capacity", len(binding.Limits))
	}
	stored, err := directory.Resolve(ctx, machine.ID)
	if err != nil || !stored.Published || stored.Binding.Incarnation != binding.Incarnation || !maps.Equal(stored.Binding.Limits, binding.Limits) {
		t.Fatal("cluster directory did not retain the accepted connector binding", stored, err)
	}
	t.Logf("registered persistent ACP connector with %d limits and served runtime.list", len(binding.Limits))
}
