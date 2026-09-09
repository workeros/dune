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

	"github.com/aiomni/dune/internal/authorization"
	"github.com/aiomni/dune/internal/identity"
	"github.com/aiomni/dune/internal/lifecycle"
	"github.com/aiomni/dune/internal/wire"
	"github.com/aiomni/dune/pkg/runner"
)

const managedDestroyColumns = "operation_id,runner_id,resource_ref,machine_id,access_closed_at,close_deadline,access_close_outcome"
const managedDestroyClosureColumns = "operation_id,instance_id,machine_id,acknowledged_at"

func scanManagedDestroy(row interface{ Scan(...any) error }, operation lifecycle.Operation) (lifecycle.ManagedDestruction, error) {
	result := lifecycle.ManagedDestruction{Operation: operation}
	var closed, deadline int64
	err := row.Scan(&result.ID, &result.RunnerID, &result.ResourceRef, &result.MachineID, &closed, &deadline, &result.AccessCloseOutcome)
	result.AccessClosedAt, result.CloseDeadline = optionalTime(closed), optionalTime(deadline)
	if errors.Is(err, sql.ErrNoRows) {
		err = ErrNotFound
	}
	return result, err
}

func readManagedDestroy(ctx context.Context, db interface {
	QueryRowContext(context.Context, string, ...any) *sql.Row
}, operation lifecycle.Operation) (lifecycle.ManagedDestruction, error) {
	return scanManagedDestroy(db.QueryRowContext(ctx, "SELECT "+managedDestroyColumns+" FROM dune_managed_destroys WHERE operation_id=$1 AND runner_id=$2", operation.ID, operation.RunnerID), operation)
}

func destroyIntent(user identity.User, requestKey string, selected authorization.Resource, resource lifecycle.Resource, closeTimeout time.Duration) (lifecycle.Intent, error) {
	payload, err := json.Marshal(struct {
		RunnerID, FabricID, ResourceRef     string
		BindingRevision, CloseTimeoutMillis int64
	}{selected.Runner.ID, selected.FabricID, resource.Ref, selected.BindingRevision, closeTimeout.Milliseconds()})
	if err != nil {
		return lifecycle.Intent{}, err
	}
	sum := sha256.Sum256(payload)
	return lifecycle.Intent{
		ID: wire.ID(), RequestKey: requestKey, Digest: hex.EncodeToString(sum[:]),
		PrincipalID: user.ID, Namespace: user.Namespace, Subject: user.Subject,
		RunnerID: selected.Runner.ID, FabricID: selected.FabricID,
		BindingRevision: selected.BindingRevision, ProviderBindingID: resource.ProviderBindingID,
		ProviderBindingRevision: resource.ProviderBindingRevision, Action: "destroy",
	}, nil
}

// CreateManagedDestroy accepts one authorized human cleanup request and closes
// all database-backed access in the same transaction. Provider I/O and waiting
// for old streams happen only after this commit. An uncertain commit returns no
// operation; recovery must query the original actor and request key.
func (s *Store) CreateManagedDestroy(ctx context.Context, user identity.User, sessionHash, requestKey string, selected authorization.Resource, resource lifecycle.Resource, instanceID string, closeTimeout time.Duration) (lifecycle.ManagedDestruction, error) {
	if closeTimeout < 0 || closeTimeout > 10*time.Minute || sessionHash == "" || selected.Runner.ID == "" || selected.Runner.Kind != "managed" ||
		selected.OwnerID == "" || selected.FabricID == "" || selected.BindingRevision <= 0 || resource.RunnerID != selected.Runner.ID ||
		resource.FabricID != selected.FabricID || resource.Ref == "" || !wire.ValidID(instanceID) {
		return lifecycle.ManagedDestruction{}, ErrInvalidArgument
	}
	machineID := ""
	if selected.Runner.Binding != nil {
		binding := *selected.Runner.Binding
		if !binding.Valid() || binding.RunnerID != selected.Runner.ID || binding.FabricID != selected.FabricID || binding.Revision != selected.BindingRevision {
			return lifecycle.ManagedDestruction{}, ErrInvalidArgument
		}
		machineID = binding.MachineID
	}
	intent, err := destroyIntent(user, requestKey, selected, resource, closeTimeout)
	if err != nil || !validOperationIntent(intent) {
		if err != nil {
			return lifecycle.ManagedDestruction{}, err
		}
		return lifecycle.ManagedDestruction{}, ErrInvalidArgument
	}
	var result lifecycle.ManagedDestruction
	err = s.transaction(ctx, func(tx *sql.Tx) error {
		if err := s.lockPrincipal(ctx, tx, user.ID); err != nil {
			return err
		}
		if err := s.checkBrowserSession(ctx, tx, user.ID, sessionHash, user.Namespace, user.Subject); err != nil {
			return err
		}
		previous, err := scanOperation(tx.QueryRowContext(ctx, "SELECT "+operationColumns+" FROM dune_operations WHERE principal_id=$1 AND request_key=$2", user.ID, requestKey))
		if err == nil {
			expected := intent
			expected.ID = previous.ID
			if previous.Intent != expected {
				return lifecycle.ErrIntentConflict
			}
			result, err = readManagedDestroy(ctx, tx, previous)
			return err
		}
		if !errors.Is(err, ErrNotFound) {
			return err
		}
		fabricID, revision, err := s.lockOperationRunner(ctx, tx, selected.Runner.ID)
		if err != nil {
			return err
		}
		var kind, ownerID, currentMachine string
		if err := tx.QueryRowContext(ctx, `SELECT r.kind,r.owner_id,COALESCE(m.id,'') FROM dune_runners r LEFT JOIN dune_machines m ON m.runner_id=r.id WHERE r.id=$1`, selected.Runner.ID).Scan(&kind, &ownerID, &currentMachine); err != nil {
			return err
		}
		currentResource, err := scanManagedResource(tx.QueryRowContext(ctx, "SELECT "+resourceColumns+" FROM dune_managed_resources WHERE runner_id=$1", selected.Runner.ID))
		if err != nil {
			return err
		}
		if kind != "managed" || ownerID != selected.OwnerID || fabricID != selected.FabricID || revision != selected.BindingRevision || currentMachine != machineID ||
			currentResource.RunnerID != resource.RunnerID || currentResource.FabricID != resource.FabricID || currentResource.Ref != resource.Ref {
			return runner.ErrBindingChanged
		}
		var active string
		err = tx.QueryRowContext(ctx, `SELECT id FROM dune_operations WHERE runner_id=$1 AND exclusive=TRUE`, selected.Runner.ID).Scan(&active)
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
		finished, outcome, exclusive := currentResource.Gone, "", true
		if finished {
			outcome, exclusive = "succeeded", false
		}
		finishedAt := int64(0)
		if finished {
			finishedAt = now
		}
		if _, err := tx.ExecContext(ctx, `INSERT INTO dune_operations(id,request_key,request_digest,principal_id,identity_namespace,identity_subject,runner_id,fabric_id,binding_revision,provider_binding_id,provider_binding_revision,action,created_at,finished_at,finished,outcome,exclusive) VALUES($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,'destroy',$12,$13,$14,$15,$16)`, intent.ID, intent.RequestKey, intent.Digest, intent.PrincipalID, intent.Namespace, intent.Subject, intent.RunnerID, intent.FabricID, intent.BindingRevision, intent.ProviderBindingID, intent.ProviderBindingRevision, now, finishedAt, finished, outcome, exclusive); err != nil {
			return err
		}
		closeOutcome := lifecycle.AccessCloseConfirmed
		if machineID != "" && !finished {
			closeOutcome = lifecycle.AccessCloseWaiting
		}
		deadline := now + closeTimeout.Milliseconds()
		if _, err := tx.ExecContext(ctx, `INSERT INTO dune_managed_destroys(operation_id,runner_id,resource_ref,machine_id,access_closed_at,close_deadline,access_close_outcome) VALUES($1,$2,$3,$4,$5,$6,$7)`, intent.ID, intent.RunnerID, resource.Ref, machineID, now, deadline, closeOutcome); err != nil {
			return err
		}
		if closeOutcome == lifecycle.AccessCloseWaiting {
			if _, err := tx.ExecContext(ctx, `INSERT INTO dune_managed_access_closures(operation_id,instance_id,machine_id) VALUES($1,$2,$3)`, intent.ID, instanceID, machineID); err != nil {
				return err
			}
			if s.postgres {
				if _, err := tx.ExecContext(ctx, `INSERT INTO dune_managed_access_closures(operation_id,instance_id,machine_id)
					SELECT $1,boot_id,$2 FROM dune_instances WHERE expires_at>`+s.databaseClock()+`
					ON CONFLICT(operation_id,instance_id) DO NOTHING`, intent.ID, machineID); err != nil {
					return err
				}
			}
		}
		if _, err := tx.ExecContext(ctx, `UPDATE dune_managed_resources SET access_closed=TRUE WHERE runner_id=$1`, intent.RunnerID); err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, `UPDATE dune_managed_maintenance SET reason='DESTROYING',next_check_at=0,renew_until=0,worker='',lease_until=0 WHERE runner_id=$1`, intent.RunnerID); err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, `DELETE FROM dune_managed_enrollments WHERE runner_id=$1`, intent.RunnerID); err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, `DELETE FROM dune_machines WHERE runner_id=$1`, intent.RunnerID); err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, `UPDATE dune_operations SET finished=TRUE,finished_at=`+s.databaseClock()+`,outcome='failed',worker='',lease_until=0 WHERE runner_id=$1 AND action='create' AND finished=FALSE AND exclusive=FALSE`, intent.RunnerID); err != nil {
			return err
		}
		operation := lifecycle.Operation{Intent: intent, CreatedAt: time.UnixMilli(now).UTC(), FinishedAt: optionalTime(finishedAt), Finished: finished, Outcome: outcome, Exclusive: exclusive}
		result = lifecycle.ManagedDestruction{Operation: operation, ResourceRef: resource.Ref, MachineID: machineID, AccessClosedAt: time.UnixMilli(now).UTC(), CloseDeadline: time.UnixMilli(deadline).UTC(), AccessCloseOutcome: closeOutcome}
		return nil
	})
	if err != nil {
		return lifecycle.ManagedDestruction{}, err
	}
	return result, nil
}

func (s *Store) ManagedDestroy(ctx context.Context, principalID, requestKey string) (lifecycle.ManagedDestruction, error) {
	operation, err := scanOperation(s.db.QueryRowContext(ctx, "SELECT "+operationColumns+" FROM dune_operations WHERE principal_id=$1 AND request_key=$2 AND action='destroy'", principalID, requestKey))
	if err != nil {
		return lifecycle.ManagedDestruction{}, err
	}
	return readManagedDestroy(ctx, s.db, operation)
}

// ManagedDestroyOperation is a trusted worker recovery read.
func (s *Store) ManagedDestroyOperation(ctx context.Context, operationID string) (lifecycle.ManagedDestruction, error) {
	operation, err := s.Operation(ctx, operationID)
	if err != nil {
		return lifecycle.ManagedDestruction{}, err
	}
	if operation.Action != "destroy" {
		return lifecycle.ManagedDestruction{}, ErrNotFound
	}
	return readManagedDestroy(ctx, s.db, operation)
}

// RecoverableManagedDestroysFor lists accepted deletion workflows only after
// access closure is confirmed or their fixed wait deadline has elapsed.
func (s *Store) RecoverableManagedDestroysFor(ctx context.Context, fabricIDs []string, limit int) ([]lifecycle.Operation, error) {
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
	rows, err := s.db.QueryContext(ctx, `SELECT `+columns+`
		FROM dune_operations o
		JOIN dune_runners r ON r.id=o.runner_id
		JOIN dune_managed_resources resource ON resource.runner_id=o.runner_id
		JOIN dune_managed_destroys destroy ON destroy.operation_id=o.id AND destroy.runner_id=o.runner_id
		LEFT JOIN dune_provider_actions action ON action.operation_id=o.id AND action.kind='destroy'
		WHERE r.kind='managed' AND o.action='destroy' AND o.finished=FALSE AND o.exclusive=TRUE
			AND o.lease_until<=`+s.databaseClock()+` AND resource.fabric_id=o.fabric_id
			AND resource.resource_ref=destroy.resource_ref AND resource.access_closed=TRUE AND resource.gone=FALSE
			AND (destroy.access_close_outcome<>'waiting' OR destroy.close_deadline<=`+s.databaseClock()+`)
			AND (action.id IS NULL OR action.completed_at=0)`+filter+`
		ORDER BY o.created_at,o.id LIMIT $1`, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	operations := make([]lifecycle.Operation, 0, limit)
	for rows.Next() {
		operation, err := scanOperation(rows)
		if err != nil {
			return nil, err
		}
		operations = append(operations, operation)
	}
	return operations, rows.Err()
}

// ClaimRecoverableManagedDestroy repeats the access-close and resource checks
// under the Runner and Operation locks. A fixed deadline changes waiting to
// timed_out; it never claims that old streams acknowledged closure.
func (s *Store) ClaimRecoverableManagedDestroy(ctx context.Context, id, worker string, ttl time.Duration) (lifecycle.Operation, error) {
	return s.claimOperation(ctx, id, worker, ttl, func(tx *sql.Tx, operation lifecycle.Operation, now int64) error {
		if operation.Action != "destroy" || !operation.Exclusive {
			return lifecycle.ErrBusy
		}
		var resourceRef, destroyRef, outcome string
		var accessClosed, gone bool
		var deadline int64
		err := tx.QueryRowContext(ctx, `SELECT resource.resource_ref,resource.access_closed,resource.gone,destroy.resource_ref,destroy.close_deadline,destroy.access_close_outcome
			FROM dune_managed_resources resource JOIN dune_managed_destroys destroy ON destroy.runner_id=resource.runner_id
			WHERE destroy.operation_id=$1 AND destroy.runner_id=$2`, operation.ID, operation.RunnerID).Scan(&resourceRef, &accessClosed, &gone, &destroyRef, &deadline, &outcome)
		if err != nil {
			return err
		}
		if !accessClosed || gone || resourceRef == "" || resourceRef != destroyRef {
			return lifecycle.ErrBusy
		}
		if outcome == lifecycle.AccessCloseWaiting {
			if deadline > now {
				return lifecycle.ErrBusy
			}
			if _, err := tx.ExecContext(ctx, `UPDATE dune_managed_destroys SET access_close_outcome=$2 WHERE operation_id=$1 AND access_close_outcome=$3`, operation.ID, lifecycle.AccessCloseTimedOut, lifecycle.AccessCloseWaiting); err != nil {
				return err
			}
		} else if outcome != lifecycle.AccessCloseConfirmed && outcome != lifecycle.AccessCloseTimedOut {
			return lifecycle.ErrBusy
		}
		var completed int64
		err = tx.QueryRowContext(ctx, `SELECT completed_at FROM dune_provider_actions WHERE operation_id=$1 AND kind='destroy'`, operation.ID).Scan(&completed)
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

// PendingManagedAccessClosures returns durable pause or destroy close requests
// addressed to one exact application incarnation. New instances are absent
// because they could not have accepted access before the transaction closed it.
func (s *Store) PendingManagedAccessClosures(ctx context.Context, instanceID string, limit int) ([]lifecycle.AccessClosure, error) {
	if !wire.ValidID(instanceID) || limit < 1 || limit > 32 {
		return nil, ErrInvalidArgument
	}
	rows, err := s.db.QueryContext(ctx, `SELECT c.operation_id,c.instance_id,c.machine_id,o.binding_revision,c.acknowledged_at
		FROM dune_managed_access_closures c
		LEFT JOIN dune_managed_destroys d ON d.operation_id=c.operation_id
		JOIN dune_operations o ON o.id=c.operation_id
		JOIN dune_managed_resources resource ON resource.runner_id=o.runner_id
		WHERE c.instance_id=$1 AND c.acknowledged_at=0 AND
			((o.action='pause' AND resource.access_suspended=TRUE) OR (o.action='destroy' AND d.access_close_outcome=$2 AND o.finished=FALSE AND o.exclusive=TRUE))
		ORDER BY o.created_at,c.operation_id LIMIT $3`, instanceID, lifecycle.AccessCloseWaiting, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	closures := make([]lifecycle.AccessClosure, 0, limit)
	for rows.Next() {
		var closure lifecycle.AccessClosure
		var acknowledged int64
		if err := rows.Scan(&closure.OperationID, &closure.InstanceID, &closure.MachineID, &closure.BindingRevision, &acknowledged); err != nil {
			return nil, err
		}
		closure.AcknowledgedAt = optionalTime(acknowledged)
		closures = append(closures, closure)
	}
	return closures, rows.Err()
}

// ConfirmManagedAccessClosed records one trusted Gateway observation. Destroy's
// overall close result becomes confirmed only after every instance live at
// acceptance has acknowledged the exact old machine binding.
func (s *Store) ConfirmManagedAccessClosed(ctx context.Context, expected lifecycle.AccessClosure) error {
	if !wire.ValidID(expected.OperationID) || !wire.ValidID(expected.InstanceID) || !wire.ValidID(expected.MachineID) || expected.BindingRevision <= 0 || !expected.AcknowledgedAt.IsZero() {
		return ErrInvalidArgument
	}
	return s.withOperation(ctx, expected.OperationID, func(tx *sql.Tx, operation lifecycle.Operation, now int64) error {
		if (operation.Action != "destroy" && operation.Action != "pause") || operation.BindingRevision != expected.BindingRevision {
			return lifecycle.ErrBusy
		}
		var currentMachine string
		var acknowledged int64
		if err := tx.QueryRowContext(ctx, `SELECT machine_id,acknowledged_at FROM dune_managed_access_closures WHERE operation_id=$1 AND instance_id=$2`, expected.OperationID, expected.InstanceID).Scan(&currentMachine, &acknowledged); err != nil {
			return err
		}
		if currentMachine != expected.MachineID {
			return lifecycle.ErrBusy
		}
		if acknowledged != 0 {
			return nil
		}
		result, err := tx.ExecContext(ctx, `UPDATE dune_managed_access_closures SET acknowledged_at=$3 WHERE operation_id=$1 AND instance_id=$2 AND acknowledged_at=0`, expected.OperationID, expected.InstanceID, now)
		if err != nil {
			return err
		}
		n, err := result.RowsAffected()
		if err != nil {
			return err
		}
		if n != 1 {
			return lifecycle.ErrBusy
		}
		if operation.Action == "pause" {
			return nil
		}
		var outcome string
		if err := tx.QueryRowContext(ctx, `SELECT access_close_outcome FROM dune_managed_destroys WHERE operation_id=$1 AND runner_id=$2`, operation.ID, operation.RunnerID).Scan(&outcome); err != nil {
			return err
		}
		if operation.Finished || !operation.Exclusive || outcome != lifecycle.AccessCloseWaiting {
			return lifecycle.ErrBusy
		}
		var pending int
		if err := tx.QueryRowContext(ctx, `SELECT count(*) FROM dune_managed_access_closures WHERE operation_id=$1 AND acknowledged_at=0`, expected.OperationID).Scan(&pending); err != nil {
			return err
		}
		if pending != 0 {
			return nil
		}
		result, err = tx.ExecContext(ctx, `UPDATE dune_managed_destroys SET access_close_outcome=$2 WHERE operation_id=$1 AND access_close_outcome=$3`, expected.OperationID, lifecycle.AccessCloseConfirmed, lifecycle.AccessCloseWaiting)
		if err != nil {
			return err
		}
		n, err = result.RowsAffected()
		if err == nil && n != 1 {
			return lifecycle.ErrBusy
		}
		return err
	})
}
