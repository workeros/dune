package metadata

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/aiomni/dune/internal/authorization"
	"github.com/aiomni/dune/internal/identity"
	"github.com/aiomni/dune/internal/lifecycle"
	"github.com/aiomni/dune/internal/wire"
	"github.com/aiomni/dune/pkg/access"
)

func confirmedManagedCreate(t *testing.T, s *Store, user identity.User, sessionHash string) (lifecycle.Creation, lifecycle.Operation) {
	t.Helper()
	ctx := context.Background()
	created, err := s.CreateManaged(ctx, user, sessionHash, wire.ID(), managedSpec())
	if err != nil {
		t.Fatal(err)
	}
	claimed, err := s.ClaimOperation(ctx, created.Operation.ID, wire.ID(), time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	action, dispatch, err := s.BeginProviderAction(ctx, claimed, lifecycle.ActionRequest{Kind: "create", Digest: claimed.Digest})
	if err != nil || !dispatch {
		t.Fatal("create action was not reserved", dispatch, err)
	}
	if err := s.RecordProviderAction(ctx, claimed, action.ID, lifecycle.ActionObservation{Outcome: "succeeded", ResourceRef: "managed-bootstrap-resource", ExpiresAt: time.Now().Add(time.Hour)}); err != nil {
		t.Fatal(err)
	}
	if err := s.YieldOperationLease(ctx, claimed); err != nil {
		t.Fatal(err)
	}
	claimed, err = s.ClaimOperation(ctx, created.Operation.ID, wire.ID(), time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	return created, claimed
}

func TestManagedBootstrapEnrollmentBindsConfirmedResourceOnce(t *testing.T) {
	for _, backend := range []string{"sqlite", "postgres"} {
		t.Run(backend, func(t *testing.T) {
			ctx := context.Background()
			s, _, user, sessionHash := managedFixture(t, backend)
			created, claimed := confirmedManagedCreate(t, s, user, sessionHash)
			digest := tokenHash("fixed bootstrap inputs")
			grant, dispatch, err := s.BeginManagedBootstrap(ctx, claimed, digest, 10*time.Minute)
			if err != nil || !dispatch || len(grant.Token) != 64 || grant.Action.Kind != "bootstrap" || grant.Action.Digest != digest || grant.Action.ResourceRef != "managed-bootstrap-resource" || grant.ExpiresAt.IsZero() {
				t.Fatal("invalid bootstrap grant", grant.Action, dispatch, err)
			}
			var storedHash string
			if err := s.db.QueryRowContext(ctx, `SELECT hash FROM dune_managed_enrollments WHERE action_id=$1`, grant.Action.ID).Scan(&storedHash); err != nil || storedHash != tokenHash(grant.Token) || storedHash == grant.Token {
				t.Fatal("bootstrap token was not stored as a one-way verifier", err)
			}
			if got, kind, err := s.EnrollmentIdentity(ctx, grant.Token); err != nil || got != user || kind != "managed" {
				t.Fatal("managed enrollment identity was not scoped", got, kind, err)
			}
			policy := discoveryCheck(func(_ context.Context, request access.Request) (access.Decision, error) {
				if request.Operation != "runner.create" || request.Suboperation != "managed" || request.Scope.PrincipalID != user.ID {
					t.Error("managed enrollment used the wrong access scope", request)
				}
				return discoveryDecision(request, true), nil
			})
			service := authorization.New(ctx, identity.NewLocal(s, true), s, policy)
			if _, err := service.EnrollmentDecision(ctx, grant.Token); err != nil {
				t.Fatal("managed enrollment policy was not evaluated", err)
			}
			again, redispatch, err := s.BeginManagedBootstrap(ctx, claimed, digest, time.Minute)
			if err != nil || redispatch || again.Action != grant.Action || again.Token != "" || !again.ExpiresAt.IsZero() {
				t.Fatal("bootstrap retry issued new dispatch material", again, redispatch, err)
			}
			if _, _, err := s.BeginManagedBootstrap(ctx, claimed, strings.Repeat("c", 64), time.Minute); !errors.Is(err, lifecycle.ErrIntentConflict) {
				t.Fatal("bootstrap action accepted changed controlled inputs", err)
			}

			var successes int
			var machine Machine
			var credential string
			var mu sync.Mutex
			var wg sync.WaitGroup
			for range 2 {
				wg.Add(1)
				go func() {
					defer wg.Done()
					got, secret, enrollErr := s.Enroll(ctx, grant.Token, "linux", "amd64")
					if enrollErr == nil {
						mu.Lock()
						successes++
						machine, credential = got, secret
						mu.Unlock()
					} else if !errors.Is(enrollErr, identity.ErrUnauthorized) {
						t.Error(enrollErr)
					}
				}()
			}
			wg.Wait()
			if successes != 1 || machine.RunnerID != created.Runner.ID || machine.ID == "" || credential == "" {
				t.Fatal("managed enrollment did not bind exactly one machine", successes, machine)
			}
			if id, err := s.MachineCredential(ctx, credential); err != nil || id != machine.ID {
				t.Fatal("managed machine credential was not usable", id, err)
			}
			bound, err := s.Runner(ctx, user.ID, created.Runner.ID)
			if err != nil || bound.Binding == nil || bound.Binding.MachineID != machine.ID || bound.Binding.FabricID != "sandbox" || bound.Binding.Revision != 1 {
				t.Fatal("managed Runner binding changed unexpectedly", bound, err)
			}
			if _, _, err := s.EnrollmentIdentity(ctx, grant.Token); !errors.Is(err, identity.ErrUnauthorized) {
				t.Fatal("consumed managed enrollment remained valid", err)
			}
			if err := s.RecordProviderAction(ctx, claimed, grant.Action.ID, lifecycle.ActionObservation{Outcome: "succeeded", ResourceRef: "managed-bootstrap-resource"}); err != nil {
				t.Fatal(err)
			}
			operation, err := s.Operation(ctx, claimed.ID)
			if err != nil || operation.Exclusive || operation.Finished || operation.Outcome != "" {
				t.Fatal("bootstrap completion did not enter connection wait", operation, err)
			}
		})
	}
}

func TestManagedBootstrapEnrollmentRechecksExpiry(t *testing.T) {
	for _, backend := range []string{"sqlite", "postgres"} {
		t.Run(backend, func(t *testing.T) {
			ctx := context.Background()
			s, _, user, sessionHash := managedFixture(t, backend)
			created, claimed := confirmedManagedCreate(t, s, user, sessionHash)
			grant, dispatch, err := s.BeginManagedBootstrap(ctx, claimed, strings.Repeat("d", 64), time.Minute)
			if err != nil || !dispatch {
				t.Fatal("bootstrap grant setup failed", dispatch, err)
			}
			if _, err := s.db.ExecContext(ctx, `UPDATE dune_managed_enrollments SET expires_at=0 WHERE action_id=$1`, grant.Action.ID); err != nil {
				t.Fatal(err)
			}
			machine, credential, err := s.Enroll(ctx, grant.Token, "linux", "amd64")
			if !errors.Is(err, identity.ErrUnauthorized) || machine.ID != "" || credential != "" {
				t.Fatal("expired managed enrollment created an identity", machine, credential, err)
			}
			bound, err := s.Runner(ctx, user.ID, created.Runner.ID)
			if err != nil || bound.Binding != nil {
				t.Fatal("failed managed enrollment changed the Runner", bound, err)
			}
			var grants int
			if err := s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM dune_managed_enrollments WHERE action_id=$1`, grant.Action.ID).Scan(&grants); err != nil || grants != 1 {
				t.Fatal("failed consumption removed the durable grant", grants, err)
			}
		})
	}
}

func TestManagedBootstrapReservationLossNeverReturnsOrReissuesSecret(t *testing.T) {
	for _, backend := range []string{"sqlite", "postgres"} {
		t.Run(backend, func(t *testing.T) {
			ctx := context.Background()
			s, config, user, sessionHash := managedFixture(t, backend)
			_, claimed := confirmedManagedCreate(t, s, user, sessionHash)
			interrupted, commits := lostCommitStore(t, config)
			digest := strings.Repeat("b", 64)
			grant, dispatch, err := interrupted.BeginManagedBootstrap(ctx, claimed, digest, time.Minute)
			if !errors.Is(err, ErrCommitUnknown) || dispatch || grant.Action.ID != "" || grant.Token != "" || commits.Load() != 1 {
				t.Fatal("uncertain reservation returned dispatch material", grant, dispatch, commits.Load(), err)
			}
			recovered, dispatch, err := s.BeginManagedBootstrap(ctx, claimed, digest, time.Minute)
			if err != nil || dispatch || recovered.Action.ID == "" || recovered.Token != "" || !recovered.ExpiresAt.IsZero() {
				t.Fatal("recovery reissued a bootstrap secret", recovered, dispatch, err)
			}
			var grants int
			if err := s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM dune_managed_enrollments WHERE operation_id=$1`, claimed.ID).Scan(&grants); err != nil || grants != 1 {
				t.Fatal("uncertain reservation did not persist exactly once", grants, err)
			}
		})
	}
}

func TestManagedBootstrapConsumptionLossNeverReturnsOrReusesCredential(t *testing.T) {
	for _, backend := range []string{"sqlite", "postgres"} {
		t.Run(backend, func(t *testing.T) {
			ctx := context.Background()
			s, config, user, sessionHash := managedFixture(t, backend)
			created, claimed := confirmedManagedCreate(t, s, user, sessionHash)
			grant, dispatch, err := s.BeginManagedBootstrap(ctx, claimed, strings.Repeat("e", 64), time.Minute)
			if err != nil || !dispatch {
				t.Fatal("bootstrap grant setup failed", dispatch, err)
			}
			interrupted, commits := lostCommitStore(t, config)
			machine, credential, err := interrupted.Enroll(ctx, grant.Token, "linux", "amd64")
			if !errors.Is(err, ErrCommitUnknown) || machine.ID != "" || credential != "" || commits.Load() != 1 {
				t.Fatal("uncertain enrollment returned a machine credential", machine, credential, commits.Load(), err)
			}
			if machine, credential, err := s.Enroll(ctx, grant.Token, "linux", "amd64"); !errors.Is(err, identity.ErrUnauthorized) || machine.ID != "" || credential != "" {
				t.Fatal("uncertain enrollment token was reusable", machine, credential, err)
			}
			bound, err := s.Runner(ctx, user.ID, created.Runner.ID)
			if err != nil || bound.Binding == nil || bound.Binding.MachineID == "" {
				t.Fatal("committed enrollment could not be reconciled", bound, err)
			}
		})
	}
}
