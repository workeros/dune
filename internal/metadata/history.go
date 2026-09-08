package metadata

import (
	"context"
	"database/sql"
	"fmt"
	"time"
)

const (
	minManagedHistoryRetention = 24 * time.Hour
	maxManagedHistoryRetention = 366 * 24 * time.Hour
)

type ManagedHistoryCleanup struct {
	Operations int64
	Runners    int64
}

func scanIDs(rows *sql.Rows) ([]string, error) {
	defer rows.Close()
	var ids []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		ids = append(ids, id)
	}
	return ids, rows.Err()
}

// PruneManagedHistory removes terminal lifecycle history after its configured
// recovery window. It never removes unfinished or uncertain work, resources
// that may still exist, current bindings, pending enrollment inputs, or recent
// idempotency records. Callers use the database clock by supplying a duration,
// rather than comparing a host timestamp with shared records.
func (s *Store) PruneManagedHistory(ctx context.Context, retainFor time.Duration, limit int) (ManagedHistoryCleanup, error) {
	if retainFor < minManagedHistoryRetention || retainFor > maxManagedHistoryRetention || limit < 1 || limit > 32 {
		return ManagedHistoryCleanup{}, ErrInvalidArgument
	}
	var cleanup ManagedHistoryCleanup
	err := s.transaction(ctx, func(tx *sql.Tx) error {
		now, err := s.databaseNow(ctx, tx)
		if err != nil {
			return err
		}
		cutoff := now - retainFor.Milliseconds()
		lock := ""
		if s.postgres {
			lock = " FOR UPDATE OF o SKIP LOCKED"
		}
		rows, err := tx.QueryContext(ctx, `SELECT o.id FROM dune_operations o
			WHERE o.action='renew' AND o.finished=TRUE AND o.finished_at>0 AND o.finished_at<=$1
			AND NOT EXISTS(SELECT 1 FROM dune_provider_actions a WHERE a.operation_id=o.id AND (a.completed_at=0 OR a.completed_at>$1))
			AND NOT EXISTS(SELECT 1 FROM dune_managed_reviews v WHERE v.operation_id=o.id AND (v.completed_at=0 OR v.completed_at>$1 OR v.outcome IN ('unknown','timed_out')))
			ORDER BY o.finished_at,o.id LIMIT $2`+lock, cutoff, limit)
		if err != nil {
			return err
		}
		renewals, err := scanIDs(rows)
		if err != nil {
			return err
		}
		for _, id := range renewals {
			if _, err := tx.ExecContext(ctx, `DELETE FROM dune_managed_reviews WHERE operation_id=$1`, id); err != nil {
				return err
			}
			if _, err := tx.ExecContext(ctx, `DELETE FROM dune_provider_actions WHERE operation_id=$1`, id); err != nil {
				return err
			}
			if _, err := tx.ExecContext(ctx, `DELETE FROM dune_managed_renewals WHERE operation_id=$1`, id); err != nil {
				return err
			}
			result, err := tx.ExecContext(ctx, `DELETE FROM dune_operations WHERE id=$1 AND action='renew' AND finished=TRUE AND finished_at>0 AND finished_at<=$2`, id, cutoff)
			if err != nil {
				return err
			}
			removed, err := result.RowsAffected()
			if err != nil {
				return err
			}
			cleanup.Operations += removed
		}
		remaining := int64(limit) - cleanup.Operations
		if remaining <= 0 {
			return nil
		}

		lock = ""
		if s.postgres {
			lock = " FOR UPDATE OF r SKIP LOCKED"
		}
		rows, err = tx.QueryContext(ctx, `SELECT r.id FROM dune_runners r
			LEFT JOIN dune_managed_resources resource ON resource.runner_id=r.id
			WHERE r.kind='managed'
			AND ((resource.runner_id IS NOT NULL AND resource.gone=TRUE AND resource.access_closed=TRUE)
				OR (resource.runner_id IS NULL AND EXISTS(SELECT 1 FROM dune_operations failed_create WHERE failed_create.runner_id=r.id AND failed_create.action='create' AND failed_create.finished=TRUE AND failed_create.outcome='failed')))
			AND NOT EXISTS(SELECT 1 FROM dune_machines m WHERE m.runner_id=r.id)
			AND NOT EXISTS(SELECT 1 FROM dune_managed_enrollments e WHERE e.runner_id=r.id)
			AND NOT EXISTS(SELECT 1 FROM dune_managed_destroys d WHERE d.runner_id=r.id AND d.access_close_outcome<>'confirmed')
			AND EXISTS(SELECT 1 FROM dune_operations present WHERE present.runner_id=r.id)
			AND NOT EXISTS(SELECT 1 FROM dune_operations pending WHERE pending.runner_id=r.id AND (pending.finished=FALSE OR pending.finished_at=0 OR pending.finished_at>$1))
			AND NOT EXISTS(SELECT 1 FROM dune_provider_actions a JOIN dune_operations o ON o.id=a.operation_id WHERE o.runner_id=r.id AND (a.completed_at=0 OR a.completed_at>$1))
			AND NOT EXISTS(SELECT 1 FROM dune_managed_reviews v JOIN dune_operations o ON o.id=v.operation_id WHERE o.runner_id=r.id AND (v.completed_at=0 OR v.completed_at>$1 OR v.outcome IN ('unknown','timed_out')))
			AND (SELECT COUNT(*) FROM dune_operations bounded WHERE bounded.runner_id=r.id)<=$2
			ORDER BY r.created_at,r.id LIMIT $2`+lock, cutoff, remaining)
		if err != nil {
			return err
		}
		runners, err := scanIDs(rows)
		if err != nil {
			return err
		}
		for _, id := range runners {
			var operationCount int64
			if err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM dune_operations WHERE runner_id=$1`, id).Scan(&operationCount); err != nil {
				return err
			}
			if operationCount < 1 || operationCount > remaining {
				continue
			}
			if _, err := tx.ExecContext(ctx, `DELETE FROM dune_managed_reviews WHERE operation_id IN (SELECT id FROM dune_operations WHERE runner_id=$1)`, id); err != nil {
				return err
			}
			if _, err := tx.ExecContext(ctx, `DELETE FROM dune_provider_actions WHERE operation_id IN (SELECT id FROM dune_operations WHERE runner_id=$1)`, id); err != nil {
				return err
			}
			if _, err := tx.ExecContext(ctx, `DELETE FROM dune_managed_destroy_closures WHERE operation_id IN (SELECT operation_id FROM dune_managed_destroys WHERE runner_id=$1)`, id); err != nil {
				return err
			}
			if _, err := tx.ExecContext(ctx, `DELETE FROM dune_managed_destroys WHERE runner_id=$1`, id); err != nil {
				return err
			}
			if _, err := tx.ExecContext(ctx, `DELETE FROM dune_managed_renewals WHERE runner_id=$1`, id); err != nil {
				return err
			}
			if _, err := tx.ExecContext(ctx, `DELETE FROM dune_managed_creations WHERE runner_id=$1`, id); err != nil {
				return err
			}
			result, err := tx.ExecContext(ctx, `DELETE FROM dune_operations WHERE runner_id=$1 AND finished=TRUE AND finished_at>0 AND finished_at<=$2`, id, cutoff)
			if err != nil {
				return err
			}
			removed, err := result.RowsAffected()
			if err != nil {
				return err
			}
			if removed != operationCount {
				return fmt.Errorf("managed history cleanup operation count changed under Runner lock")
			}
			cleanup.Operations += removed
			remaining -= removed
			if _, err := tx.ExecContext(ctx, `DELETE FROM dune_managed_maintenance WHERE runner_id=$1`, id); err != nil {
				return err
			}
			if _, err := tx.ExecContext(ctx, `DELETE FROM dune_managed_resources WHERE runner_id=$1 AND gone=TRUE AND access_closed=TRUE`, id); err != nil {
				return err
			}
			result, err = tx.ExecContext(ctx, `DELETE FROM dune_runners WHERE id=$1 AND kind='managed'
				AND NOT EXISTS(SELECT 1 FROM dune_machines m WHERE m.runner_id=$1)
				AND NOT EXISTS(SELECT 1 FROM dune_managed_enrollments e WHERE e.runner_id=$1)
				AND NOT EXISTS(SELECT 1 FROM dune_operations o WHERE o.runner_id=$1)
				AND NOT EXISTS(SELECT 1 FROM dune_managed_resources resource WHERE resource.runner_id=$1)`, id)
			if err != nil {
				return err
			}
			removed, err = result.RowsAffected()
			if err != nil {
				return err
			}
			if removed != 1 {
				return fmt.Errorf("managed history cleanup lost Runner lock")
			}
			cleanup.Runners++
			if remaining == 0 {
				break
			}
		}
		return nil
	})
	if err != nil {
		return ManagedHistoryCleanup{}, err
	}
	return cleanup, nil
}
