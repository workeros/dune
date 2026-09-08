package metadata

import (
	"context"
	"fmt"
	"os"
	"sort"
	"sync"
	"testing"
	"time"

	"github.com/aiomni/dune/internal/identity"
	"github.com/aiomni/dune/internal/lifecycle"
	"github.com/aiomni/dune/internal/wire"
	"github.com/aiomni/dune/pkg/api"
	"github.com/aiomni/dune/pkg/gateway"
)

const (
	baselineMachines = 128
	baselineWorkers  = 16
)

type baselineResult struct {
	total        time.Duration
	measurements []time.Duration
}

func measureBaseline(ctx context.Context, operations, workers int, operation func(int) error) (baselineResult, error) {
	jobs := make(chan int)
	measurements := make([]time.Duration, operations)
	errCh := make(chan error, 1)
	var wg sync.WaitGroup
	started := time.Now()
	for range workers {
		wg.Go(func() {
			for index := range jobs {
				begin := time.Now()
				err := operation(index)
				measurements[index] = time.Since(begin)
				if err != nil {
					select {
					case errCh <- err:
					default:
					}
				}
			}
		})
	}
	for index := range operations {
		select {
		case jobs <- index:
		case <-ctx.Done():
			close(jobs)
			wg.Wait()
			return baselineResult{}, ctx.Err()
		}
	}
	close(jobs)
	wg.Wait()
	result := baselineResult{total: time.Since(started), measurements: measurements}
	select {
	case err := <-errCh:
		return result, err
	default:
		return result, nil
	}
}

func baselinePercentile(values []time.Duration, percentile int) time.Duration {
	ordered := append([]time.Duration(nil), values...)
	sort.Slice(ordered, func(i, j int) bool { return ordered[i] < ordered[j] })
	index := (len(ordered)*percentile + 99) / 100
	if index < 1 {
		index = 1
	}
	return ordered[index-1]
}

func reportBaseline(t *testing.T, name string, result baselineResult) {
	t.Helper()
	rate := float64(len(result.measurements)) / result.total.Seconds()
	t.Logf("BASELINE %s operations=%d workers=%d total=%s rate=%.1f/s p50=%s p95=%s p99=%s",
		name, len(result.measurements), baselineWorkers, result.total.Round(time.Microsecond), rate,
		baselinePercentile(result.measurements, 50).Round(time.Microsecond),
		baselinePercentile(result.measurements, 95).Round(time.Microsecond),
		baselinePercentile(result.measurements, 99).Round(time.Microsecond))
}

// TestPostgresEnterprisePerformanceBaseline is an opt-in, target-free baseline.
// It reports observed capacity and latency without imposing a product SLO. The
// fixed cardinality and concurrency make results comparable across releases.
func TestPostgresEnterprisePerformanceBaseline(t *testing.T) {
	if os.Getenv("DUNE_PERFORMANCE_BASELINE") != "1" {
		t.Skip("set DUNE_PERFORMANCE_BASELINE=1 with a dedicated DUNE_TEST_POSTGRES")
	}
	config, _, _ := postgresConfig(t)
	ctx, cancel := context.WithTimeout(context.Background(), 4*time.Minute)
	defer cancel()
	store, err := Open(ctx, config)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	recovery := wire.ID()
	directory, err := store.ConnectionDirectory(ctx, recovery)
	if err != nil {
		t.Fatal(err)
	}
	routes := make([]gateway.Route, 0, baselineMachines)
	schedules := make([]lifecycle.RenewalSchedule, 0, baselineMachines)

	for account := range baselineMachines / 32 {
		local := identity.NewLocal(store, true)
		user, cookie, err := local.Register(ctx, fmt.Sprintf("baseline-%d@example.test", account), "baseline-performance-password")
		if err != nil {
			t.Fatal(err)
		}
		for item := range 32 {
			index := account*32 + item
			created, err := store.CreateManaged(ctx, user, tokenHash(cookie), wire.ID(), managedSpec())
			if err != nil {
				t.Fatal(err)
			}
			claimed, err := store.ClaimOperation(ctx, created.Operation.ID, wire.ID(), time.Minute)
			if err != nil {
				t.Fatal(err)
			}
			action, dispatch, err := store.BeginProviderAction(ctx, claimed, lifecycle.ActionRequest{Kind: "create", Digest: claimed.Digest})
			if err != nil || !dispatch {
				t.Fatal("create reservation", dispatch, err)
			}
			resourceRef := fmt.Sprintf("baseline-resource-%03d", index)
			if err := store.RecordProviderAction(ctx, claimed, action.ID, lifecycle.ActionObservation{Outcome: "succeeded", ResourceRef: resourceRef, ExpiresAt: time.Now().Add(time.Hour)}); err != nil {
				t.Fatal(err)
			}
			if err := store.YieldOperationLease(ctx, claimed); err != nil {
				t.Fatal(err)
			}
			claimed, err = store.ClaimOperation(ctx, created.Operation.ID, wire.ID(), time.Minute)
			if err != nil {
				t.Fatal(err)
			}
			grant, dispatch, err := store.BeginManagedBootstrap(ctx, claimed, tokenHash(fmt.Sprintf("baseline-bootstrap-%03d", index)), time.Minute)
			if err != nil || !dispatch {
				t.Fatal("bootstrap reservation", dispatch, err)
			}
			machine, _, err := store.Enroll(ctx, grant.Token, "linux", "amd64")
			if err != nil {
				t.Fatal(err)
			}
			if err := store.RecordProviderAction(ctx, claimed, grant.Action.ID, lifecycle.ActionObservation{Outcome: "succeeded", ResourceRef: resourceRef}); err != nil {
				t.Fatal(err)
			}
			binding := api.Binding{Version: api.Version, Target: machine.ID, Incarnation: wire.ID(), Generation: 1, Capabilities: []string{"runtime.list"}, Limits: map[string]int{"streams": 8}}
			claim := gateway.RouteClaim{Target: machine.ID, RecoveryGeneration: recovery, OwnerBootID: wire.ID(), OwnerAddress: "https://baseline-owner.test/peer", Binding: binding}
			lease, err := directory.Acquire(ctx, claim, 0)
			if err != nil {
				t.Fatal(err)
			}
			if err := directory.Publish(ctx, lease.Route); err != nil {
				t.Fatal(err)
			}
			binding.RouteRecovery, binding.RouteEpoch = recovery, lease.Epoch
			if err := store.ConfirmMachineOnline(ctx, binding); err != nil {
				t.Fatal(err)
			}
			inspection, err := store.ClaimManagedInspection(ctx, created.Runner.ID, "personal-v1", wire.ID(), time.Minute)
			if err != nil {
				t.Fatal(err)
			}
			schedule, err := store.RecordManagedInspection(ctx, inspection, "personal-v1", lifecycle.DefaultRenewalConfig(), lifecycle.ResourceInspection{Status: lifecycle.InspectionConfirmed, ResourceRef: resourceRef, ExpiresAt: time.Now().Add(5 * time.Minute)})
			if err != nil || schedule.RenewUntil.IsZero() {
				t.Fatal("renewal schedule", schedule, err)
			}
			schedules = append(schedules, schedule)
			if err := directory.Release(ctx, lease.Route); err != nil {
				t.Fatal(err)
			}
			routes = append(routes, lease.Route)
		}
	}

	for index := range routes {
		claim := routes[index].RouteClaim
		lease, err := directory.Acquire(ctx, claim, routes[index].Epoch)
		if err != nil {
			t.Fatal(err)
		}
		if err := directory.Publish(ctx, lease.Route); err != nil {
			t.Fatal(err)
		}
		routes[index] = lease.Route
	}

	resolve, err := measureBaseline(ctx, 4096, baselineWorkers, func(index int) error {
		_, err := directory.Resolve(ctx, routes[index%len(routes)].Target)
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
	reportBaseline(t, "directory-resolve-128-routes", resolve)

	renew, err := measureBaseline(ctx, 1024, baselineWorkers, func(index int) error {
		_, err := directory.Renew(ctx, routes[index%len(routes)])
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
	reportBaseline(t, "directory-renew-128-routes", renew)

	scan, err := measureBaseline(ctx, 512, baselineWorkers, func(int) error {
		found, err := store.RecoverableManagedRenewalSchedulesFor(ctx, []string{"sandbox"}, "personal-v1", 32)
		if err == nil && len(found) != 32 {
			return fmt.Errorf("renewal scan returned %d schedules", len(found))
		}
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
	reportBaseline(t, "managed-renewal-scan-128-due", scan)

	claim, err := measureBaseline(ctx, len(schedules), baselineWorkers, func(index int) error {
		operation, consumed, err := store.ClaimScheduledManagedRenewal(ctx, schedules[index], "personal-v1", wire.ID(), time.Minute)
		if err == nil && (!consumed || operation.ID == "") {
			return fmt.Errorf("renewal schedule %d was not consumed", index)
		}
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
	reportBaseline(t, "managed-renewal-claim-128-due", claim)
}
