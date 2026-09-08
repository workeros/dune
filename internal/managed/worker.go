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
	Bootstrap    BootstrapConfig
	Renewal      lifecycle.RenewalConfig
	// RenewalPolicyVersion changes whenever policy meaning or configuration
	// changes, so stopped schedules are reconsidered without rewriting history.
	RenewalPolicyVersion string
}

func DefaultWorkerConfig() WorkerConfig {
	return WorkerConfig{PollInterval: time.Second, LeaseTTL: 30 * time.Second, CallTimeout: 20 * time.Second, Renewal: lifecycle.DefaultRenewalConfig(), RenewalPolicyVersion: "personal-v1"}
}

func (c WorkerConfig) validate() error {
	if c.PollInterval <= 0 || c.PollInterval > time.Minute || c.LeaseTTL < time.Second || c.LeaseTTL > time.Minute || c.CallTimeout <= 0 || c.CallTimeout > 10*time.Minute {
		return fmt.Errorf("managed worker requires bounded polling, lease and provider call durations")
	}
	return nil
}

type ProviderSet struct {
	Create    map[string]fabric.CreateProvider
	Bootstrap map[string]fabric.BootstrapProvider
	Inspect   map[string]fabric.InspectProvider
	Renew     map[string]fabric.RenewProvider
}

// Worker recovers accepted create and Bootstrap stages independently of browser
// sessions. One worker executes one provider call at a time. PostgreSQL replicas
// compete through operation leases, while the durable action journal decides
// whether a claimant may dispatch or may only reconcile.
type Worker struct {
	store              *metadata.Store
	create             *Executor
	bootstrap          *BootstrapExecutor
	maintenance        *MaintenanceExecutor
	renewal            *RenewalExecutor
	createProviders    map[string]struct{}
	bootstrapProviders map[string]struct{}
	inspectProviders   map[string]struct{}
	renewProviders     map[string]struct{}
	config             WorkerConfig
	instanceID         string
}

func NewWorker(store *metadata.Store, providers ProviderSet, config WorkerConfig) (*Worker, error) {
	if err := config.validate(); err != nil {
		return nil, err
	}
	if len(providers.Renew) > 0 && !validProviderName(config.RenewalPolicyVersion) {
		return nil, fmt.Errorf("managed renewal worker requires a policy version")
	}
	create, err := NewExecutor(store, providers.Create)
	if err != nil {
		return nil, err
	}
	createConfigured := make(map[string]struct{}, len(create.providers))
	for id := range create.providers {
		createConfigured[id] = struct{}{}
	}
	var bootstrap *BootstrapExecutor
	bootstrapConfigured := make(map[string]struct{}, len(providers.Bootstrap))
	if len(providers.Bootstrap) > 0 {
		bootstrap, err = NewBootstrapExecutor(store, providers.Bootstrap, config.Bootstrap)
		if err != nil {
			return nil, err
		}
		for id := range bootstrap.providers {
			bootstrapConfigured[id] = struct{}{}
		}
	}
	var maintenance *MaintenanceExecutor
	inspectConfigured := make(map[string]struct{}, len(providers.Inspect))
	if len(providers.Inspect) > 0 {
		maintenance, err = NewMaintenanceExecutor(store, providers.Inspect, config.Renewal, config.RenewalPolicyVersion)
		if err != nil {
			return nil, err
		}
		for id := range maintenance.providers {
			inspectConfigured[id] = struct{}{}
		}
	}
	renewal, err := NewRenewalExecutor(store, providers.Renew)
	if err != nil {
		return nil, err
	}
	renewConfigured := make(map[string]struct{}, len(renewal.providers))
	for id := range renewal.providers {
		renewConfigured[id] = struct{}{}
	}
	return &Worker{
		store: store, create: create, bootstrap: bootstrap, maintenance: maintenance, renewal: renewal,
		createProviders: createConfigured, bootstrapProviders: bootstrapConfigured, inspectProviders: inspectConfigured, renewProviders: renewConfigured,
		config: config, instanceID: wire.ID(),
	}, nil
}

func configuredFabrics(providers map[string]struct{}) []string {
	ids := make([]string, 0, len(providers))
	for id := range providers {
		ids = append(ids, id)
	}
	return ids
}

// RunOnce claims and advances at most one eligible lifecycle stage. Inspection
// is checked first so expiry and confirmed deletion facts are not starved by new
// creates. The bool reports whether this worker obtained a claim. Unsupported
// Fabric namespaces remain untouched so another correctly configured deployment
// can recover them.
func (w *Worker) RunOnce(ctx context.Context) (bool, error) {
	if w.maintenance != nil {
		candidates, err := w.store.RecoverableManagedInspectionsFor(ctx, configuredFabrics(w.inspectProviders), w.config.RenewalPolicyVersion, managedCreateBatch)
		if err != nil {
			return false, err
		}
		for _, candidate := range candidates {
			claimed, err := w.store.ClaimManagedInspection(ctx, candidate.RunnerID, w.config.RenewalPolicyVersion, w.instanceID, w.config.LeaseTTL)
			if errors.Is(err, lifecycle.ErrBusy) || errors.Is(err, lifecycle.ErrLeaseLost) {
				continue
			}
			if err != nil {
				return false, err
			}
			return true, w.executeInspection(ctx, claimed)
		}
	}
	if len(w.renewProviders) > 0 {
		candidates, err := w.store.RecoverableManagedRenewalsFor(ctx, configuredFabrics(w.renewProviders), managedCreateBatch)
		if err != nil {
			return false, err
		}
		for _, candidate := range candidates {
			claimed, err := w.store.ClaimRecoverableManagedRenewal(ctx, candidate.ID, w.instanceID, w.config.LeaseTTL)
			if errors.Is(err, lifecycle.ErrBusy) || errors.Is(err, lifecycle.ErrLeaseLost) {
				continue
			}
			if err != nil {
				return false, err
			}
			return true, w.execute(ctx, claimed, "renew", w.renewal.Execute)
		}
		schedules, err := w.store.RecoverableManagedRenewalSchedulesFor(ctx, configuredFabrics(w.renewProviders), w.config.RenewalPolicyVersion, managedCreateBatch)
		if err != nil {
			return false, err
		}
		for _, schedule := range schedules {
			claimed, consumed, err := w.store.ClaimScheduledManagedRenewal(ctx, schedule, w.config.RenewalPolicyVersion, w.instanceID, w.config.LeaseTTL)
			if errors.Is(err, lifecycle.ErrBusy) || errors.Is(err, lifecycle.ErrLeaseLost) {
				continue
			}
			if err != nil {
				return false, err
			}
			if !consumed || claimed.ID == "" {
				return consumed, nil
			}
			return true, w.execute(ctx, claimed, "renew", w.renewal.Execute)
		}
	}
	if w.bootstrap != nil {
		candidates, err := w.store.RecoverableManagedBootstrapsFor(ctx, configuredFabrics(w.bootstrapProviders), managedCreateBatch)
		if err != nil {
			return false, err
		}
		for _, candidate := range candidates {
			if _, ok := w.bootstrapProviders[candidate.FabricID]; !ok {
				continue
			}
			claimed, err := w.store.ClaimRecoverableManagedBootstrap(ctx, candidate.ID, w.instanceID, w.config.LeaseTTL)
			if errors.Is(err, lifecycle.ErrBusy) || errors.Is(err, lifecycle.ErrLeaseLost) {
				continue
			}
			if err != nil {
				return false, err
			}
			return true, w.execute(ctx, claimed, "bootstrap", w.bootstrap.execute)
		}
	}
	candidates, err := w.store.RecoverableManagedCreatesFor(ctx, configuredFabrics(w.createProviders), managedCreateBatch)
	if err != nil {
		return false, err
	}
	for _, candidate := range candidates {
		if _, ok := w.createProviders[candidate.FabricID]; !ok {
			continue
		}
		claimed, err := w.store.ClaimRecoverableManagedCreate(ctx, candidate.ID, w.instanceID, w.config.LeaseTTL)
		if errors.Is(err, lifecycle.ErrBusy) || errors.Is(err, lifecycle.ErrLeaseLost) {
			continue
		}
		if err != nil {
			return false, err
		}
		return true, w.execute(ctx, claimed, "create", w.create.executeCreate)
	}
	return false, nil
}

func (w *Worker) executeInspection(ctx context.Context, claimed lifecycle.RenewalSchedule) error {
	callCtx, cancel := context.WithTimeout(ctx, w.config.CallTimeout)
	defer cancel()
	completed := make(chan error, 1)
	go func() { completed <- w.maintenance.Execute(ctx, callCtx, claimed) }()
	ticker := time.NewTicker(w.config.LeaseTTL / 3)
	defer ticker.Stop()
	for {
		select {
		case err := <-completed:
			return err
		case <-ticker.C:
			if _, err := w.store.RenewManagedInspectionLease(ctx, claimed, w.config.LeaseTTL); err != nil {
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

func (w *Worker) execute(ctx context.Context, claimed lifecycle.Operation, kind string, execute func(context.Context, context.Context, lifecycle.Operation) error) error {
	callCtx, cancel := context.WithTimeout(ctx, w.config.CallTimeout)
	defer cancel()
	completed := make(chan error, 1)
	go func() { completed <- execute(ctx, callCtx, claimed) }()

	ticker := time.NewTicker(w.config.LeaseTTL / 3)
	defer ticker.Stop()
	for {
		select {
		case err := <-completed:
			if err != nil {
				return err
			}
			action, err := w.store.ProviderAction(ctx, claimed.ID, kind)
			if errors.Is(err, metadata.ErrNotFound) {
				operation, operationErr := w.store.Operation(ctx, claimed.ID)
				if operationErr == nil && operation.Finished {
					return nil
				}
				return errors.Join(err, operationErr)
			}
			if err != nil {
				return err
			}
			if action.CompletedAt.IsZero() {
				// Keep the current lease as the durable minimum reconciliation
				// interval. Expiry permits takeover and another observation; it
				// never permits the mutation to be dispatched again.
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
