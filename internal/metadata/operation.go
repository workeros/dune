package metadata

import (
	"context"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/aiomni/dune/internal/lifecycle"
	"github.com/aiomni/dune/pkg/runner"
)

const operationColumns = "id,request_key,request_digest,principal_id,identity_namespace,identity_subject,runner_id,fabric_id,binding_revision,action,created_at,finished,outcome,worker,execution_revision,lease_until,exclusive"

func validOperationIntent(i lifecycle.Intent) bool {
	for _, field := range []struct {
		s   string
		max int
	}{
		{i.ID, 128}, {i.RequestKey, 128}, {i.PrincipalID, 128}, {i.RunnerID, 128}, {i.FabricID, 128},
	} {
		if field.s == "" || !utf8.ValidString(field.s) || len(field.s) > field.max || strings.ContainsFunc(field.s, unicode.IsControl) {
			return false
		}
	}
	for _, field := range []struct {
		s   string
		max int
	}{{i.Namespace, 2048}, {i.Subject, 512}} {
		if !utf8.ValidString(field.s) || len(field.s) > field.max || strings.ContainsFunc(field.s, unicode.IsControl) {
			return false
		}
	}
	if (i.Namespace == "") != (i.Subject == "") || i.BindingRevision <= 0 {
		return false
	}
	if len(i.Digest) != 64 {
		return false
	}
	digest, err := hex.DecodeString(i.Digest)
	if err != nil || len(digest) != 32 || i.Digest != strings.ToLower(i.Digest) {
		return false
	}
	return i.Action == "create" || i.Action == "renew" || i.Action == "destroy"
}

func scanOperation(row interface{ Scan(...any) error }) (lifecycle.Operation, error) {
	var op lifecycle.Operation
	var created, until int64
	err := row.Scan(&op.ID, &op.RequestKey, &op.Digest, &op.PrincipalID, &op.Namespace, &op.Subject, &op.RunnerID, &op.FabricID, &op.BindingRevision, &op.Action, &created, &op.Finished, &op.Outcome, &op.Worker, &op.Revision, &until, &op.Exclusive)
	op.CreatedAt = time.UnixMilli(created).UTC()
	if until != 0 {
		op.Until = time.UnixMilli(until).UTC()
	}
	if errors.Is(err, sql.ErrNoRows) {
		err = ErrNotFound
	}
	return op, err
}

// databaseNow uses the database clock after acquiring locks. Replicas cannot
// gain execution time by supplying their own timestamps or waiting on a row lock.
func (s *Store) databaseClock() string {
	if s.postgres {
		return `CAST(EXTRACT(EPOCH FROM clock_timestamp())*1000 AS BIGINT)`
	}
	return `CAST((julianday('now')-2440587.5)*86400000 AS INTEGER)`
}
func (s *Store) databaseNow(ctx context.Context, tx *sql.Tx) (int64, error) {
	var now int64
	err := tx.QueryRowContext(ctx, "SELECT "+s.databaseClock()).Scan(&now)
	return now, err
}

func (s *Store) lockOperationRunner(ctx context.Context, tx *sql.Tx, id string) (string, int64, error) {
	query := `SELECT fabric_id,binding_revision FROM dune_runners WHERE id=$1`
	if s.postgres {
		query += ` FOR UPDATE`
	}
	var fabric string
	var revision int64
	err := tx.QueryRowContext(ctx, query, id).Scan(&fabric, &revision)
	if errors.Is(err, sql.ErrNoRows) {
		err = ErrNotFound
	}
	return fabric, revision, err
}

// BeginOperation is an internal transaction boundary for an existing Runner.
// Product entry points must authenticate and authorize before invoking it. New
// Runner creation and destroy access restrictions must join this transaction in
// their lifecycle-specific entry point; this primitive alone does neither.
func (s *Store) BeginOperation(ctx context.Context, intent lifecycle.Intent) (lifecycle.Operation, error) {
	if !validOperationIntent(intent) {
		return lifecycle.Operation{}, ErrInvalidArgument
	}
	var result lifecycle.Operation
	err := s.transaction(ctx, func(tx *sql.Tx) error {
		// The actor lock serializes duplicate request keys even across different
		// Runners. Its lifetime is unrelated to a browser session or a worker lease.
		actorQuery := `SELECT id FROM dune_principals WHERE id=$1`
		if s.postgres {
			actorQuery += ` FOR UPDATE`
		}
		var actor string
		if err := tx.QueryRowContext(ctx, actorQuery, intent.PrincipalID).Scan(&actor); err != nil {
			if errors.Is(err, sql.ErrNoRows) {
				return ErrNotFound
			}
			return err
		}
		previous, err := scanOperation(tx.QueryRowContext(ctx, "SELECT "+operationColumns+" FROM dune_operations WHERE principal_id=$1 AND request_key=$2", intent.PrincipalID, intent.RequestKey))
		if err == nil {
			expected := intent
			expected.ID = previous.ID
			if previous.Intent != expected {
				return lifecycle.ErrIntentConflict
			}
			result = previous
			return nil
		}
		if !errors.Is(err, ErrNotFound) {
			return err
		}
		fabric, revision, err := s.lockOperationRunner(ctx, tx, intent.RunnerID)
		if err != nil {
			return err
		}
		if fabric != intent.FabricID || revision != intent.BindingRevision {
			return runner.ErrBindingChanged
		}
		var active string
		err = tx.QueryRowContext(ctx, `SELECT id FROM dune_operations WHERE runner_id=$1 AND exclusive=TRUE`, intent.RunnerID).Scan(&active)
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
		_, err = tx.ExecContext(ctx, `INSERT INTO dune_operations(id,request_key,request_digest,principal_id,identity_namespace,identity_subject,runner_id,fabric_id,binding_revision,action,created_at) VALUES($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11)`, intent.ID, intent.RequestKey, intent.Digest, intent.PrincipalID, intent.Namespace, intent.Subject, intent.RunnerID, intent.FabricID, intent.BindingRevision, intent.Action, now)
		result = lifecycle.Operation{Intent: intent, CreatedAt: time.UnixMilli(now).UTC(), Exclusive: true}
		return err
	})
	if err != nil {
		return lifecycle.Operation{}, err
	}
	return result, nil
}

// Operation is for trusted workers and recovery. User-facing discovery must
// resolve the recorded actor/resource and perform its own access check.
func (s *Store) Operation(ctx context.Context, id string) (lifecycle.Operation, error) {
	return scanOperation(s.db.QueryRowContext(ctx, "SELECT "+operationColumns+" FROM dune_operations WHERE id=$1", id))
}

// RecoverableManagedCreates returns trusted create-stage work whose execution
// lease has expired according to the database clock. It excludes operations
// that have already advanced past create, even though those workflows remain
// unfinished while bootstrap or the first connection is pending.
func (s *Store) RecoverableManagedCreates(ctx context.Context, limit int) ([]lifecycle.Operation, error) {
	return s.recoverableManagedCreates(ctx, nil, limit)
}

// RecoverableManagedCreatesFor limits recovery to configured Fabric namespaces.
func (s *Store) RecoverableManagedCreatesFor(ctx context.Context, fabricIDs []string, limit int) ([]lifecycle.Operation, error) {
	if limit < 1 || limit > 32 {
		return nil, ErrInvalidArgument
	}
	if len(fabricIDs) == 0 {
		return []lifecycle.Operation{}, nil
	}
	return s.recoverableManagedCreates(ctx, fabricIDs, limit)
}

func managedFabricFilter(fabricIDs []string) (string, []any, error) {
	return managedFabricFilterAt(fabricIDs, 2)
}

func managedFabricFilterAt(fabricIDs []string, first int) (string, []any, error) {
	return managedFabricFilterColumn(fabricIDs, first, "o.fabric_id")
}

func managedFabricFilterColumn(fabricIDs []string, first int, column string) (string, []any, error) {
	placeholders := make([]string, len(fabricIDs))
	args := make([]any, len(fabricIDs))
	for i, id := range fabricIDs {
		if id == "" || id == "attached" || len(id) > 128 || !utf8.ValidString(id) || strings.TrimSpace(id) != id || strings.ContainsFunc(id, unicode.IsControl) {
			return "", nil, ErrInvalidArgument
		}
		placeholders[i] = fmt.Sprintf("$%d", i+first)
		args[i] = id
	}
	if len(placeholders) == 0 {
		return "", nil, nil
	}
	return " AND " + column + " IN (" + strings.Join(placeholders, ",") + ")", args, nil
}

func (s *Store) recoverableManagedCreates(ctx context.Context, fabricIDs []string, limit int) ([]lifecycle.Operation, error) {
	if limit < 1 || limit > 32 {
		return nil, ErrInvalidArgument
	}
	fabricFilter, fabricArgs, err := managedFabricFilter(fabricIDs)
	if err != nil {
		return nil, err
	}
	columns := "o." + strings.ReplaceAll(operationColumns, ",", ",o.")
	args := append([]any{limit}, fabricArgs...)
	rows, err := s.db.QueryContext(ctx, `SELECT `+columns+`
		FROM dune_operations o
		JOIN dune_runners r ON r.id=o.runner_id
		LEFT JOIN dune_provider_actions a ON a.operation_id=o.id AND a.kind='create'
		WHERE r.kind='managed' AND o.action='create' AND o.finished=FALSE
			AND o.exclusive=TRUE AND o.lease_until<=`+s.databaseClock()+`
			AND (a.id IS NULL OR a.completed_at=0)`+fabricFilter+`
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

// RecoverableManagedBootstraps returns create workflows whose resource is
// confirmed but whose Bootstrap action is absent or unresolved. The resource
// checks use the database clock so an expired target is never scheduled merely
// because one worker's local clock differs.
func (s *Store) RecoverableManagedBootstraps(ctx context.Context, limit int) ([]lifecycle.Operation, error) {
	return s.recoverableManagedBootstraps(ctx, nil, limit)
}

// RecoverableManagedBootstrapsFor limits recovery to configured Bootstrap
// adapters, preventing older unsupported namespaces from filling the batch.
func (s *Store) RecoverableManagedBootstrapsFor(ctx context.Context, fabricIDs []string, limit int) ([]lifecycle.Operation, error) {
	if limit < 1 || limit > 32 {
		return nil, ErrInvalidArgument
	}
	if len(fabricIDs) == 0 {
		return []lifecycle.Operation{}, nil
	}
	return s.recoverableManagedBootstraps(ctx, fabricIDs, limit)
}

func (s *Store) recoverableManagedBootstraps(ctx context.Context, fabricIDs []string, limit int) ([]lifecycle.Operation, error) {
	if limit < 1 || limit > 32 {
		return nil, ErrInvalidArgument
	}
	fabricFilter, fabricArgs, err := managedFabricFilter(fabricIDs)
	if err != nil {
		return nil, err
	}
	columns := "o." + strings.ReplaceAll(operationColumns, ",", ",o.")
	args := append([]any{limit}, fabricArgs...)
	rows, err := s.db.QueryContext(ctx, `SELECT `+columns+`
		FROM dune_operations o
		JOIN dune_runners r ON r.id=o.runner_id
		JOIN dune_provider_actions created ON created.operation_id=o.id AND created.kind='create'
		JOIN dune_managed_resources resource ON resource.runner_id=o.runner_id AND resource.fabric_id=o.fabric_id
		LEFT JOIN dune_provider_actions bootstrap ON bootstrap.operation_id=o.id AND bootstrap.kind='bootstrap'
		WHERE r.kind='managed' AND o.action='create' AND o.finished=FALSE
			AND o.exclusive=TRUE AND o.lease_until<=`+s.databaseClock()+`
			AND created.completed_at>0 AND created.outcome='succeeded'
			AND resource.gone=FALSE AND resource.access_closed=FALSE
			AND (resource.expires_at=0 OR resource.expires_at>`+s.databaseClock()+`)
			AND (bootstrap.id IS NULL OR bootstrap.completed_at=0)`+fabricFilter+`
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

func validWorker(worker string) bool {
	if len(worker) != 32 {
		return false
	}
	data, err := hex.DecodeString(worker)
	return err == nil && len(data) == 16 && worker == strings.ToLower(worker)
}
func validLeaseDuration(ttl time.Duration) bool { return ttl >= time.Second && ttl <= time.Minute }

// ClaimOperation only takes the right to follow up on the original operation.
// It must not be used as permission to repeat an unresolved external mutation.
func (s *Store) ClaimOperation(ctx context.Context, id, worker string, ttl time.Duration) (lifecycle.Operation, error) {
	return s.claimOperation(ctx, id, worker, ttl, nil)
}

// ClaimRecoverableManagedCreate conditionally claims the create stage observed
// by RecoverableManagedCreates. The condition is repeated under row locks so a
// stale scan cannot reclaim a workflow that has already advanced to bootstrap.
func (s *Store) ClaimRecoverableManagedCreate(ctx context.Context, id, worker string, ttl time.Duration) (lifecycle.Operation, error) {
	return s.claimOperation(ctx, id, worker, ttl, func(tx *sql.Tx, op lifecycle.Operation, _ int64) error {
		if op.Action != "create" || !op.Exclusive {
			return lifecycle.ErrBusy
		}
		var completed int64
		err := tx.QueryRowContext(ctx, `SELECT completed_at FROM dune_provider_actions WHERE operation_id=$1 AND kind='create'`, op.ID).Scan(&completed)
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

// ClaimRecoverableManagedBootstrap repeats the Bootstrap-stage checks while
// holding the Runner and Operation locks. This prevents a stale recovery scan
// from claiming a workflow after its resource or action has changed.
func (s *Store) ClaimRecoverableManagedBootstrap(ctx context.Context, id, worker string, ttl time.Duration) (lifecycle.Operation, error) {
	return s.claimOperation(ctx, id, worker, ttl, func(tx *sql.Tx, op lifecycle.Operation, now int64) error {
		if op.Action != "create" || !op.Exclusive {
			return lifecycle.ErrBusy
		}
		var createdCompleted, expires int64
		var createdOutcome, fabricID, resourceRef string
		var gone, accessClosed bool
		err := tx.QueryRowContext(ctx, `SELECT a.completed_at,a.outcome,r.fabric_id,r.resource_ref,r.expires_at,r.gone,r.access_closed
			FROM dune_provider_actions a JOIN dune_managed_resources r ON r.runner_id=$2
			WHERE a.operation_id=$1 AND a.kind='create'`, op.ID, op.RunnerID).Scan(&createdCompleted, &createdOutcome, &fabricID, &resourceRef, &expires, &gone, &accessClosed)
		if errors.Is(err, sql.ErrNoRows) {
			return lifecycle.ErrBusy
		}
		if err != nil {
			return err
		}
		if createdCompleted == 0 || createdOutcome != "succeeded" || fabricID != op.FabricID || resourceRef == "" || gone || accessClosed || (expires != 0 && expires <= now) {
			return lifecycle.ErrBusy
		}
		var bootstrapCompleted int64
		err = tx.QueryRowContext(ctx, `SELECT completed_at FROM dune_provider_actions WHERE operation_id=$1 AND kind='bootstrap'`, op.ID).Scan(&bootstrapCompleted)
		if errors.Is(err, sql.ErrNoRows) {
			return nil
		}
		if err != nil {
			return err
		}
		if bootstrapCompleted != 0 {
			return lifecycle.ErrBusy
		}
		return nil
	})
}

func (s *Store) claimOperation(ctx context.Context, id, worker string, ttl time.Duration, eligible func(*sql.Tx, lifecycle.Operation, int64) error) (lifecycle.Operation, error) {
	if !validWorker(worker) || !validLeaseDuration(ttl) {
		return lifecycle.Operation{}, ErrInvalidArgument
	}
	var result lifecycle.Operation
	err := s.withOperation(ctx, id, func(tx *sql.Tx, op lifecycle.Operation, now int64) error {
		if op.Finished || op.Until.UnixMilli() > now {
			return lifecycle.ErrBusy
		}
		if eligible != nil {
			if err := eligible(tx, op, now); err != nil {
				return err
			}
		}
		var until int64
		err := tx.QueryRowContext(ctx, "UPDATE dune_operations SET worker=$2,execution_revision=execution_revision+1,lease_until="+s.databaseClock()+"+$3 WHERE id=$1 AND execution_revision<9223372036854775807 RETURNING execution_revision,lease_until,exclusive", id, worker, ttl.Milliseconds()).Scan(&op.Revision, &until, &op.Exclusive)
		if errors.Is(err, sql.ErrNoRows) {
			return lifecycle.ErrLeaseLost
		}
		if err != nil {
			return err
		}
		op.Worker = worker
		op.Until = time.UnixMilli(until).UTC()
		result = op
		return nil
	})
	if err != nil {
		return lifecycle.Operation{}, err
	}
	return result, nil
}

// withOperation serializes binding changes and updates in Runner -> Operation
// order. The immutable Runner reference may be read before acquiring its lock.
func (s *Store) withOperation(ctx context.Context, id string, change func(*sql.Tx, lifecycle.Operation, int64) error) error {
	return s.transaction(ctx, func(tx *sql.Tx) error {
		var runnerID string
		if err := tx.QueryRowContext(ctx, `SELECT runner_id FROM dune_operations WHERE id=$1`, id).Scan(&runnerID); err != nil {
			if errors.Is(err, sql.ErrNoRows) {
				return ErrNotFound
			}
			return err
		}
		fabric, revision, err := s.lockOperationRunner(ctx, tx, runnerID)
		if err != nil {
			return err
		}
		query := "SELECT " + operationColumns + " FROM dune_operations WHERE id=$1"
		if s.postgres {
			query += ` FOR UPDATE`
		}
		op, err := scanOperation(tx.QueryRowContext(ctx, query, id))
		if err != nil {
			return err
		}
		if fabric != op.FabricID || revision != op.BindingRevision {
			return runner.ErrBindingChanged
		}
		now, err := s.databaseNow(ctx, tx)
		if err != nil {
			return err
		}
		return change(tx, op, now)
	})
}

func ownsOperation(current, expected lifecycle.Operation, now int64) bool {
	return !current.Finished && current.Intent == expected.Intent && current.Worker == expected.Worker && current.Revision == expected.Revision && current.Until.UnixMilli() > now
}

func (s *Store) RenewOperationLease(ctx context.Context, expected lifecycle.Operation, ttl time.Duration) (lifecycle.Operation, error) {
	if !validLeaseDuration(ttl) {
		return lifecycle.Operation{}, ErrInvalidArgument
	}
	var result lifecycle.Operation
	err := s.withOperation(ctx, expected.ID, func(tx *sql.Tx, current lifecycle.Operation, now int64) error {
		if !ownsOperation(current, expected, now) {
			return lifecycle.ErrLeaseLost
		}
		// Refresh cannot shorten an existing lease when a caller changes its TTL.
		var until int64
		clock := s.databaseClock()
		err := tx.QueryRowContext(ctx, "UPDATE dune_operations SET lease_until=CASE WHEN lease_until>"+clock+"+$2 THEN lease_until ELSE "+clock+"+$2 END WHERE id=$1 AND worker=$3 AND execution_revision=$4 AND lease_until>"+clock+" RETURNING lease_until", expected.ID, ttl.Milliseconds(), expected.Worker, expected.Revision).Scan(&until)
		if errors.Is(err, sql.ErrNoRows) {
			return lifecycle.ErrLeaseLost
		}
		current.Until = time.UnixMilli(until).UTC()
		result = current
		return err
	})
	if err != nil {
		return lifecycle.Operation{}, err
	}
	return result, nil
}

// YieldOperationLease makes unfinished work immediately claimable without
// releasing its business mutex or changing an uncertain outcome. A finished
// operation is already unclaimed and is treated as successfully yielded.
func (s *Store) YieldOperationLease(ctx context.Context, expected lifecycle.Operation) error {
	return s.withOperation(ctx, expected.ID, func(tx *sql.Tx, current lifecycle.Operation, now int64) error {
		if current.Finished {
			return nil
		}
		if !ownsOperation(current, expected, now) {
			return lifecycle.ErrLeaseLost
		}
		result, err := tx.ExecContext(ctx, `UPDATE dune_operations SET worker='',lease_until=0 WHERE id=$1 AND worker=$2 AND execution_revision=$3 AND lease_until>`+s.databaseClock(), expected.ID, expected.Worker, expected.Revision)
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
	})
}

// FinishOperation requires authoritative terminal evidence from the lifecycle
// executor. Unknown, timeouts, cancellation and lease expiry are not terminal
// evidence and cannot release the Runner business lock through this method.
func (s *Store) FinishOperation(ctx context.Context, expected lifecycle.Operation, outcome string) error {
	if outcome != "succeeded" && outcome != "failed" {
		return ErrInvalidArgument
	}
	return s.withOperation(ctx, expected.ID, func(tx *sql.Tx, current lifecycle.Operation, now int64) error {
		if !ownsOperation(current, expected, now) {
			return lifecycle.ErrLeaseLost
		}
		if err := s.requireNoPendingAction(ctx, tx, current.ID); err != nil {
			return err
		}
		return s.updateOperationOutcome(ctx, tx, expected, outcome, true)
	})
}

// RecordOperationUncertainty keeps the business lock and the original request.
// A timeout is recorded distinctly from an unclassified unknown outcome.
func (s *Store) RecordOperationUncertainty(ctx context.Context, expected lifecycle.Operation, outcome string) error {
	if outcome != "unknown" && outcome != "timed_out" {
		return ErrInvalidArgument
	}
	return s.withOperation(ctx, expected.ID, func(tx *sql.Tx, current lifecycle.Operation, now int64) error {
		if !ownsOperation(current, expected, now) {
			return lifecycle.ErrLeaseLost
		}
		if !current.Exclusive {
			return lifecycle.ErrBusy
		}
		return s.updateOperationOutcome(ctx, tx, expected, outcome, false)
	})
}

func (s *Store) updateOperationOutcome(ctx context.Context, tx *sql.Tx, expected lifecycle.Operation, outcome string, finished bool) error {
	result, err := tx.ExecContext(ctx, "UPDATE dune_operations SET outcome=$2,finished=$3,exclusive=CASE WHEN $3 THEN FALSE ELSE exclusive END,worker=CASE WHEN $3 THEN '' ELSE worker END,lease_until=CASE WHEN $3 THEN 0 ELSE lease_until END WHERE id=$1 AND worker=$4 AND execution_revision=$5 AND lease_until>"+s.databaseClock(), expected.ID, outcome, finished, expected.Worker, expected.Revision)
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

// ReleaseOperationMutex permits nonconflicting maintenance while an unfinished
// workflow observes readiness. Its caller must have confirmed the external
// stage has ended. Uncertain mutations cannot be released by this primitive;
// their later resolution must commit the verified stage facts atomically.
func (s *Store) ReleaseOperationMutex(ctx context.Context, expected lifecycle.Operation) error {
	return s.operationMutex(ctx, expected, false)
}

// AcquireOperationMutex is required before a new external mutation in an
// unfinished workflow. Claiming or refreshing the worker lease does not do it.
func (s *Store) AcquireOperationMutex(ctx context.Context, expected lifecycle.Operation) error {
	return s.operationMutex(ctx, expected, true)
}
func (s *Store) operationMutex(ctx context.Context, expected lifecycle.Operation, exclusive bool) error {
	return s.withOperation(ctx, expected.ID, func(tx *sql.Tx, current lifecycle.Operation, now int64) error {
		if !ownsOperation(current, expected, now) {
			return lifecycle.ErrLeaseLost
		}
		if !exclusive && current.Outcome != "" {
			return lifecycle.ErrBusy
		}
		if !exclusive {
			if err := s.requireNoPendingAction(ctx, tx, current.ID); err != nil {
				return err
			}
		}
		if exclusive {
			var competing string
			err := tx.QueryRowContext(ctx, `SELECT id FROM dune_operations WHERE runner_id=$1 AND exclusive=TRUE AND id<>$2`, current.RunnerID, current.ID).Scan(&competing)
			if err == nil {
				return lifecycle.ErrBusy
			}
			if !errors.Is(err, sql.ErrNoRows) {
				return err
			}
		}
		return s.updateOperationMutex(ctx, tx, expected, exclusive)
	})
}

func (s *Store) updateOperationMutex(ctx context.Context, tx *sql.Tx, expected lifecycle.Operation, exclusive bool) error {
	result, err := tx.ExecContext(ctx, "UPDATE dune_operations SET exclusive=$2 WHERE id=$1 AND worker=$3 AND execution_revision=$4 AND lease_until>"+s.databaseClock(), expected.ID, exclusive, expected.Worker, expected.Revision)
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
