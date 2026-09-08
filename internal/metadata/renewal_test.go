package metadata

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/aiomni/dune/internal/identity"
	"github.com/aiomni/dune/internal/lifecycle"
	"github.com/aiomni/dune/internal/wire"
	"github.com/aiomni/dune/pkg/renewal"
)

func readyManagedResource(t *testing.T, s *Store, user identity.User, sessionHash string) (lifecycle.Creation, Machine) {
	t.Helper()
	created, claimed, machine := managedMachineAwaitingOnline(t, s, user, sessionHash)
	completeBootstrap(t, s, claimed)
	if err := s.ConfirmMachineOnline(context.Background(), onlineBinding(machine)); err != nil {
		t.Fatal(err)
	}
	return created, machine
}

func TestManagedRenewalDecisionRejectsChangedPolicyInput(t *testing.T) {
	ctx := context.Background()
	s, _, user, sessionHash := managedFixture(t, "sqlite")
	created, _ := readyManagedResource(t, s, user, sessionHash)
	claim, err := s.ClaimManagedInspection(ctx, created.Runner.ID, "enterprise-v1", wire.ID(), time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	expires := time.Now().Add(time.Hour).Truncate(time.Millisecond)
	inspection := lifecycle.ResourceInspection{Status: lifecycle.InspectionConfirmed, ResourceRef: claim.ResourceRef, ExpiresAt: expires}
	input, err := s.ManagedRenewalPolicyInput(ctx, claim, inspection)
	if err != nil {
		t.Fatal(err)
	}
	if input.PrincipalID != user.ID || !input.PrincipalEnabled || input.Namespace != user.Namespace || input.RunnerID != created.Runner.ID || input.FabricID != "sandbox" || input.CreatedAt.IsZero() || !input.State.EverReady {
		t.Fatal("policy input did not preserve the accepted create identity and lifecycle facts", input)
	}
	before, err := s.ManagedResource(ctx, created.Runner.ID)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.SetPrincipalEnabled(ctx, user.ID, false); err != nil {
		t.Fatal(err)
	}
	decision := renewal.Decision{Reason: "ENTERPRISE_WAIT", RecheckAt: input.Now.Add(time.Minute)}
	if _, err := s.RecordManagedRenewalDecision(ctx, claim, "enterprise-v1", input, inspection, decision); !errors.Is(err, lifecycle.ErrLeaseLost) {
		t.Fatal("a policy decision committed after its authoritative input changed", err)
	}
	after, err := s.ManagedResource(ctx, created.Runner.ID)
	if err != nil || !after.ExpiresAt.Equal(before.ExpiresAt) {
		t.Fatal("rejected decision persisted stale provider facts", before, after, err)
	}
}

func TestManagedInspectionPersistsRenewalDecision(t *testing.T) {
	for _, backend := range []string{"sqlite", "postgres"} {
		t.Run(backend, func(t *testing.T) {
			ctx := context.Background()
			s, config, user, sessionHash := managedFixture(t, backend)
			created, _ := readyManagedResource(t, s, user, sessionHash)
			other := s
			if backend == "postgres" {
				var err error
				other, err = Open(ctx, config)
				if err != nil {
					t.Fatal(err)
				}
				defer other.Close()
			}
			candidates, err := s.RecoverableManagedInspectionsFor(ctx, []string{"sandbox"}, "personal-v1", 32)
			if err != nil || len(candidates) != 1 || candidates[0].RunnerID != created.Runner.ID {
				t.Fatal("new resource was not due for inspection", candidates, err)
			}

			var mu sync.Mutex
			var claims []lifecycle.RenewalSchedule
			var wg sync.WaitGroup
			for i := range 6 {
				wg.Go(func() {
					store := s
					if i%2 == 1 {
						store = other
					}
					claim, claimErr := store.ClaimManagedInspection(ctx, created.Runner.ID, "personal-v1", wire.ID(), time.Minute)
					if claimErr == nil {
						mu.Lock()
						claims = append(claims, claim)
						mu.Unlock()
					} else if !errors.Is(claimErr, lifecycle.ErrBusy) && !errors.Is(claimErr, lifecycle.ErrLeaseLost) {
						t.Error(claimErr)
					}
				})
			}
			wg.Wait()
			if len(claims) != 1 {
				t.Fatal("inspection lease was not unique", len(claims))
			}
			claim := claims[0]
			wrong := lifecycle.ResourceInspection{Status: lifecycle.InspectionConfirmed, ResourceRef: "another-resource", ExpiresAt: time.Now().Add(time.Hour)}
			if _, err := s.RecordManagedInspection(ctx, claim, "personal-v1", lifecycle.DefaultRenewalConfig(), wrong); !errors.Is(err, lifecycle.ErrIntentConflict) {
				t.Fatal("inspection rebound the resource", err)
			}
			expires := time.Now().Add(5 * time.Minute).Truncate(time.Millisecond)
			schedule, err := other.RecordManagedInspection(ctx, claim, "personal-v1", lifecycle.DefaultRenewalConfig(), lifecycle.ResourceInspection{Status: lifecycle.InspectionConfirmed, ResourceRef: claim.ResourceRef, ExpiresAt: expires})
			if err != nil || schedule.Reason != "RENEW" || schedule.Facts != lifecycle.InspectionConfirmed || schedule.RenewUntil.IsZero() || !schedule.NextCheckAt.IsZero() || schedule.Worker != "" || !schedule.Until.IsZero() {
				t.Fatal("due resource was not durably scheduled", schedule, err)
			}
			if delta := time.Until(schedule.RenewUntil); delta < 59*time.Minute || delta > 61*time.Minute {
				t.Fatal("renewal target did not use the database decision time", schedule.RenewUntil)
			}
			resource, err := s.ManagedResource(ctx, created.Runner.ID)
			if err != nil || !resource.ExpiresAt.Equal(expires) {
				t.Fatal("confirmed expiry was not saved with the decision", resource, err)
			}
			if candidates, err := s.RecoverableManagedInspectionsFor(ctx, []string{"sandbox"}, "personal-v1", 32); err != nil || len(candidates) != 0 {
				t.Fatal("frozen renewal decision was immediately inspected again", candidates, err)
			}

			candidates, err = s.RecoverableManagedInspectionsFor(ctx, []string{"sandbox"}, "personal-v2", 32)
			if err != nil || len(candidates) != 1 {
				t.Fatal("policy version change did not reschedule resource", candidates, err)
			}
			takeover, err := s.ClaimManagedInspection(ctx, created.Runner.ID, "personal-v2", wire.ID(), time.Minute)
			if err != nil || takeover.Revision != claim.Revision+1 {
				t.Fatal("new policy could not claim inspection", takeover, err)
			}
			if _, err := s.RecordManagedInspection(ctx, claim, "personal-v1", lifecycle.DefaultRenewalConfig(), lifecycle.ResourceInspection{Status: lifecycle.InspectionUnknown}); !errors.Is(err, lifecycle.ErrLeaseLost) {
				t.Fatal("old inspection overwrote a new claim", err)
			}
			schedule, err = s.RecordManagedInspection(ctx, takeover, "personal-v2", lifecycle.DefaultRenewalConfig(), lifecycle.ResourceInspection{Status: lifecycle.InspectionUnknown})
			if err != nil || schedule.Reason != "FACTS_UNKNOWN" || schedule.Facts != lifecycle.InspectionUnknown || schedule.NextCheckAt.IsZero() || !schedule.RenewUntil.IsZero() {
				t.Fatal("unknown facts did not preserve a bounded recheck", schedule, err)
			}
			resource, err = s.ManagedResource(ctx, created.Runner.ID)
			if err != nil || !resource.ExpiresAt.Equal(expires) {
				t.Fatal("unknown inspection replaced confirmed expiry", resource, err)
			}
		})
	}
}

func TestFirstOnlineReactivatesStoppedRenewalSchedule(t *testing.T) {
	for _, backend := range []string{"sqlite", "postgres"} {
		t.Run(backend, func(t *testing.T) {
			ctx := context.Background()
			s, _, user, sessionHash := managedFixture(t, backend)
			created, claimed, machine := managedMachineAwaitingOnline(t, s, user, sessionHash)
			completeBootstrap(t, s, claimed)
			if _, err := s.db.ExecContext(ctx, `UPDATE dune_managed_resources SET confirmed_at=`+s.databaseClock()+`-600000,expires_at=`+s.databaseClock()+`+3600000 WHERE runner_id=$1`, created.Runner.ID); err != nil {
				t.Fatal(err)
			}
			claim, err := s.ClaimManagedInspection(ctx, created.Runner.ID, "personal-v1", wire.ID(), time.Minute)
			if err != nil {
				t.Fatal(err)
			}
			schedule, err := s.RecordManagedInspection(ctx, claim, "personal-v1", lifecycle.DefaultRenewalConfig(), lifecycle.ResourceInspection{Status: lifecycle.InspectionConfirmed, ResourceRef: claim.ResourceRef, ExpiresAt: time.Now().Add(time.Hour)})
			if err != nil || schedule.Reason != "FIRST_CONNECTION_TIMEOUT" || !schedule.NextCheckAt.IsZero() || !schedule.RenewUntil.IsZero() {
				t.Fatal("expired first-connection grace remained scheduled", schedule, err)
			}
			if candidates, err := s.RecoverableManagedInspectionsFor(ctx, []string{"sandbox"}, "personal-v1", 32); err != nil || len(candidates) != 0 {
				t.Fatal("stopped grace decision kept polling", candidates, err)
			}
			if err := s.ConfirmMachineOnline(ctx, onlineBinding(machine)); err != nil {
				t.Fatal(err)
			}
			candidates, err := s.RecoverableManagedInspectionsFor(ctx, []string{"sandbox"}, "personal-v1", 32)
			if err != nil || len(candidates) != 1 {
				t.Fatal("first-online fact did not reactivate policy", candidates, err)
			}
		})
	}
}

func TestConfirmedGoneResourceClosesManagedAccess(t *testing.T) {
	for _, backend := range []string{"sqlite", "postgres"} {
		t.Run(backend, func(t *testing.T) {
			ctx := context.Background()
			s, _, user, sessionHash := managedFixture(t, backend)
			created, machine := readyManagedResource(t, s, user, sessionHash)
			claim, err := s.ClaimManagedInspection(ctx, created.Runner.ID, "personal-v1", wire.ID(), time.Minute)
			if err != nil {
				t.Fatal(err)
			}
			schedule, err := s.RecordManagedInspection(ctx, claim, "personal-v1", lifecycle.DefaultRenewalConfig(), lifecycle.ResourceInspection{Status: lifecycle.InspectionConfirmed, ResourceRef: claim.ResourceRef, Gone: true})
			if err != nil || schedule.Reason != "RESOURCE_GONE" || !schedule.RenewUntil.IsZero() || !schedule.NextCheckAt.IsZero() {
				t.Fatal("confirmed deletion did not stop renewal", schedule, err)
			}
			resource, err := s.ManagedResource(ctx, created.Runner.ID)
			if err != nil || !resource.Gone || !resource.AccessClosed {
				t.Fatal("confirmed deletion retained resource access", resource, err)
			}
			if selected, err := s.Runner(ctx, user.ID, created.Runner.ID); err != nil || selected.Binding != nil {
				t.Fatal("confirmed deletion retained machine binding", selected, err)
			}
			if err := s.ConfirmMachineOnline(ctx, onlineBinding(machine)); !errors.Is(err, identity.ErrUnauthorized) {
				t.Fatal("deleted resource reconnected", err)
			}
		})
	}
}

func TestManagedInspectionCommitLossAndValidation(t *testing.T) {
	for _, backend := range []string{"sqlite", "postgres"} {
		t.Run(backend, func(t *testing.T) {
			ctx := context.Background()
			s, config, user, sessionHash := managedFixture(t, backend)
			created, _ := readyManagedResource(t, s, user, sessionHash)
			claim, err := s.ClaimManagedInspection(ctx, created.Runner.ID, "personal-v1", wire.ID(), time.Minute)
			if err != nil {
				t.Fatal(err)
			}
			interrupted, commits := lostCommitStore(t, config)
			_, err = interrupted.RecordManagedInspection(ctx, claim, "personal-v1", lifecycle.DefaultRenewalConfig(), lifecycle.ResourceInspection{Status: lifecycle.InspectionTimedOut})
			if !errors.Is(err, ErrCommitUnknown) || commits.Load() != 1 {
				t.Fatal("inspection commit uncertainty was hidden or replayed", commits.Load(), err)
			}
			schedule, err := s.ManagedRenewalSchedule(ctx, created.Runner.ID)
			if err != nil || schedule.Facts != lifecycle.InspectionTimedOut || schedule.Reason != "FACTS_UNKNOWN" || schedule.Worker != "" {
				t.Fatal("uncertain acknowledgement did not leave a readable schedule", schedule, err)
			}
			if _, err := s.RecordManagedInspection(ctx, claim, "personal-v1", lifecycle.DefaultRenewalConfig(), lifecycle.ResourceInspection{Status: lifecycle.InspectionUnknown, ResourceRef: claim.ResourceRef}); !errors.Is(err, ErrInvalidArgument) {
				t.Fatal("unknown inspection accepted resource assertions", err)
			}
			if _, err := s.RecoverableManagedInspectionsFor(ctx, nil, "personal-v1", 32); err != nil {
				t.Fatal("empty provider set was not an empty scan", err)
			}
			if _, err := s.RecoverableManagedInspectionsFor(ctx, []string{"sandbox"}, "", 32); !errors.Is(err, ErrInvalidArgument) {
				t.Fatal("empty policy version was accepted", err)
			}
		})
	}
}
