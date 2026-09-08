package metadata

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/aiomni/dune/internal/lifecycle"
)

const maintenanceColumns = "runner_id,fabric_id,resource_ref,binding_revision,policy_version,reason,facts,observed_at,next_check_at,renew_until,worker,execution_revision,lease_until"

func scanRenewalSchedule(row interface{ Scan(...any) error }) (lifecycle.RenewalSchedule, error) {
	var schedule lifecycle.RenewalSchedule
	var observed, next, renew, until int64
	err := row.Scan(&schedule.RunnerID, &schedule.FabricID, &schedule.ResourceRef, &schedule.BindingRevision,
		&schedule.PolicyVersion, &schedule.Reason, &schedule.Facts, &observed, &next, &renew,
		&schedule.Worker, &schedule.Revision, &until)
	schedule.ObservedAt, schedule.NextCheckAt, schedule.RenewUntil, schedule.Until = optionalTime(observed), optionalTime(next), optionalTime(renew), optionalTime(until)
	if errors.Is(err, sql.ErrNoRows) {
		err = ErrNotFound
	}
	return schedule, err
}

func validPolicyVersion(version string) bool {
	return version != "" && len(version) <= 128 && utf8.ValidString(version) && strings.TrimSpace(version) == version && !strings.ContainsFunc(version, unicode.IsControl)
}

func validInspection(inspection lifecycle.ResourceInspection) bool {
	if inspection.Status != lifecycle.InspectionConfirmed && inspection.Status != lifecycle.InspectionUnknown && inspection.Status != lifecycle.InspectionTimedOut {
		return false
	}
	if inspection.Status != lifecycle.InspectionConfirmed {
		return inspection.ResourceRef == "" && inspection.ExpiresAt.IsZero() && !inspection.Gone
	}
	if inspection.ResourceRef == "" || len(inspection.ResourceRef) > 1024 || !utf8.ValidString(inspection.ResourceRef) || strings.ContainsFunc(inspection.ResourceRef, unicode.IsControl) {
		return false
	}
	if inspection.Gone {
		return inspection.ExpiresAt.IsZero()
	}
	return !inspection.ExpiresAt.IsZero() && inspection.ExpiresAt.UnixMilli() > 0
}

func renewalScheduleDue(schedule lifecycle.RenewalSchedule, policyVersion string, now int64) bool {
	return schedule.Until.UnixMilli() <= now && (schedule.PolicyVersion != policyVersion || schedule.Reason == "" ||
		(schedule.RenewUntil.IsZero() && !schedule.NextCheckAt.IsZero() && schedule.NextCheckAt.UnixMilli() <= now))
}

// RecoverableManagedInspectionsFor lists only configured live resources whose
// persisted policy decision is due. The claim transaction repeats all checks.
func (s *Store) RecoverableManagedInspectionsFor(ctx context.Context, fabricIDs []string, policyVersion string, limit int) ([]lifecycle.Resource, error) {
	if limit < 1 || limit > 32 || !validPolicyVersion(policyVersion) {
		return nil, ErrInvalidArgument
	}
	if len(fabricIDs) == 0 {
		return []lifecycle.Resource{}, nil
	}
	fabricFilter, fabricArgs, err := managedFabricFilterColumn(fabricIDs, 3, "resource.fabric_id")
	if err != nil {
		return nil, err
	}
	columns := "resource." + strings.ReplaceAll(resourceColumns, ",", ",resource.")
	args := append([]any{limit, policyVersion}, fabricArgs...)
	rows, err := s.db.QueryContext(ctx, `SELECT `+columns+`
		FROM dune_managed_resources resource
		JOIN dune_runners runner ON runner.id=resource.runner_id AND runner.kind='managed'
		LEFT JOIN dune_managed_maintenance maintenance ON maintenance.runner_id=resource.runner_id
		WHERE resource.gone=FALSE AND resource.access_closed=FALSE`+fabricFilter+`
			AND (maintenance.runner_id IS NULL OR (maintenance.lease_until<=`+s.databaseClock()+` AND
				(maintenance.policy_version<>$2 OR maintenance.reason='' OR
				(maintenance.renew_until=0 AND maintenance.next_check_at>0 AND maintenance.next_check_at<=`+s.databaseClock()+`))))
		ORDER BY resource.confirmed_at,resource.runner_id LIMIT $1`, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	resources := make([]lifecycle.Resource, 0, limit)
	for rows.Next() {
		resource, err := scanManagedResource(rows)
		if err != nil {
			return nil, err
		}
		resources = append(resources, resource)
	}
	return resources, rows.Err()
}

// ClaimManagedInspection serializes provider fact reads without granting any
// mutation. Policy changes make stopped schedules eligible again, while an
// already scheduled renewal remains frozen until a later transaction consumes
// or reevaluates it.
func (s *Store) ClaimManagedInspection(ctx context.Context, runnerID, policyVersion, worker string, ttl time.Duration) (lifecycle.RenewalSchedule, error) {
	if runnerID == "" || !validPolicyVersion(policyVersion) || !validWorker(worker) || !validLeaseDuration(ttl) {
		return lifecycle.RenewalSchedule{}, ErrInvalidArgument
	}
	var claimed lifecycle.RenewalSchedule
	err := s.transaction(ctx, func(tx *sql.Tx) error {
		fabricID, bindingRevision, err := s.lockOperationRunner(ctx, tx, runnerID)
		if err != nil {
			return err
		}
		var kind string
		if err := tx.QueryRowContext(ctx, `SELECT kind FROM dune_runners WHERE id=$1`, runnerID).Scan(&kind); err != nil {
			return err
		}
		resource, err := scanManagedResource(tx.QueryRowContext(ctx, "SELECT "+resourceColumns+" FROM dune_managed_resources WHERE runner_id=$1", runnerID))
		if err != nil {
			return err
		}
		if kind != "managed" || resource.FabricID != fabricID || resource.Ref == "" || resource.Gone || resource.AccessClosed {
			return lifecycle.ErrBusy
		}
		if _, err := tx.ExecContext(ctx, `INSERT INTO dune_managed_maintenance(runner_id,fabric_id,resource_ref,binding_revision) VALUES($1,$2,$3,$4) ON CONFLICT(runner_id) DO NOTHING`, runnerID, fabricID, resource.Ref, bindingRevision); err != nil {
			return err
		}
		query := "SELECT " + maintenanceColumns + " FROM dune_managed_maintenance WHERE runner_id=$1"
		if s.postgres {
			query += ` FOR UPDATE`
		}
		schedule, err := scanRenewalSchedule(tx.QueryRowContext(ctx, query, runnerID))
		if err != nil {
			return err
		}
		now, err := s.databaseNow(ctx, tx)
		if err != nil {
			return err
		}
		if schedule.FabricID != fabricID || schedule.ResourceRef != resource.Ref || schedule.BindingRevision != bindingRevision || !renewalScheduleDue(schedule, policyVersion, now) {
			return lifecycle.ErrBusy
		}
		var until int64
		err = tx.QueryRowContext(ctx, `UPDATE dune_managed_maintenance SET worker=$2,execution_revision=execution_revision+1,lease_until=`+s.databaseClock()+`+$3 WHERE runner_id=$1 AND lease_until<=`+s.databaseClock()+` AND execution_revision<9223372036854775807 RETURNING execution_revision,lease_until`, runnerID, worker, ttl.Milliseconds()).Scan(&schedule.Revision, &until)
		if errors.Is(err, sql.ErrNoRows) {
			return lifecycle.ErrLeaseLost
		}
		if err != nil {
			return err
		}
		schedule.Worker, schedule.Until = worker, time.UnixMilli(until).UTC()
		claimed = schedule
		return nil
	})
	if err != nil {
		return lifecycle.RenewalSchedule{}, err
	}
	return claimed, nil
}

func ownsRenewalSchedule(current, expected lifecycle.RenewalSchedule, now int64) bool {
	return current.RunnerID == expected.RunnerID && current.FabricID == expected.FabricID && current.ResourceRef == expected.ResourceRef &&
		current.BindingRevision == expected.BindingRevision && current.Worker == expected.Worker && current.Revision == expected.Revision && current.Until.UnixMilli() > now
}

func (s *Store) renewalScheduleTx(ctx context.Context, tx *sql.Tx, runnerID string) (lifecycle.RenewalSchedule, error) {
	query := "SELECT " + maintenanceColumns + " FROM dune_managed_maintenance WHERE runner_id=$1"
	if s.postgres {
		query += ` FOR UPDATE`
	}
	return scanRenewalSchedule(tx.QueryRowContext(ctx, query, runnerID))
}

// RenewManagedInspectionLease extends only the current inspection claim. It
// cannot revive an expired claimant or alter the persisted policy decision.
func (s *Store) RenewManagedInspectionLease(ctx context.Context, expected lifecycle.RenewalSchedule, ttl time.Duration) (lifecycle.RenewalSchedule, error) {
	if !validLeaseDuration(ttl) {
		return lifecycle.RenewalSchedule{}, ErrInvalidArgument
	}
	var renewed lifecycle.RenewalSchedule
	err := s.transaction(ctx, func(tx *sql.Tx) error {
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
		if fabricID != expected.FabricID || bindingRevision != expected.BindingRevision || !ownsRenewalSchedule(current, expected, now) {
			return lifecycle.ErrLeaseLost
		}
		var until int64
		clock := s.databaseClock()
		err = tx.QueryRowContext(ctx, `UPDATE dune_managed_maintenance SET lease_until=CASE WHEN lease_until>`+clock+`+$2 THEN lease_until ELSE `+clock+`+$2 END WHERE runner_id=$1 AND worker=$3 AND execution_revision=$4 AND lease_until>`+clock+` RETURNING lease_until`, expected.RunnerID, ttl.Milliseconds(), expected.Worker, expected.Revision).Scan(&until)
		if errors.Is(err, sql.ErrNoRows) {
			return lifecycle.ErrLeaseLost
		}
		current.Until = time.UnixMilli(until).UTC()
		renewed = current
		return err
	})
	if err != nil {
		return lifecycle.RenewalSchedule{}, err
	}
	return renewed, nil
}

// RecordManagedInspection saves one trusted fact read and the pure policy
// decision in one transaction. A confirmed missing resource closes Managed
// access immediately; unknown reads retain the last confirmed resource facts.
func (s *Store) RecordManagedInspection(ctx context.Context, expected lifecycle.RenewalSchedule, policyVersion string, config lifecycle.RenewalConfig, inspection lifecycle.ResourceInspection) (lifecycle.RenewalSchedule, error) {
	if !validPolicyVersion(policyVersion) || !validInspection(inspection) {
		return lifecycle.RenewalSchedule{}, ErrInvalidArgument
	}
	var saved lifecycle.RenewalSchedule
	err := s.transaction(ctx, func(tx *sql.Tx) error {
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
		if fabricID != expected.FabricID || bindingRevision != expected.BindingRevision || !ownsRenewalSchedule(current, expected, now) {
			return lifecycle.ErrLeaseLost
		}
		resource, err := scanManagedResource(tx.QueryRowContext(ctx, "SELECT "+resourceColumns+" FROM dune_managed_resources WHERE runner_id=$1", expected.RunnerID))
		if err != nil {
			return err
		}
		if resource.FabricID != expected.FabricID || resource.Ref != expected.ResourceRef || resource.Gone || resource.AccessClosed {
			return lifecycle.ErrIntentConflict
		}
		if inspection.Status == lifecycle.InspectionConfirmed {
			if inspection.ResourceRef != resource.Ref {
				return lifecycle.ErrIntentConflict
			}
			if inspection.Gone {
				if _, err := tx.ExecContext(ctx, `UPDATE dune_managed_resources SET gone=TRUE,access_closed=TRUE WHERE runner_id=$1 AND gone=FALSE`, resource.RunnerID); err != nil {
					return err
				}
				if _, err := tx.ExecContext(ctx, `DELETE FROM dune_managed_enrollments WHERE runner_id=$1`, resource.RunnerID); err != nil {
					return err
				}
				if _, err := tx.ExecContext(ctx, `DELETE FROM dune_machines WHERE runner_id=$1`, resource.RunnerID); err != nil {
					return err
				}
				resource.Gone, resource.AccessClosed = true, true
			} else {
				if _, err := tx.ExecContext(ctx, `UPDATE dune_managed_resources SET expires_at=$2 WHERE runner_id=$1 AND gone=FALSE AND access_closed=FALSE`, resource.RunnerID, inspection.ExpiresAt.UnixMilli()); err != nil {
					return err
				}
				resource.ExpiresAt = optionalTime(inspection.ExpiresAt.UnixMilli())
			}
		}

		var createFinished bool
		var createOutcome string
		if err := tx.QueryRowContext(ctx, `SELECT finished,outcome FROM dune_operations WHERE runner_id=$1 AND action='create'`, resource.RunnerID).Scan(&createFinished, &createOutcome); err != nil {
			return err
		}
		var bootstrapOutcome string
		err = tx.QueryRowContext(ctx, `SELECT outcome FROM dune_provider_actions WHERE operation_id=(SELECT operation_id FROM dune_managed_creations WHERE runner_id=$1) AND kind='bootstrap'`, resource.RunnerID).Scan(&bootstrapOutcome)
		if errors.Is(err, sql.ErrNoRows) {
			bootstrapOutcome = ""
		} else if err != nil {
			return err
		}
		var destroying, mutationPending bool
		if err := tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM dune_operations WHERE runner_id=$1 AND action='destroy'),EXISTS(SELECT 1 FROM dune_operations WHERE runner_id=$1 AND exclusive=TRUE) OR EXISTS(SELECT 1 FROM dune_provider_actions a JOIN dune_operations o ON o.id=a.operation_id WHERE o.runner_id=$1 AND a.completed_at=0)`, resource.RunnerID).Scan(&destroying, &mutationPending); err != nil {
			return err
		}
		state := lifecycle.RenewalState{
			Managed: true, ResourceRef: resource.Ref, ResourceConfirmedAt: resource.ConfirmedAt,
			EverReady: createFinished && createOutcome == "succeeded", BootstrapFailed: bootstrapOutcome == "failed",
			Destroying: destroying, MutationPending: mutationPending,
			FactsConfirmed: inspection.Status == lifecycle.InspectionConfirmed, ResourceGone: resource.Gone, ExpiresAt: resource.ExpiresAt,
		}
		decision, err := lifecycle.DecideRenewal(time.UnixMilli(now).UTC(), config, state)
		if err != nil {
			return fmt.Errorf("invalid renewal policy: %w", err)
		}
		next, renew := optionalMillis(decision.RecheckAt), optionalMillis(decision.Until)
		result, err := tx.ExecContext(ctx, `UPDATE dune_managed_maintenance SET policy_version=$2,reason=$3,facts=$4,observed_at=$5,next_check_at=$6,renew_until=$7,worker='',lease_until=0 WHERE runner_id=$1 AND worker=$8 AND execution_revision=$9 AND lease_until>`+s.databaseClock(), expected.RunnerID, policyVersion, decision.Reason, inspection.Status, now, next, renew, expected.Worker, expected.Revision)
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
		current.PolicyVersion, current.Reason, current.Facts = policyVersion, decision.Reason, inspection.Status
		current.ObservedAt, current.NextCheckAt, current.RenewUntil = time.UnixMilli(now).UTC(), optionalTime(next), optionalTime(renew)
		current.Worker, current.Until = "", time.Time{}
		saved = current
		return nil
	})
	if err != nil {
		return lifecycle.RenewalSchedule{}, err
	}
	return saved, nil
}

// ManagedRenewalSchedule is a trusted recovery/status read.
func (s *Store) ManagedRenewalSchedule(ctx context.Context, runnerID string) (lifecycle.RenewalSchedule, error) {
	return scanRenewalSchedule(s.db.QueryRowContext(ctx, "SELECT "+maintenanceColumns+" FROM dune_managed_maintenance WHERE runner_id=$1", runnerID))
}
