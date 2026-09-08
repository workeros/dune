package metadata

import (
	"context"
	"time"
)

// ManagedStatus is a database-clock snapshot of lifecycle work that operators
// may need to investigate. Counts are global to the shared metadata store and
// contain no actor, resource or provider-specific values.
type ManagedStatus struct {
	CheckedAt           time.Time
	Runners             int64
	KnownResources      int64
	AccessibleResources int64
	UnknownOperations   int64
	TimedOutOperations  int64
	RenewalBacklog      int64
	ExpiryRiskResources int64
	ResidualResources   int64
}

// ManagedStatusSnapshot reads one consistent statement-level view. Renewal
// backlog includes live resources with no current policy decision, a changed
// policy version, a due inspection, or a frozen renewal decision. Expiry risk
// uses the caller's configured renewal lead time. A residual resource is still
// present after access closed, or is unbound while its lifecycle is unresolved
// or failed. Provider reachability is deliberately not inferred.
func (s *Store) ManagedStatusSnapshot(ctx context.Context, policyVersion string, expiryRiskWindow time.Duration) (ManagedStatus, error) {
	if !validPolicyVersion(policyVersion) || expiryRiskWindow.Milliseconds() <= 0 {
		return ManagedStatus{}, ErrInvalidArgument
	}
	var status ManagedStatus
	var checkedAt int64
	err := s.db.QueryRowContext(ctx, `WITH snapshot(now) AS (SELECT `+s.databaseClock()+`)
		SELECT snapshot.now,
			(SELECT COUNT(*) FROM dune_runners runner WHERE runner.kind='managed'),
			(SELECT COUNT(*) FROM dune_managed_resources),
			(SELECT COUNT(*) FROM dune_managed_resources resource WHERE resource.gone=FALSE AND resource.access_closed=FALSE),
			(SELECT COUNT(*) FROM dune_operations operation WHERE operation.finished=FALSE AND operation.outcome='unknown'),
			(SELECT COUNT(*) FROM dune_operations operation WHERE operation.finished=FALSE AND operation.outcome='timed_out'),
			(SELECT COUNT(*) FROM dune_managed_resources resource
				LEFT JOIN dune_managed_maintenance maintenance ON maintenance.runner_id=resource.runner_id
				WHERE resource.gone=FALSE AND resource.access_closed=FALSE AND
					(maintenance.runner_id IS NULL OR maintenance.policy_version<>$1 OR maintenance.reason='' OR maintenance.renew_until>0 OR
						(maintenance.renew_until=0 AND maintenance.next_check_at>0 AND maintenance.next_check_at<=snapshot.now))),
			(SELECT COUNT(*) FROM dune_managed_resources resource WHERE resource.gone=FALSE AND resource.access_closed=FALSE AND
				resource.expires_at>0 AND resource.expires_at<=snapshot.now+$2),
			(SELECT COUNT(*) FROM dune_managed_resources resource WHERE resource.gone=FALSE AND
				(resource.access_closed=TRUE OR (NOT EXISTS(SELECT 1 FROM dune_machines machine WHERE machine.runner_id=resource.runner_id) AND
					EXISTS(SELECT 1 FROM dune_operations operation
						LEFT JOIN dune_provider_actions action ON action.operation_id=operation.id
						WHERE operation.runner_id=resource.runner_id AND
							(operation.outcome IN ('unknown','timed_out') OR (operation.finished=TRUE AND operation.outcome='failed') OR action.outcome IN ('unknown','timed_out','failed'))))))
		FROM snapshot`, policyVersion, expiryRiskWindow.Milliseconds()).Scan(
		&checkedAt, &status.Runners, &status.KnownResources, &status.AccessibleResources,
		&status.UnknownOperations, &status.TimedOutOperations, &status.RenewalBacklog,
		&status.ExpiryRiskResources, &status.ResidualResources,
	)
	if err != nil {
		return ManagedStatus{}, err
	}
	status.CheckedAt = time.UnixMilli(checkedAt).UTC()
	return status, nil
}
