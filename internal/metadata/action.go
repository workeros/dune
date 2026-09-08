package metadata

import (
	"context"
	"database/sql"
	"errors"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/aiomni/dune/internal/lifecycle"
	"github.com/aiomni/dune/internal/wire"
)

const actionColumns = "id,operation_id,kind,request_digest,resource_ref,renew_until,worker,execution_revision,started_at,completed_at,outcome"
const resourceColumns = "runner_id,fabric_id,resource_ref,confirmed_at,expires_at,gone,access_closed"

func optionalMillis(t time.Time) int64 {
	if t.IsZero() {
		return 0
	}
	return t.UnixMilli()
}
func optionalTime(millis int64) time.Time {
	if millis == 0 {
		return time.Time{}
	}
	return time.UnixMilli(millis).UTC()
}

func scanAction(row interface{ Scan(...any) error }) (lifecycle.ProviderAction, error) {
	var a lifecycle.ProviderAction
	var until, started, completed int64
	err := row.Scan(&a.ID, &a.OperationID, &a.Kind, &a.Digest, &a.ResourceRef, &until, &a.Worker, &a.ExecutionRevision, &started, &completed, &a.Outcome)
	a.RenewUntil, a.StartedAt, a.CompletedAt = optionalTime(until), optionalTime(started), optionalTime(completed)
	if errors.Is(err, sql.ErrNoRows) {
		err = ErrNotFound
	}
	return a, err
}
func scanManagedResource(row interface{ Scan(...any) error }) (lifecycle.Resource, error) {
	var r lifecycle.Resource
	var confirmed, expires int64
	err := row.Scan(&r.RunnerID, &r.FabricID, &r.Ref, &confirmed, &expires, &r.Gone, &r.AccessClosed)
	r.ConfirmedAt, r.ExpiresAt = optionalTime(confirmed), optionalTime(expires)
	if errors.Is(err, sql.ErrNoRows) {
		err = ErrNotFound
	}
	return r, err
}

// ProviderAction and ManagedResource are trusted recovery reads. Their callers
// authenticate and authorize user-facing discovery independently.
func (s *Store) ProviderAction(ctx context.Context, operationID, kind string) (lifecycle.ProviderAction, error) {
	return scanAction(s.db.QueryRowContext(ctx, "SELECT "+actionColumns+" FROM dune_provider_actions WHERE operation_id=$1 AND kind=$2", operationID, kind))
}
func (s *Store) ManagedResource(ctx context.Context, runnerID string) (lifecycle.Resource, error) {
	return scanManagedResource(s.db.QueryRowContext(ctx, "SELECT "+resourceColumns+" FROM dune_managed_resources WHERE runner_id=$1", runnerID))
}

// BeginProviderAction commits one dispatch reservation. Dispatch is true only
// for its first confirmed commit. A repeated request, another claimant, or an
// unknown commit never grants dispatch; inspect the original action instead.
// The caller must recheck its execution context immediately before provider I/O.
// This journal cannot fence an external system or prove a late call was cancelled.
func (s *Store) BeginProviderAction(ctx context.Context, expected lifecycle.Operation, request lifecycle.ActionRequest) (action lifecycle.ProviderAction, dispatch bool, err error) {
	if !peerHash(request.Digest) || (request.Kind != "create" && request.Kind != "bootstrap" && request.Kind != "renew" && request.Kind != "destroy") || (request.Kind == "renew") != (!request.RenewUntil.IsZero()) || (!request.RenewUntil.IsZero() && request.RenewUntil.UnixMilli() <= 0) {
		return action, false, ErrInvalidArgument
	}
	// Persisted deadlines have millisecond precision, just like execution leases.
	request.RenewUntil = optionalTime(optionalMillis(request.RenewUntil))
	err = s.withOperation(ctx, expected.ID, func(tx *sql.Tx, current lifecycle.Operation, now int64) error {
		if !ownsOperation(current, expected, now) {
			return lifecycle.ErrLeaseLost
		}
		previous, err := scanAction(tx.QueryRowContext(ctx, "SELECT "+actionColumns+" FROM dune_provider_actions WHERE operation_id=$1 AND kind=$2", current.ID, request.Kind))
		if err == nil {
			if previous.ActionRequest != request {
				return lifecycle.ErrIntentConflict
			}
			action = previous
			return nil
		}
		if !errors.Is(err, ErrNotFound) {
			return err
		}
		if !current.Exclusive || current.Outcome != "" {
			return lifecycle.ErrBusy
		}
		if err := s.requireNoPendingAction(ctx, tx, current.ID); err != nil {
			return err
		}
		ref, err := s.actionTarget(ctx, tx, current, request, now)
		if err != nil {
			return err
		}
		action = lifecycle.ProviderAction{ActionRequest: request, ID: wire.ID(), OperationID: current.ID, ResourceRef: ref, Worker: current.Worker, ExecutionRevision: current.Revision, StartedAt: time.UnixMilli(now).UTC()}
		result, err := tx.ExecContext(ctx, `INSERT INTO dune_provider_actions(id,operation_id,kind,request_digest,resource_ref,renew_until,worker,execution_revision,started_at) SELECT $1,id,$2,$3,$4,$5,$6,$7,$8 FROM dune_operations WHERE id=$9 AND worker=$6 AND execution_revision=$7 AND lease_until>`+s.databaseClock(), action.ID, request.Kind, request.Digest, ref, optionalMillis(request.RenewUntil), current.Worker, current.Revision, now, current.ID)
		if err != nil {
			return err
		}
		n, err := result.RowsAffected()
		if err != nil {
			return err
		}
		if n != 1 {
			return lifecycle.ErrLeaseLost
		}
		dispatch = true
		return nil
	})
	if err != nil {
		return lifecycle.ProviderAction{}, false, err
	}
	return action, dispatch, nil
}

func (s *Store) requireNoPendingAction(ctx context.Context, tx *sql.Tx, operationID string) error {
	var id string
	err := tx.QueryRowContext(ctx, `SELECT id FROM dune_provider_actions WHERE operation_id=$1 AND completed_at=0`, operationID).Scan(&id)
	if err == nil {
		return lifecycle.ErrBusy
	}
	if errors.Is(err, sql.ErrNoRows) {
		return nil
	}
	return err
}

func (s *Store) actionTarget(ctx context.Context, tx *sql.Tx, op lifecycle.Operation, request lifecycle.ActionRequest, now int64) (string, error) {
	var kind string
	if err := tx.QueryRowContext(ctx, `SELECT kind FROM dune_runners WHERE id=$1`, op.RunnerID).Scan(&kind); err != nil {
		return "", err
	}
	if kind != "managed" {
		return "", ErrInvalidArgument
	}
	resource, err := scanManagedResource(tx.QueryRowContext(ctx, "SELECT "+resourceColumns+" FROM dune_managed_resources WHERE runner_id=$1", op.RunnerID))
	if request.Kind == "create" {
		if op.Action != "create" || request.Digest != op.Digest {
			return "", ErrInvalidArgument
		}
		if err == nil {
			return "", lifecycle.ErrBusy
		}
		if !errors.Is(err, ErrNotFound) {
			return "", err
		}
		var creation string
		if err := tx.QueryRowContext(ctx, `SELECT operation_id FROM dune_managed_creations WHERE runner_id=$1 AND operation_id=$2`, op.RunnerID, op.ID).Scan(&creation); err != nil {
			return "", err
		}
		return "", nil
	}
	if err != nil {
		return "", err
	}
	if resource.FabricID != op.FabricID || resource.Gone {
		return "", lifecycle.ErrBusy
	}
	if request.Kind == "destroy" {
		if op.Action != "destroy" || !resource.AccessClosed {
			return "", lifecycle.ErrBusy
		}
	} else {
		if resource.AccessClosed || (!resource.ExpiresAt.IsZero() && resource.ExpiresAt.UnixMilli() <= now) {
			return "", lifecycle.ErrBusy
		}
		if request.Kind == "bootstrap" {
			if op.Action != "create" {
				return "", ErrInvalidArgument
			}
			created, err := scanAction(tx.QueryRowContext(ctx, "SELECT "+actionColumns+" FROM dune_provider_actions WHERE operation_id=$1 AND kind='create'", op.ID))
			if err != nil {
				return "", err
			}
			if created.Outcome != "succeeded" {
				return "", lifecycle.ErrBusy
			}
		} else if op.Action != "renew" || request.RenewUntil.UnixMilli() <= now {
			return "", ErrInvalidArgument
		}
	}
	return resource.Ref, nil
}

func validObservation(o lifecycle.ActionObservation) bool {
	if o.Outcome != "succeeded" && o.Outcome != "failed" && o.Outcome != "unknown" && o.Outcome != "timed_out" {
		return false
	}
	if len(o.ResourceRef) > 1024 || !utf8.ValidString(o.ResourceRef) || strings.ContainsFunc(o.ResourceRef, unicode.IsControl) {
		return false
	}
	if (!o.ExpiresAt.IsZero() && (o.ExpiresAt.UnixMilli() <= 0 || o.ResourceRef == "")) || (o.Gone && o.ResourceRef == "") {
		return false
	}
	return true
}

// RecordProviderAction atomically saves verified provider facts, the action
// result and the Operation's outcome/mutex. Unknown observations retain the
// business lock. Only terminal evidence may finish or advance external work.
// A current claimant can record reconciliation; a stale issuer cannot overwrite
// it. The observation must come from a trusted adapter, never a user assertion.
func (s *Store) RecordProviderAction(ctx context.Context, expected lifecycle.Operation, actionID string, observation lifecycle.ActionObservation) error {
	if !validObservation(observation) {
		return ErrInvalidArgument
	}
	return s.withOperation(ctx, expected.ID, func(tx *sql.Tx, current lifecycle.Operation, now int64) error {
		if !ownsOperation(current, expected, now) {
			return lifecycle.ErrLeaseLost
		}
		if !current.Exclusive {
			return lifecycle.ErrBusy
		}
		action, err := scanAction(tx.QueryRowContext(ctx, "SELECT "+actionColumns+" FROM dune_provider_actions WHERE id=$1 AND operation_id=$2", actionID, current.ID))
		if err != nil {
			return err
		}
		if !action.CompletedAt.IsZero() {
			return lifecycle.ErrIntentConflict
		}
		if err := s.recordActionResource(ctx, tx, current, action, observation, now); err != nil {
			return err
		}
		terminal := observation.Outcome == "succeeded" || observation.Outcome == "failed"
		completed := int64(0)
		if terminal {
			completed = now
		}
		if _, err := tx.ExecContext(ctx, `UPDATE dune_provider_actions SET outcome=$2,completed_at=$3 WHERE id=$1`, action.ID, observation.Outcome, completed); err != nil {
			return err
		}
		if !terminal {
			return s.updateOperationOutcome(ctx, tx, expected, observation.Outcome, false)
		}
		finished := observation.Outcome == "failed" || current.Action != "create"
		outcome := observation.Outcome
		if !finished {
			outcome = ""
		}
		if err := s.updateOperationOutcome(ctx, tx, expected, outcome, finished); err != nil {
			return err
		}
		if !finished && action.Kind == "bootstrap" {
			// External work has ended; observing the first connection must not prevent
			// a separate renewal Operation from acquiring the Runner business lock.
			err = s.updateOperationMutex(ctx, tx, expected, false)
		}
		return err
	})
}

func (s *Store) recordActionResource(ctx context.Context, tx *sql.Tx, op lifecycle.Operation, action lifecycle.ProviderAction, o lifecycle.ActionObservation, now int64) error {
	if action.Kind == "create" {
		if o.Outcome == "succeeded" && (o.ResourceRef == "" || o.Gone) {
			return ErrInvalidArgument
		}
	} else {
		if o.ResourceRef != "" && o.ResourceRef != action.ResourceRef {
			return lifecycle.ErrIntentConflict
		}
		if o.Outcome == "succeeded" {
			if o.ResourceRef == "" {
				return ErrInvalidArgument
			}
			if action.Kind == "destroy" {
				if !o.Gone {
					return ErrInvalidArgument
				}
			} else if o.Gone {
				return ErrInvalidArgument
			}
			if action.Kind == "renew" && (o.ExpiresAt.IsZero() || o.ExpiresAt.Before(action.RenewUntil)) {
				return ErrInvalidArgument
			}
		}
	}
	if o.ResourceRef == "" {
		return nil
	}
	resource, err := scanManagedResource(tx.QueryRowContext(ctx, "SELECT "+resourceColumns+" FROM dune_managed_resources WHERE runner_id=$1", op.RunnerID))
	if errors.Is(err, ErrNotFound) && action.Kind == "create" {
		_, err = tx.ExecContext(ctx, `INSERT INTO dune_managed_resources(runner_id,fabric_id,resource_ref,confirmed_at,expires_at,gone,access_closed) VALUES($1,$2,$3,$4,$5,$6,$6)`, op.RunnerID, op.FabricID, o.ResourceRef, now, optionalMillis(o.ExpiresAt), o.Gone)
		return err
	}
	if err != nil {
		return err
	}
	if resource.FabricID != op.FabricID || resource.Ref != o.ResourceRef || (resource.Gone && !o.Gone) {
		return lifecycle.ErrIntentConflict
	}
	_, err = tx.ExecContext(ctx, `UPDATE dune_managed_resources SET expires_at=CASE WHEN CAST($2 AS BIGINT)=0 THEN expires_at ELSE $2 END,gone=$3,access_closed=CASE WHEN $3 THEN TRUE ELSE access_closed END WHERE runner_id=$1`, op.RunnerID, optionalMillis(o.ExpiresAt), o.Gone)
	return err
}

// CheckProviderAction checks a first-dispatch reservation immediately before
// the adapter call. It cannot turn a false BeginProviderAction dispatch result
// into permission, and must not be used to repeat a call. The provider still
// needs its own fencing/deduplication to reject a paused issuer's late request.
func (s *Store) CheckProviderAction(ctx context.Context, expected lifecycle.Operation, actionID string) error {
	return s.withOperation(ctx, expected.ID, func(tx *sql.Tx, current lifecycle.Operation, now int64) error {
		if !ownsOperation(current, expected, now) {
			return lifecycle.ErrLeaseLost
		}
		if !current.Exclusive || current.Outcome != "" {
			return lifecycle.ErrBusy
		}
		action, err := scanAction(tx.QueryRowContext(ctx, "SELECT "+actionColumns+" FROM dune_provider_actions WHERE id=$1 AND operation_id=$2", actionID, current.ID))
		if err != nil {
			return err
		}
		if action.Worker != current.Worker || action.ExecutionRevision != current.Revision {
			return lifecycle.ErrLeaseLost
		}
		if action.Outcome != "" || !action.CompletedAt.IsZero() {
			return lifecycle.ErrBusy
		}
		return nil
	})
}
