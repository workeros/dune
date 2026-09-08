package metadata

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/aiomni/dune/internal/authorization"
	"github.com/aiomni/dune/internal/identity"
	"github.com/aiomni/dune/internal/lifecycle"
	"github.com/aiomni/dune/internal/wire"
	"github.com/aiomni/dune/pkg/runner"
)

const reviewColumns = "id,request_key,request_digest,principal_id,identity_namespace,identity_subject,operation_id,action_id,mode,candidate_resource_ref,reason,created_at,completed_at,outcome,verified_resource_ref,worker,execution_revision,lease_until"

func scanManagedReview(row interface{ Scan(...any) error }) (lifecycle.Review, error) {
	var review lifecycle.Review
	var created, completed, until int64
	err := row.Scan(&review.ID, &review.RequestKey, &review.Digest, &review.PrincipalID, &review.Namespace, &review.Subject,
		&review.OperationID, &review.ActionID, &review.Mode, &review.Candidate, &review.Reason, &created, &completed,
		&review.Outcome, &review.VerifiedResourceRef, &review.Worker, &review.Revision, &until)
	review.CreatedAt, review.CompletedAt, review.Until = optionalTime(created), optionalTime(completed), optionalTime(until)
	if errors.Is(err, sql.ErrNoRows) {
		err = ErrNotFound
	}
	return review, err
}

func validReviewRequest(request lifecycle.ReviewRequest) bool {
	if request.OperationID == "" || len(request.OperationID) > 128 || !utf8.ValidString(request.OperationID) || strings.ContainsFunc(request.OperationID, unicode.IsControl) ||
		request.Reason == "" || len(request.Reason) > 512 || !utf8.ValidString(request.Reason) || strings.TrimSpace(request.Reason) != request.Reason || strings.ContainsFunc(request.Reason, unicode.IsControl) {
		return false
	}
	if request.Mode == lifecycle.ReviewReconcile {
		return request.Candidate == ""
	}
	return request.Mode == lifecycle.ReviewCandidate && request.Candidate != "" && len(request.Candidate) <= 1024 && utf8.ValidString(request.Candidate) && strings.TrimSpace(request.Candidate) == request.Candidate && !strings.ContainsFunc(request.Candidate, unicode.IsControl)
}

func reviewDigest(request lifecycle.ReviewRequest) (string, error) {
	encoded, err := json.Marshal(request)
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256(encoded)
	return hex.EncodeToString(sum[:]), nil
}

// CreateManagedReview records one authorized request to re-check an unresolved
// provider action. It revalidates the browser session and immutable Runner,
// Operation and action facts in one transaction. It never changes a lifecycle
// outcome or grants provider mutation authority.
func (s *Store) CreateManagedReview(ctx context.Context, user identity.User, sessionHash, requestKey string, selected authorization.Resource, request lifecycle.ReviewRequest) (lifecycle.Review, error) {
	digest, err := reviewDigest(request)
	if err != nil || sessionHash == "" || requestKey == "" || len(requestKey) > 128 || !utf8.ValidString(requestKey) || strings.ContainsFunc(requestKey, unicode.IsControl) ||
		!validReviewRequest(request) || selected.Runner.ID == "" || selected.Runner.Kind != "managed" || selected.FabricID == "" || selected.BindingRevision <= 0 {
		return lifecycle.Review{}, ErrInvalidArgument
	}
	var result lifecycle.Review
	err = s.transaction(ctx, func(tx *sql.Tx) error {
		if err := s.lockPrincipal(ctx, tx, user.ID); err != nil {
			return err
		}
		if err := s.checkBrowserSession(ctx, tx, user.ID, sessionHash, user.Namespace, user.Subject); err != nil {
			return err
		}
		previous, err := scanManagedReview(tx.QueryRowContext(ctx, "SELECT "+reviewColumns+" FROM dune_managed_reviews WHERE principal_id=$1 AND request_key=$2", user.ID, requestKey))
		if err == nil {
			if previous.Digest != digest || previous.Namespace != user.Namespace || previous.Subject != user.Subject {
				return lifecycle.ErrIntentConflict
			}
			result = previous
			return nil
		}
		if !errors.Is(err, ErrNotFound) {
			return err
		}
		var runnerID string
		if err := tx.QueryRowContext(ctx, `SELECT runner_id FROM dune_operations WHERE id=$1`, request.OperationID).Scan(&runnerID); err != nil {
			if errors.Is(err, sql.ErrNoRows) {
				return ErrNotFound
			}
			return err
		}
		fabricID, revision, err := s.lockOperationRunner(ctx, tx, runnerID)
		if err != nil {
			return err
		}
		var ownerID, kind string
		if err := tx.QueryRowContext(ctx, `SELECT owner_id,kind FROM dune_runners WHERE id=$1`, runnerID).Scan(&ownerID, &kind); err != nil {
			return err
		}
		operation, err := scanOperation(tx.QueryRowContext(ctx, "SELECT "+operationColumns+" FROM dune_operations WHERE id=$1", request.OperationID))
		if err != nil {
			return err
		}
		if runnerID != selected.Runner.ID || ownerID != selected.OwnerID || kind != "managed" || fabricID != selected.FabricID || revision != selected.BindingRevision ||
			operation.RunnerID != runnerID || operation.FabricID != fabricID || operation.BindingRevision != revision || operation.Finished || !operation.Exclusive {
			return runner.ErrBindingChanged
		}
		action, err := scanAction(tx.QueryRowContext(ctx, "SELECT "+actionColumns+" FROM dune_provider_actions WHERE operation_id=$1 AND completed_at=0", operation.ID))
		if err != nil {
			return err
		}
		if action.Outcome != "unknown" && action.Outcome != "timed_out" {
			return lifecycle.ErrBusy
		}
		if request.Mode == lifecycle.ReviewCandidate && action.Kind != "create" {
			return ErrInvalidArgument
		}
		if request.Mode == lifecycle.ReviewCandidate {
			resource, resourceErr := scanManagedResource(tx.QueryRowContext(ctx, "SELECT "+resourceColumns+" FROM dune_managed_resources WHERE runner_id=$1", operation.RunnerID))
			if resourceErr == nil && resource.Ref != request.Candidate {
				return lifecycle.ErrIntentConflict
			}
			if resourceErr != nil && !errors.Is(resourceErr, ErrNotFound) {
				return resourceErr
			}
		}
		var pending string
		err = tx.QueryRowContext(ctx, `SELECT id FROM dune_managed_reviews WHERE action_id=$1 AND completed_at=0`, action.ID).Scan(&pending)
		if err == nil {
			return lifecycle.ErrBusy
		}
		if !errors.Is(err, sql.ErrNoRows) {
			return err
		}
		now, err := s.databaseNow(ctx, tx)
		if err != nil {
			return err
		}
		result = lifecycle.Review{ID: wire.ID(), RequestKey: requestKey, Digest: digest, PrincipalID: user.ID, Namespace: user.Namespace, Subject: user.Subject,
			OperationID: operation.ID, ActionID: action.ID, Mode: request.Mode, Candidate: request.Candidate, Reason: request.Reason, CreatedAt: time.UnixMilli(now).UTC()}
		_, err = tx.ExecContext(ctx, `INSERT INTO dune_managed_reviews(id,request_key,request_digest,principal_id,identity_namespace,identity_subject,operation_id,action_id,mode,candidate_resource_ref,reason,created_at) VALUES($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12)`,
			result.ID, result.RequestKey, result.Digest, result.PrincipalID, result.Namespace, result.Subject, result.OperationID, result.ActionID, result.Mode, result.Candidate, result.Reason, now)
		return err
	})
	if err != nil {
		return lifecycle.Review{}, err
	}
	return result, nil
}

func (s *Store) ManagedReview(ctx context.Context, principalID, id string) (lifecycle.Review, error) {
	return scanManagedReview(s.db.QueryRowContext(ctx, "SELECT "+reviewColumns+" FROM dune_managed_reviews WHERE id=$1 AND principal_id=$2", id, principalID))
}

func (s *Store) ManagedOperationReview(ctx context.Context, principalID, operationID string) (lifecycle.Review, error) {
	return scanManagedReview(s.db.QueryRowContext(ctx, "SELECT "+reviewColumns+" FROM dune_managed_reviews WHERE principal_id=$1 AND operation_id=$2 ORDER BY created_at DESC,id DESC LIMIT 1", principalID, operationID))
}

func (s *Store) RecoverableManagedReviewsFor(ctx context.Context, fabricIDs []string, limit int) ([]lifecycle.Review, error) {
	if limit < 1 || limit > 32 {
		return nil, ErrInvalidArgument
	}
	if len(fabricIDs) == 0 {
		return []lifecycle.Review{}, nil
	}
	filter, args, err := managedFabricFilterColumn(fabricIDs, 2, "o.fabric_id")
	if err != nil {
		return nil, err
	}
	queryArgs := append([]any{limit}, args...)
	columns := "review." + strings.ReplaceAll(reviewColumns, ",", ",review.")
	rows, err := s.db.QueryContext(ctx, `SELECT `+columns+` FROM dune_managed_reviews review
		JOIN dune_operations o ON o.id=review.operation_id
		JOIN dune_provider_actions action ON action.id=review.action_id AND action.operation_id=o.id
		WHERE review.completed_at=0 AND review.lease_until<=`+s.databaseClock()+`
			AND (action.completed_at>0 OR (o.finished=FALSE AND o.exclusive=TRUE AND o.lease_until<=`+s.databaseClock()+` AND action.completed_at=0 AND action.outcome IN ('unknown','timed_out')))`+filter+`
		ORDER BY review.created_at,review.id LIMIT $1`, queryArgs...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	result := make([]lifecycle.Review, 0, limit)
	for rows.Next() {
		review, err := scanManagedReview(rows)
		if err != nil {
			return nil, err
		}
		result = append(result, review)
	}
	return result, rows.Err()
}

func finishReviewFromAction(ctx context.Context, tx *sql.Tx, review lifecycle.Review, action lifecycle.ProviderAction, now int64) error {
	outcome := action.Outcome
	if outcome == "" {
		outcome = "unknown"
	}
	verified := ""
	if outcome == "succeeded" {
		verified = action.ResourceRef
		if verified == "" {
			if err := tx.QueryRowContext(ctx, `SELECT resource.resource_ref FROM dune_operations operation JOIN dune_managed_resources resource ON resource.runner_id=operation.runner_id WHERE operation.id=$1`, review.OperationID).Scan(&verified); err != nil {
				return err
			}
		}
	}
	result, err := tx.ExecContext(ctx, `UPDATE dune_managed_reviews SET completed_at=$2,outcome=$3,verified_resource_ref=$4,worker='',lease_until=0 WHERE id=$1 AND completed_at=0`, review.ID, now, outcome, verified)
	if err != nil {
		return err
	}
	n, err := result.RowsAffected()
	if err != nil {
		return err
	}
	if n != 1 {
		return lifecycle.ErrIntentConflict
	}
	return nil
}

func finishClaimedReviewFromAction(ctx context.Context, tx *sql.Tx, review lifecycle.Review, action lifecycle.ProviderAction, now int64, clock string) error {
	outcome := action.Outcome
	if outcome == "" {
		outcome = "unknown"
	}
	verified := ""
	if outcome == "succeeded" {
		verified = action.ResourceRef
		if verified == "" {
			if err := tx.QueryRowContext(ctx, `SELECT resource.resource_ref FROM dune_operations operation JOIN dune_managed_resources resource ON resource.runner_id=operation.runner_id WHERE operation.id=$1`, review.OperationID).Scan(&verified); err != nil {
				return err
			}
		}
	}
	result, err := tx.ExecContext(ctx, `UPDATE dune_managed_reviews SET completed_at=$2,outcome=$3,verified_resource_ref=$4,worker='',lease_until=0 WHERE id=$1 AND completed_at=0 AND worker=$5 AND execution_revision=$6 AND lease_until>`+clock,
		review.ID, now, outcome, verified, review.Worker, review.Revision)
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
	return nil
}

// ClaimManagedReview serializes a manual read-only check with the original
// Operation. If another worker already resolved the action, it only closes the
// audit record and returns a completed review.
func (s *Store) ClaimManagedReview(ctx context.Context, id, worker string, ttl time.Duration) (lifecycle.Review, lifecycle.Operation, error) {
	if !validWorker(worker) || !validLeaseDuration(ttl) {
		return lifecycle.Review{}, lifecycle.Operation{}, ErrInvalidArgument
	}
	var result lifecycle.Review
	var claimed lifecycle.Operation
	err := s.transaction(ctx, func(tx *sql.Tx) error {
		review, err := scanManagedReview(tx.QueryRowContext(ctx, "SELECT "+reviewColumns+" FROM dune_managed_reviews WHERE id=$1", id))
		if err != nil {
			return err
		}
		var runnerID string
		if err := tx.QueryRowContext(ctx, `SELECT runner_id FROM dune_operations WHERE id=$1`, review.OperationID).Scan(&runnerID); err != nil {
			return err
		}
		if _, _, err := s.lockOperationRunner(ctx, tx, runnerID); err != nil {
			return err
		}
		now, err := s.databaseNow(ctx, tx)
		if err != nil {
			return err
		}
		operation, err := scanOperation(tx.QueryRowContext(ctx, "SELECT "+operationColumns+" FROM dune_operations WHERE id=$1", review.OperationID))
		if err != nil {
			return err
		}
		action, err := scanAction(tx.QueryRowContext(ctx, "SELECT "+actionColumns+" FROM dune_provider_actions WHERE id=$1 AND operation_id=$2", review.ActionID, review.OperationID))
		if err != nil {
			return err
		}
		if !review.CompletedAt.IsZero() || review.Until.UnixMilli() > now {
			return lifecycle.ErrBusy
		}
		if !action.CompletedAt.IsZero() {
			if err := finishReviewFromAction(ctx, tx, review, action, now); err != nil {
				return err
			}
			result, err = scanManagedReview(tx.QueryRowContext(ctx, "SELECT "+reviewColumns+" FROM dune_managed_reviews WHERE id=$1", review.ID))
			return err
		}
		if operation.Finished || !operation.Exclusive || operation.Until.UnixMilli() > now || (action.Outcome != "unknown" && action.Outcome != "timed_out") {
			return lifecycle.ErrBusy
		}
		var opUntil int64
		err = tx.QueryRowContext(ctx, `UPDATE dune_operations SET worker=$2,execution_revision=execution_revision+1,lease_until=`+s.databaseClock()+`+$3 WHERE id=$1 AND execution_revision<9223372036854775807 RETURNING execution_revision,lease_until`, operation.ID, worker, ttl.Milliseconds()).Scan(&operation.Revision, &opUntil)
		if errors.Is(err, sql.ErrNoRows) {
			return lifecycle.ErrLeaseLost
		}
		if err != nil {
			return err
		}
		var reviewUntil int64
		err = tx.QueryRowContext(ctx, `UPDATE dune_managed_reviews SET worker=$2,execution_revision=execution_revision+1,lease_until=`+s.databaseClock()+`+$3 WHERE id=$1 AND completed_at=0 AND execution_revision<9223372036854775807 RETURNING execution_revision,lease_until`, review.ID, worker, ttl.Milliseconds()).Scan(&review.Revision, &reviewUntil)
		if errors.Is(err, sql.ErrNoRows) {
			return lifecycle.ErrLeaseLost
		}
		if err != nil {
			return err
		}
		operation.Worker, operation.Until = worker, time.UnixMilli(opUntil).UTC()
		review.Worker, review.Until = worker, time.UnixMilli(reviewUntil).UTC()
		result, claimed = review, operation
		return nil
	})
	if err != nil {
		return lifecycle.Review{}, lifecycle.Operation{}, err
	}
	return result, claimed, nil
}

// CompleteManagedReview snapshots the result of a reconciliation already
// committed through the ordinary action journal. A crash between those two
// transactions is recoverable because ClaimManagedReview closes terminal
// actions without another provider call.
func (s *Store) CompleteManagedReview(ctx context.Context, expected lifecycle.Review) error {
	return s.transaction(ctx, func(tx *sql.Tx) error {
		review, err := scanManagedReview(tx.QueryRowContext(ctx, "SELECT "+reviewColumns+" FROM dune_managed_reviews WHERE id=$1", expected.ID))
		if err != nil {
			return err
		}
		now, err := s.databaseNow(ctx, tx)
		if err != nil {
			return err
		}
		if review.Worker != expected.Worker || review.Revision != expected.Revision || review.Until.UnixMilli() <= now || !review.CompletedAt.IsZero() {
			return lifecycle.ErrLeaseLost
		}
		action, err := scanAction(tx.QueryRowContext(ctx, "SELECT "+actionColumns+" FROM dune_provider_actions WHERE id=$1 AND operation_id=$2", review.ActionID, review.OperationID))
		if err != nil {
			return err
		}
		return finishClaimedReviewFromAction(ctx, tx, review, action, now, s.databaseClock())
	})
}

// RenewManagedReviewLease keeps the review and its operation claim aligned.
// Both updates use the database clock; losing either claim cancels the caller's
// authority to save provider facts.
func (s *Store) RenewManagedReviewLease(ctx context.Context, review lifecycle.Review, operation lifecycle.Operation, ttl time.Duration) (lifecycle.Review, lifecycle.Operation, error) {
	if !validLeaseDuration(ttl) || review.Worker == "" || review.Worker != operation.Worker || review.OperationID != operation.ID {
		return lifecycle.Review{}, lifecycle.Operation{}, ErrInvalidArgument
	}
	err := s.withOperation(ctx, operation.ID, func(tx *sql.Tx, current lifecycle.Operation, now int64) error {
		if !ownsOperation(current, operation, now) {
			return lifecycle.ErrLeaseLost
		}
		stored, err := scanManagedReview(tx.QueryRowContext(ctx, "SELECT "+reviewColumns+" FROM dune_managed_reviews WHERE id=$1", review.ID))
		if err != nil {
			return err
		}
		if stored.Worker != review.Worker || stored.Revision != review.Revision || stored.Until.UnixMilli() <= now || !stored.CompletedAt.IsZero() {
			return lifecycle.ErrLeaseLost
		}
		var operationUntil int64
		clock := s.databaseClock()
		if err := tx.QueryRowContext(ctx, `UPDATE dune_operations SET lease_until=CASE WHEN lease_until>`+clock+`+$4 THEN lease_until ELSE `+clock+`+$4 END WHERE id=$1 AND worker=$2 AND execution_revision=$3 AND lease_until>`+clock+` RETURNING lease_until`, operation.ID, operation.Worker, operation.Revision, ttl.Milliseconds()).Scan(&operationUntil); err != nil {
			if errors.Is(err, sql.ErrNoRows) {
				return lifecycle.ErrLeaseLost
			}
			return err
		}
		var reviewUntil int64
		if err := tx.QueryRowContext(ctx, `UPDATE dune_managed_reviews SET lease_until=CASE WHEN lease_until>`+clock+`+$4 THEN lease_until ELSE `+clock+`+$4 END WHERE id=$1 AND worker=$2 AND execution_revision=$3 AND lease_until>`+clock+` AND completed_at=0 RETURNING lease_until`, review.ID, review.Worker, review.Revision, ttl.Milliseconds()).Scan(&reviewUntil); err != nil {
			if errors.Is(err, sql.ErrNoRows) {
				return lifecycle.ErrLeaseLost
			}
			return err
		}
		operation.Until, review.Until = time.UnixMilli(operationUntil).UTC(), time.UnixMilli(reviewUntil).UTC()
		return nil
	})
	if err != nil {
		return lifecycle.Review{}, lifecycle.Operation{}, err
	}
	return review, operation, nil
}
