package managed

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/aiomni/dune/internal/lifecycle"
	"github.com/aiomni/dune/internal/metadata"
	"github.com/aiomni/dune/internal/wire"
	"github.com/aiomni/dune/pkg/fabric"
	"github.com/aiomni/dune/pkg/renewal"
)

const managedCreateBatch = 32

const managedHistoryCleanupInterval = time.Hour

type WorkerConfig struct {
	PollInterval  time.Duration
	LeaseTTL      time.Duration
	CallTimeout   time.Duration
	Bootstrap     BootstrapConfig
	RenewalPolicy renewal.Policy
	// RenewalPolicyVersion changes whenever policy meaning or configuration
	// changes, so stopped schedules are reconsidered without rewriting history.
	RenewalPolicyVersion string
	// HistoryRetention bounds the recovery and idempotency window for terminal
	// lifecycle records. Cleanup uses the shared database clock.
	HistoryRetention time.Duration
	// InstanceID and CloseTarget enable durable access-close fanout. They must
	// be configured together by a host and are unrelated to provider execution.
	InstanceID  string
	CloseTarget func(string) <-chan struct{}
}

func DefaultWorkerConfig() WorkerConfig {
	config := renewal.DefaultConfig()
	return WorkerConfig{PollInterval: time.Second, LeaseTTL: 30 * time.Second, CallTimeout: 20 * time.Second, RenewalPolicy: renewal.Personal{Config: config}, RenewalPolicyVersion: "personal-v1", HistoryRetention: 30 * 24 * time.Hour}
}

func (c WorkerConfig) validate() error {
	if c.PollInterval <= 0 || c.PollInterval > time.Minute || c.LeaseTTL < time.Second || c.LeaseTTL > time.Minute || c.CallTimeout <= 0 || c.CallTimeout > 10*time.Minute {
		return fmt.Errorf("managed worker requires bounded polling, lease and provider call durations")
	}
	if c.HistoryRetention < 24*time.Hour || c.HistoryRetention > 366*24*time.Hour {
		return fmt.Errorf("managed worker history retention must be between 24 hours and 366 days")
	}
	if (c.InstanceID == "") != (c.CloseTarget == nil) || (c.InstanceID != "" && !wire.ValidID(c.InstanceID)) {
		return fmt.Errorf("managed worker access closer requires an instance ID and target closer")
	}
	return nil
}

type ProviderSet struct {
	Create    map[string]fabric.CreateProvider
	Bootstrap map[string]fabric.BootstrapProvider
	Inspect   map[string]fabric.InspectProvider
	Renew     map[string]fabric.RenewProvider
	Destroy   map[string]fabric.DestroyProvider
	Candidate map[string]fabric.CandidateProvider
}

// Worker recovers accepted lifecycle stages independently of browser sessions.
// One worker executes one provider call at a time. PostgreSQL replicas compete
// through operation leases, while the durable action journal decides whether a
// claimant may dispatch or may only reconcile.
type Worker struct {
	store              *metadata.Store
	create             *Executor
	bootstrap          *BootstrapExecutor
	maintenance        *MaintenanceExecutor
	renewal            *RenewalExecutor
	destroyal          *DestroyExecutor
	review             *ReviewExecutor
	createProviders    map[string]struct{}
	bootstrapProviders map[string]struct{}
	inspectProviders   map[string]struct{}
	renewProviders     map[string]struct{}
	destroyProviders   map[string]struct{}
	config             WorkerConfig
	instanceID         string
	cleanupMu          sync.Mutex
	nextCleanup        time.Time
}

func NewWorker(store *metadata.Store, providers ProviderSet, config WorkerConfig) (*Worker, error) {
	if err := config.validate(); err != nil {
		return nil, err
	}
	if len(providers.Inspect) > 0 && config.RenewalPolicy == nil {
		return nil, fmt.Errorf("managed inspection worker requires a renewal policy")
	}
	if (len(providers.Inspect) > 0 || len(providers.Renew) > 0) && !validProviderName(config.RenewalPolicyVersion) {
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
		maintenance, err = NewMaintenanceExecutor(store, providers.Inspect, config.RenewalPolicy, config.RenewalPolicyVersion, config.CallTimeout)
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
	destroyal, err := NewDestroyExecutor(store, providers.Destroy)
	if err != nil {
		return nil, err
	}
	destroyConfigured := make(map[string]struct{}, len(destroyal.providers))
	for id := range destroyal.providers {
		destroyConfigured[id] = struct{}{}
	}
	review, err := NewReviewExecutor(store, providers)
	if err != nil {
		return nil, err
	}
	return &Worker{
		store: store, create: create, bootstrap: bootstrap, maintenance: maintenance, renewal: renewal, destroyal: destroyal, review: review,
		createProviders: createConfigured, bootstrapProviders: bootstrapConfigured, inspectProviders: inspectConfigured, renewProviders: renewConfigured, destroyProviders: destroyConfigured,
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

// RunOnce claims and advances at most one eligible lifecycle stage. Access
// closure, mutations, manual checks and maintenance run before low-priority
// history cleanup. The bool reports whether this worker did work. Unsupported
// Fabric namespaces remain untouched so another configured deployment can
// recover them.
func (w *Worker) RunOnce(ctx context.Context) (bool, error) {
	if w.config.CloseTarget != nil {
		closures, err := w.store.PendingManagedAccessClosures(ctx, w.config.InstanceID, managedCreateBatch)
		if err != nil {
			return false, err
		}
		for _, closure := range closures {
			select {
			case <-w.config.CloseTarget(closure.MachineID):
				if err := w.store.ConfirmManagedDestroyAccessClosed(ctx, closure); err != nil {
					if errors.Is(err, lifecycle.ErrBusy) {
						continue
					}
					return false, err
				}
				return true, nil
			default:
			}
		}
	}
	if len(w.destroyProviders) > 0 {
		candidates, err := w.store.RecoverableManagedDestroysFor(ctx, configuredFabrics(w.destroyProviders), managedCreateBatch)
		if err != nil {
			return false, err
		}
		for _, candidate := range candidates {
			claimed, err := w.store.ClaimRecoverableManagedDestroy(ctx, candidate.ID, w.instanceID, w.config.LeaseTTL)
			if errors.Is(err, lifecycle.ErrBusy) || errors.Is(err, lifecycle.ErrLeaseLost) {
				continue
			}
			if err != nil {
				return false, err
			}
			return true, w.execute(ctx, claimed, "destroy", w.destroyal.Execute)
		}
	}
	if fabrics := w.review.configuredFabrics(); len(fabrics) > 0 {
		reviews, err := w.store.RecoverableManagedReviewsFor(ctx, fabrics, managedCreateBatch)
		if err != nil {
			return false, err
		}
		for _, candidate := range reviews {
			review, operation, err := w.store.ClaimManagedReview(ctx, candidate.ID, w.instanceID, w.config.LeaseTTL)
			if errors.Is(err, lifecycle.ErrBusy) || errors.Is(err, lifecycle.ErrLeaseLost) {
				continue
			}
			if err != nil {
				return false, err
			}
			if !review.CompletedAt.IsZero() {
				return true, nil
			}
			return true, w.review.ExecuteWithLease(ctx, review, operation, w.config.LeaseTTL, w.config.CallTimeout)
		}
	}
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
	if w.historyCleanupDue(time.Now()) {
		cleanup, err := w.store.PruneManagedHistory(ctx, w.config.HistoryRetention, managedCreateBatch)
		if err != nil {
			return false, err
		}
		if cleanup.Operations > 0 || cleanup.Runners > 0 {
			return true, nil
		}
		w.deferHistoryCleanup(time.Now().Add(managedHistoryCleanupInterval))
	}
	return false, nil
}

func (w *Worker) historyCleanupDue(now time.Time) bool {
	w.cleanupMu.Lock()
	defer w.cleanupMu.Unlock()
	return w.nextCleanup.IsZero() || !now.Before(w.nextCleanup)
}

func (w *Worker) deferHistoryCleanup(until time.Time) {
	w.cleanupMu.Lock()
	w.nextCleanup = until
	w.cleanupMu.Unlock()
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
	return w.RunUntil(ctx, nil)
}

// RunUntil stops starting lifecycle iterations after drain is closed. An
// iteration already accepted before that boundary may finish while ctx remains
// valid; cancelling ctx still interrupts it through the normal provider-call
// and lease paths. A nil drain preserves Run's process-lifetime behavior.
func (w *Worker) RunUntil(ctx context.Context, drain <-chan struct{}) error {
	timer := time.NewTimer(0)
	defer timer.Stop()
	for {
		select {
		case <-drain:
			return nil
		default:
		}
		select {
		case <-ctx.Done():
			return nil
		case <-drain:
			return nil
		case <-timer.C:
		}
		// A zero-duration initial timer and an already closed drain are both
		// ready. Give drain another check before accepting an iteration.
		select {
		case <-drain:
			return nil
		default:
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
