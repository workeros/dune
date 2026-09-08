package metadata

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"time"

	"github.com/aiomni/dune/internal/authorization"
	"github.com/aiomni/dune/internal/identity"
	"github.com/aiomni/dune/internal/lifecycle"
	"github.com/aiomni/dune/internal/wire"
	"github.com/aiomni/dune/pkg/runner"
)

const managedDestroyColumns = "operation_id,runner_id,resource_ref,machine_id,access_closed_at,close_deadline,access_close_outcome"

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
		BindingRevision: selected.BindingRevision, Action: "destroy",
	}, nil
}

// CreateManagedDestroy accepts one authorized human cleanup request and closes
// all database-backed access in the same transaction. Provider I/O and waiting
// for old streams happen only after this commit. An uncertain commit returns no
// operation; recovery must query the original actor and request key.
func (s *Store) CreateManagedDestroy(ctx context.Context, user identity.User, sessionHash, requestKey string, selected authorization.Resource, resource lifecycle.Resource, closeTimeout time.Duration) (lifecycle.ManagedDestruction, error) {
	if closeTimeout < 0 || closeTimeout > 10*time.Minute || sessionHash == "" || selected.Runner.ID == "" || selected.Runner.Kind != "managed" ||
		selected.OwnerID == "" || selected.FabricID == "" || selected.BindingRevision <= 0 || resource.RunnerID != selected.Runner.ID ||
		resource.FabricID != selected.FabricID || resource.Ref == "" {
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
		if _, err := tx.ExecContext(ctx, `INSERT INTO dune_operations(id,request_key,request_digest,principal_id,identity_namespace,identity_subject,runner_id,fabric_id,binding_revision,action,created_at,finished,outcome,exclusive) VALUES($1,$2,$3,$4,$5,$6,$7,$8,$9,'destroy',$10,$11,$12,$13)`, intent.ID, intent.RequestKey, intent.Digest, intent.PrincipalID, intent.Namespace, intent.Subject, intent.RunnerID, intent.FabricID, intent.BindingRevision, now, finished, outcome, exclusive); err != nil {
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
		if _, err := tx.ExecContext(ctx, `UPDATE dune_operations SET finished=TRUE,outcome='failed',worker='',lease_until=0 WHERE runner_id=$1 AND action='create' AND finished=FALSE AND exclusive=FALSE`, intent.RunnerID); err != nil {
			return err
		}
		operation := lifecycle.Operation{Intent: intent, CreatedAt: time.UnixMilli(now).UTC(), Finished: finished, Outcome: outcome, Exclusive: exclusive}
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
