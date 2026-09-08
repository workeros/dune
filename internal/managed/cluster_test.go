package managed

import (
	"context"
	"errors"
	"os"
	"sync"
	"testing"
	"time"

	"github.com/aiomni/dune/internal/identity"
	"github.com/aiomni/dune/internal/lifecycle"
	"github.com/aiomni/dune/internal/metadata"
	"github.com/aiomni/dune/internal/wire"
	"github.com/aiomni/dune/pkg/fabric"
	"github.com/aiomni/dune/pkg/storage"
	"github.com/jackc/pgx/v5"
)

type lateCreateProvider struct {
	started chan fabric.CreateCall
	release chan struct{}

	mu         sync.Mutex
	creates    []fabric.CreateCall
	reconciles []fabric.ReconcileCall
}

func (p *lateCreateProvider) Create(_ context.Context, call fabric.CreateCall) (fabric.Observation, error) {
	p.mu.Lock()
	p.creates = append(p.creates, call)
	p.mu.Unlock()
	p.started <- call
	<-p.release
	return fabric.Observation{Outcome: fabric.OutcomeSucceeded, ResourceRef: "late-resource"}, nil
}

func (p *lateCreateProvider) ReconcileCreate(_ context.Context, call fabric.ReconcileCall) (fabric.Observation, error) {
	p.mu.Lock()
	p.reconciles = append(p.reconciles, call)
	p.mu.Unlock()
	return fabric.Observation{Outcome: fabric.OutcomeSucceeded, ResourceRef: "confirmed-resource"}, nil
}

func postgresManagedStores(t *testing.T, ctx context.Context) (*metadata.Store, *metadata.Store) {
	t.Helper()
	address := os.Getenv("DUNE_TEST_POSTGRES")
	if address == "" {
		t.Skip("DUNE_TEST_POSTGRES not configured")
	}
	admin, err := pgx.Connect(ctx, address)
	if err != nil {
		t.Fatal(err)
	}
	name := "dune_managed_" + wire.ID()
	quoted := pgx.Identifier{name}.Sanitize()
	if _, err := admin.Exec(ctx, "CREATE SCHEMA "+quoted); err != nil {
		admin.Close(ctx)
		t.Fatal(err)
	}
	config := storage.Config{Postgres: &storage.Postgres{URL: address, BeforeConnect: func(_ context.Context, c *pgx.ConnConfig) error {
		c.RuntimeParams["search_path"] = name
		return nil
	}}}
	first, err := metadata.Open(ctx, config)
	if err != nil {
		admin.Exec(ctx, "DROP SCHEMA "+quoted+" CASCADE")
		admin.Close(ctx)
		t.Fatal(err)
	}
	second, err := metadata.Open(ctx, config)
	if err != nil {
		first.Close()
		admin.Exec(ctx, "DROP SCHEMA "+quoted+" CASCADE")
		admin.Close(ctx)
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := second.Close(); err != nil {
			t.Error(err)
		}
		if err := first.Close(); err != nil {
			t.Error(err)
		}
		cleanupCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		if _, err := admin.Exec(cleanupCtx, "DROP SCHEMA "+quoted+" CASCADE"); err != nil {
			t.Error(err)
		}
		admin.Close(cleanupCtx)
	})
	return first, second
}

func TestPostgresWorkerTakeoverRejectsLateCreateResult(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	first, second := postgresManagedStores(t, ctx)
	local := identity.NewLocal(first, true)
	user, cookie, err := local.Register(ctx, "managed-takeover@example.test", "managed-takeover-password")
	if err != nil {
		t.Fatal(err)
	}
	created, err := first.CreateManaged(ctx, user, credentialDigest(cookie), wire.ID(), createRequest("visible"))
	if err != nil {
		t.Fatal(err)
	}
	original, err := first.ClaimOperation(ctx, created.Operation.ID, wire.ID(), time.Second)
	if err != nil {
		t.Fatal(err)
	}
	provider := &lateCreateProvider{started: make(chan fabric.CreateCall, 1), release: make(chan struct{})}
	firstExecutor, err := NewExecutor(first, map[string]fabric.CreateProvider{"sandbox": provider})
	if err != nil {
		t.Fatal(err)
	}
	secondExecutor, err := NewExecutor(second, map[string]fabric.CreateProvider{"sandbox": provider})
	if err != nil {
		t.Fatal(err)
	}
	late := make(chan error, 1)
	go func() { late <- firstExecutor.ExecuteCreate(ctx, original) }()

	var dispatched fabric.CreateCall
	select {
	case dispatched = <-provider.started:
	case <-ctx.Done():
		t.Fatal("first worker did not dispatch create")
	}
	time.Sleep(1100 * time.Millisecond)
	takeover, err := second.ClaimOperation(ctx, original.ID, wire.ID(), time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	if takeover.Worker == original.Worker || takeover.Revision <= original.Revision {
		t.Fatal("second pool did not take over the expired execution", takeover.Lease)
	}
	if err := secondExecutor.ExecuteCreate(ctx, takeover); err != nil {
		t.Fatal(err)
	}
	close(provider.release)
	select {
	case err := <-late:
		if !errors.Is(err, lifecycle.ErrLeaseLost) {
			t.Fatal("late worker result was not fenced", err)
		}
	case <-ctx.Done():
		t.Fatal("late provider call did not return")
	}

	provider.mu.Lock()
	creates := append([]fabric.CreateCall(nil), provider.creates...)
	reconciles := append([]fabric.ReconcileCall(nil), provider.reconciles...)
	provider.mu.Unlock()
	if len(creates) != 1 || len(reconciles) != 1 {
		t.Fatal("takeover redispatched create or skipped reconciliation", len(creates), len(reconciles))
	}
	if reconciles[0].Action.ID != dispatched.Action.ID || reconciles[0].Action.Issuer != original.Worker || reconciles[0].Action.ExecutionRevision != original.Revision {
		t.Fatal("takeover changed the original provider action identity", dispatched.Action, reconciles[0].Action)
	}
	action, err := second.ProviderAction(ctx, original.ID, "create")
	if err != nil || action.Outcome != "succeeded" || action.CompletedAt.IsZero() {
		t.Fatal("reconciled action facts were not retained", action, err)
	}
	resource, err := second.ManagedResource(ctx, original.RunnerID)
	if err != nil || resource.Ref != "confirmed-resource" || resource.Gone {
		t.Fatal("late result replaced the reconciled resource", resource, err)
	}
}
