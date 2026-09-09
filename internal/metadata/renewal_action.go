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

	"github.com/aiomni/dune/internal/identity"
	"github.com/aiomni/dune/internal/lifecycle"
	"github.com/aiomni/dune/internal/wire"
)

const managedRenewalColumns = "operation_id,runner_id,policy_version,renew_until"

func scanManagedRenewal(row interface{ Scan(...any) error }) (lifecycle.ManagedRenewal, error) {
	var renewal lifecycle.ManagedRenewal
	var until int64
	err := row.Scan(&renewal.OperationID, &renewal.RunnerID, &renewal.PolicyVersion, &until)
	renewal.Until = optionalTime(until)
	if errors.Is(err, sql.ErrNoRows) {
		err = ErrNotFound
	}
	return renewal, err
}

func (s *Store) ManagedRenewal(ctx context.Context, operationID string) (lifecycle.ManagedRenewal, error) {
	return scanManagedRenewal(s.db.QueryRowContext(ctx, "SELECT "+managedRenewalColumns+" FROM dune_managed_renewals WHERE operation_id=$1", operationID))
}

func renewalIntent(schedule lifecycle.RenewalSchedule, principalID, namespace, subject string) (lifecycle.Intent, error) {
	payload, err := json.Marshal(struct {
		RunnerID, FabricID, ProviderBindingID, ResourceRef, PolicyVersion string
		BindingRevision, ProviderBindingRevision, RenewUntil              int64
	}{schedule.RunnerID, schedule.FabricID, schedule.ProviderBindingID, schedule.ResourceRef, schedule.PolicyVersion, schedule.BindingRevision, schedule.ProviderBindingRevision, schedule.RenewUntil.UnixMilli()})
	if err != nil {
		return lifecycle.Intent{}, err
	}
	sum := sha256.Sum256(payload)
	digest := hex.EncodeToString(sum[:])
	return lifecycle.Intent{
		ID: wire.ID(), RequestKey: "maintenance-renew-" + digest, Digest: digest,
		PrincipalID: principalID, Namespace: namespace, Subject: subject,
		RunnerID: schedule.RunnerID, FabricID: schedule.FabricID,
		BindingRevision: schedule.BindingRevision, ProviderBindingID: schedule.ProviderBindingID,
		ProviderBindingRevision: schedule.ProviderBindingRevision, Action: "renew",
	}, nil
}

// RecoverableManagedRenewalSchedulesFor returns frozen policy decisions that
// have not yet been consumed into mutation Operations. The transaction below
// repeats every condition before creating and claiming work.
func (s *Store) RecoverableManagedRenewalSchedulesFor(ctx context.Context, fabricIDs []string, policyVersion string, limit int) ([]lifecycle.RenewalSchedule, error) {
	if limit < 1 || limit > 32 || !validPolicyVersion(policyVersion) {
		return nil, ErrInvalidArgument
	}
	if len(fabricIDs) == 0 {
		return []lifecycle.RenewalSchedule{}, nil
	}
	filter, filterArgs, err := managedFabricFilterColumn(fabricIDs, 3, "maintenance.fabric_id")
	if err != nil {
		return nil, err
	}
	columns := "maintenance." + strings.ReplaceAll(maintenanceColumns, ",", ",maintenance.")
	args := append([]any{limit, policyVersion}, filterArgs...)
	rows, err := s.db.QueryContext(ctx, `SELECT `+columns+` FROM dune_managed_maintenance maintenance
		JOIN dune_managed_resources resource ON resource.runner_id=maintenance.runner_id
		WHERE maintenance.policy_version=$2 AND maintenance.renew_until>0
			AND maintenance.lease_until<=`+s.databaseClock()+`
			AND resource.fabric_id=maintenance.fabric_id AND resource.resource_ref=maintenance.resource_ref
			AND resource.gone=FALSE AND resource.access_closed=FALSE`+filter+`
		ORDER BY maintenance.renew_until,maintenance.runner_id LIMIT $1`, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	schedules := make([]lifecycle.RenewalSchedule, 0, limit)
	for rows.Next() {
		schedule, err := scanRenewalSchedule(rows)
		if err != nil {
			return nil, err
		}
		schedules = append(schedules, schedule)
	}
	return schedules, rows.Err()
}

// ClaimScheduledManagedRenewal atomically consumes one frozen decision into a
// claimed automatic Operation. A target that is already past is cleared for a
// fresh inspection and never dispatched. Commit uncertainty returns no claim.
func (s *Store) ClaimScheduledManagedRenewal(ctx context.Context, expected lifecycle.RenewalSchedule, policyVersion, worker string, ttl time.Duration) (lifecycle.Operation, bool, error) {
	if expected.RunnerID == "" || !validPolicyVersion(policyVersion) || !validWorker(worker) || !validLeaseDuration(ttl) {
		return lifecycle.Operation{}, false, ErrInvalidArgument
	}
	var result lifecycle.Operation
	var consumed bool
	err := s.transaction(ctx, func(tx *sql.Tx) error {
		var principalID, namespace, subject string
		err := tx.QueryRowContext(ctx, `SELECT o.principal_id,o.identity_namespace,o.identity_subject
			FROM dune_managed_creations c JOIN dune_operations o ON o.id=c.operation_id
			WHERE c.runner_id=$1`, expected.RunnerID).Scan(&principalID, &namespace, &subject)
		if errors.Is(err, sql.ErrNoRows) {
			return ErrNotFound
		}
		if err != nil {
			return err
		}
		principalQuery := `SELECT id FROM dune_principals WHERE id=$1`
		if s.postgres {
			principalQuery += ` FOR UPDATE`
		}
		var lockedPrincipal string
		if err := tx.QueryRowContext(ctx, principalQuery, principalID).Scan(&lockedPrincipal); err != nil {
			if errors.Is(err, sql.ErrNoRows) {
				return identity.ErrUnauthorized
			}
			return err
		}
		fabricID, bindingRevision, err := s.lockOperationRunner(ctx, tx, expected.RunnerID)
		if err != nil {
			return err
		}
		current, err := s.renewalScheduleTx(ctx, tx, expected.RunnerID)
		if err != nil {
			return err
		}
		now, err := s.databaseNow(ctx, tx)
		if err != nil {
			return err
		}
		if fabricID != expected.FabricID || bindingRevision != expected.BindingRevision || current.RunnerID != expected.RunnerID ||
			current.FabricID != expected.FabricID || current.ProviderBindingID != expected.ProviderBindingID || current.ProviderBindingRevision != expected.ProviderBindingRevision || current.ResourceRef != expected.ResourceRef || current.BindingRevision != expected.BindingRevision ||
			current.PolicyVersion != policyVersion || current.PolicyVersion != expected.PolicyVersion || !current.ObservedAt.Equal(expected.ObservedAt) ||
			!current.RenewUntil.Equal(expected.RenewUntil) || current.RenewUntil.IsZero() || current.Until.UnixMilli() > now {
			return lifecycle.ErrBusy
		}
		resource, err := scanManagedResource(tx.QueryRowContext(ctx, "SELECT "+resourceColumns+" FROM dune_managed_resources WHERE runner_id=$1", expected.RunnerID))
		if err != nil {
			return err
		}
		if resource.FabricID != current.FabricID || resource.ProviderBindingID != current.ProviderBindingID || resource.ProviderBindingRevision != current.ProviderBindingRevision || resource.Ref != current.ResourceRef || resource.Gone || resource.AccessClosed {
			return lifecycle.ErrBusy
		}
		if current.RenewUntil.UnixMilli() <= now {
			update, err := tx.ExecContext(ctx, `UPDATE dune_managed_maintenance SET reason='',next_check_at=`+s.databaseClock()+`,renew_until=0 WHERE runner_id=$1 AND policy_version=$2 AND renew_until=$3`, current.RunnerID, current.PolicyVersion, current.RenewUntil.UnixMilli())
			if err != nil {
				return err
			}
			n, err := update.RowsAffected()
			if err != nil {
				return err
			}
			if n != 1 {
				return lifecycle.ErrLeaseLost
			}
			consumed = true
			return nil
		}
		var active string
		err = tx.QueryRowContext(ctx, `SELECT id FROM dune_operations WHERE runner_id=$1 AND exclusive=TRUE`, current.RunnerID).Scan(&active)
		if err == nil {
			return lifecycle.ErrBusy
		}
		if !errors.Is(err, sql.ErrNoRows) {
			return err
		}
		intent, err := renewalIntent(current, principalID, namespace, subject)
		if err != nil || !validOperationIntent(intent) {
			if err != nil {
				return err
			}
			return ErrInvalidArgument
		}
		until := now + ttl.Milliseconds()
		_, err = tx.ExecContext(ctx, `INSERT INTO dune_operations(id,request_key,request_digest,principal_id,identity_namespace,identity_subject,runner_id,fabric_id,binding_revision,provider_binding_id,provider_binding_revision,action,created_at,worker,execution_revision,lease_until)
			VALUES($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,'renew',$12,$13,1,$14)`, intent.ID, intent.RequestKey, intent.Digest, intent.PrincipalID, intent.Namespace, intent.Subject, intent.RunnerID, intent.FabricID, intent.BindingRevision, intent.ProviderBindingID, intent.ProviderBindingRevision, now, worker, until)
		if err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, `INSERT INTO dune_managed_renewals(operation_id,runner_id,policy_version,renew_until) VALUES($1,$2,$3,$4)`, intent.ID, current.RunnerID, current.PolicyVersion, current.RenewUntil.UnixMilli()); err != nil {
			return err
		}
		update, err := tx.ExecContext(ctx, `UPDATE dune_managed_maintenance SET reason='MUTATION_PENDING',next_check_at=`+s.databaseClock()+`+30000,renew_until=0 WHERE runner_id=$1 AND policy_version=$2 AND renew_until=$3 AND lease_until<=`+s.databaseClock(), current.RunnerID, current.PolicyVersion, current.RenewUntil.UnixMilli())
		if err != nil {
			return err
		}
		n, err := update.RowsAffected()
		if err != nil {
			return err
		}
		if n != 1 {
			return lifecycle.ErrLeaseLost
		}
		result = lifecycle.Operation{Intent: intent, CreatedAt: time.UnixMilli(now).UTC(), Exclusive: true, Lease: lifecycle.Lease{Worker: worker, Revision: 1, Until: time.UnixMilli(until).UTC()}}
		consumed = true
		return nil
	})
	if err != nil {
		return lifecycle.Operation{}, false, err
	}
	return result, consumed, nil
}

// RecoverableManagedRenewalsFor returns automatic renewal Operations whose
// execution lease is available and whose original action still needs work.
func (s *Store) RecoverableManagedRenewalsFor(ctx context.Context, fabricIDs []string, limit int) ([]lifecycle.Operation, error) {
	if limit < 1 || limit > 32 {
		return nil, ErrInvalidArgument
	}
	if len(fabricIDs) == 0 {
		return []lifecycle.Operation{}, nil
	}
	filter, filterArgs, err := managedFabricFilter(fabricIDs)
	if err != nil {
		return nil, err
	}
	columns := "o." + strings.ReplaceAll(operationColumns, ",", ",o.")
	args := append([]any{limit}, filterArgs...)
	rows, err := s.db.QueryContext(ctx, `SELECT `+columns+` FROM dune_operations o
		JOIN dune_managed_renewals renewal ON renewal.operation_id=o.id AND renewal.runner_id=o.runner_id
		LEFT JOIN dune_provider_actions action ON action.operation_id=o.id AND action.kind='renew'
		WHERE o.action='renew' AND o.finished=FALSE AND o.exclusive=TRUE AND o.lease_until<=`+s.databaseClock()+`
			AND (action.id IS NULL OR action.completed_at=0)`+filter+`
		ORDER BY o.created_at,o.id LIMIT $1`, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	operations := make([]lifecycle.Operation, 0, limit)
	for rows.Next() {
		op, err := scanOperation(rows)
		if err != nil {
			return nil, err
		}
		operations = append(operations, op)
	}
	return operations, rows.Err()
}

func (s *Store) ClaimRecoverableManagedRenewal(ctx context.Context, id, worker string, ttl time.Duration) (lifecycle.Operation, error) {
	return s.claimOperation(ctx, id, worker, ttl, func(tx *sql.Tx, op lifecycle.Operation, _ int64) error {
		if op.Action != "renew" || !op.Exclusive {
			return lifecycle.ErrBusy
		}
		if _, err := scanManagedRenewal(tx.QueryRowContext(ctx, "SELECT "+managedRenewalColumns+" FROM dune_managed_renewals WHERE operation_id=$1 AND runner_id=$2", op.ID, op.RunnerID)); err != nil {
			if errors.Is(err, ErrNotFound) {
				return lifecycle.ErrBusy
			}
			return err
		}
		var completed int64
		err := tx.QueryRowContext(ctx, `SELECT completed_at FROM dune_provider_actions WHERE operation_id=$1 AND kind='renew'`, op.ID).Scan(&completed)
		if errors.Is(err, sql.ErrNoRows) {
			return nil
		}
		if err != nil {
			return err
		}
		if completed != 0 {
			return lifecycle.ErrBusy
		}
		return nil
	})
}

// BeginManagedRenewal reserves the action or terminally rejects an unsubmitted
// target that is no longer meaningful according to the database clock.
func (s *Store) BeginManagedRenewal(ctx context.Context, expected lifecycle.Operation) (lifecycle.ProviderAction, bool, bool, error) {
	var action lifecycle.ProviderAction
	var dispatch, expired bool
	err := s.withOperation(ctx, expected.ID, func(tx *sql.Tx, current lifecycle.Operation, now int64) error {
		if !ownsOperation(current, expected, now) || current.Action != "renew" || !current.Exclusive {
			return lifecycle.ErrLeaseLost
		}
		renewal, err := scanManagedRenewal(tx.QueryRowContext(ctx, "SELECT "+managedRenewalColumns+" FROM dune_managed_renewals WHERE operation_id=$1 AND runner_id=$2", current.ID, current.RunnerID))
		if err != nil {
			return err
		}
		previous, previousErr := scanAction(tx.QueryRowContext(ctx, "SELECT "+actionColumns+" FROM dune_provider_actions WHERE operation_id=$1 AND kind='renew'", current.ID))
		if previousErr == nil {
			action = previous
			return nil
		}
		if !errors.Is(previousErr, ErrNotFound) {
			return previousErr
		}
		resource, err := scanManagedResource(tx.QueryRowContext(ctx, "SELECT "+resourceColumns+" FROM dune_managed_resources WHERE runner_id=$1", current.RunnerID))
		if err != nil {
			return err
		}
		if renewal.Until.UnixMilli() <= now || resource.Gone || resource.AccessClosed || (!resource.ExpiresAt.IsZero() && resource.ExpiresAt.UnixMilli() <= now) {
			if err := s.updateOperationOutcome(ctx, tx, expected, "failed", true); err != nil {
				return err
			}
			_, err = tx.ExecContext(ctx, `UPDATE dune_managed_maintenance SET reason='',next_check_at=`+s.databaseClock()+`,renew_until=0 WHERE runner_id=$1`, current.RunnerID)
			expired = err == nil
			return err
		}
		action, dispatch, err = s.beginProviderAction(ctx, tx, current, lifecycle.ActionRequest{Kind: "renew", Digest: current.Digest, RenewUntil: renewal.Until}, now)
		return err
	})
	if err != nil {
		return lifecycle.ProviderAction{}, false, false, err
	}
	return action, dispatch, expired, nil
}
