package managed

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/aiomni/dune/internal/lifecycle"
	"github.com/aiomni/dune/internal/metadata"
	"github.com/aiomni/dune/internal/wire"
	"github.com/aiomni/dune/pkg/fabric"
)

const managedCreateBatch = 32

type WorkerConfig struct {
	PollInterval time.Duration
	LeaseTTL     time.Duration
	CallTimeout  time.Duration
}

func DefaultWorkerConfig() WorkerConfig {
	return WorkerConfig{PollInterval: time.Second, LeaseTTL: 30 * time.Second, CallTimeout: 20 * time.Second}
}

func (c WorkerConfig) validate() error {
	if c.PollInterval <= 0 || c.PollInterval > time.Minute || c.LeaseTTL < time.Second || c.LeaseTTL > time.Minute || c.CallTimeout <= 0 || c.CallTimeout > 10*time.Minute {
		return fmt.Errorf("managed worker requires bounded polling, lease and provider call durations")
	}
	return nil
}

// Worker recovers accepted create operations independently of browser sessions.
// One worker executes one provider call at a time. PostgreSQL replicas compete
// through operation leases, while the durable action journal decides whether a
// claimant may dispatch or may only reconcile.
type Worker struct {
	store      *metadata.Store
	executor   *Executor
	providers  map[string]struct{}
	config     WorkerConfig
	instanceID string
}

func NewWorker(store *metadata.Store, providers map[string]fabric.CreateProvider, config WorkerConfig) (*Worker, error) {
	if err := config.validate(); err != nil {
		return nil, err
	}
	executor, err := NewExecutor(store, providers)
	if err != nil {
		return nil, err
	}
	configured := make(map[string]struct{}, len(executor.providers))
	for id := range executor.providers {
		configured[id] = struct{}{}
	}
	return &Worker{store: store, executor: executor, providers: configured, config: config, instanceID: wire.ID()}, nil
}

// RunOnce claims and advances at most one eligible create operation. The bool
// reports whether this worker obtained a claim. Unsupported Fabric namespaces
// remain untouched so another correctly configured deployment can recover them.
func (w *Worker) RunOnce(ctx context.Context) (bool, error) {
	candidates, err := w.store.RecoverableManagedCreates(ctx, managedCreateBatch)
	if err != nil {
		return false, err
	}
	for _, candidate := range candidates {
		if _, ok := w.providers[candidate.FabricID]; !ok {
			continue
		}
		claimed, err := w.store.ClaimRecoverableManagedCreate(ctx, candidate.ID, w.instanceID, w.config.LeaseTTL)
		if errors.Is(err, lifecycle.ErrBusy) || errors.Is(err, lifecycle.ErrLeaseLost) {
			continue
		}
		if err != nil {
			return false, err
		}
		return true, w.execute(ctx, claimed)
	}
	return false, nil
}

func (w *Worker) execute(ctx context.Context, claimed lifecycle.Operation) error {
	callCtx, cancel := context.WithTimeout(ctx, w.config.CallTimeout)
	defer cancel()
	completed := make(chan error, 1)
	go func() { completed <- w.executor.executeCreate(ctx, callCtx, claimed) }()

	ticker := time.NewTicker(w.config.LeaseTTL / 3)
	defer ticker.Stop()
	for {
		select {
		case err := <-completed:
			if err != nil {
				return err
			}
			action, err := w.store.ProviderAction(ctx, claimed.ID, "create")
			if err != nil {
				return err
			}
			if action.CompletedAt.IsZero() {
				// Keep the current lease as the durable minimum reconciliation
				// interval. Expiry permits takeover and another observation; it
				// never permits Create to be dispatched again.
				return nil
			}
			return w.store.YieldOperationLease(ctx, claimed)
		case <-ticker.C:
			if _, err := w.store.RenewOperationLease(ctx, claimed, w.config.LeaseTTL); err != nil {
				cancel()
				executionErr := <-completed
				if executionErr == nil {
					return nil
				}
				return errors.Join(err, executionErr)
			}
		case <-ctx.Done():
			cancel()
			<-completed
			return ctx.Err()
		}
	}
}

// Run polls until cancellation. Operational or provider-contract errors stop
// the worker so its owner can make the failure visible and restart from durable
// state; no external mutation is replayed during recovery.
func (w *Worker) Run(ctx context.Context) error {
	timer := time.NewTimer(0)
	defer timer.Stop()
	for {
		select {
		case <-ctx.Done():
			return nil
		case <-timer.C:
		}
		if _, err := w.RunOnce(ctx); err != nil {
			if ctx.Err() != nil {
				return nil
			}
			return err
		}
		timer.Reset(w.config.PollInterval)
	}
}
