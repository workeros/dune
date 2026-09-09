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
	"github.com/aiomni/dune/pkg/fabric"
	"github.com/aiomni/dune/pkg/runner"
)

func pauseResumeIntent(user identity.User, requestKey, action string, selected authorization.Resource, resource lifecycle.Resource) (lifecycle.Intent, error) {
	payload, err := json.Marshal(struct {
		RunnerID, FabricID, ResourceRef, Action, ProviderBindingID string
		BindingRevision, ProviderBindingRevision                   int64
	}{selected.Runner.ID, selected.FabricID, resource.Ref, action, resource.ProviderBindingID, selected.BindingRevision, resource.ProviderBindingRevision})
	if err != nil {
		return lifecycle.Intent{}, err
	}
	sum := sha256.Sum256(payload)
	return lifecycle.Intent{
		ID: wire.ID(), RequestKey: requestKey, Digest: hex.EncodeToString(sum[:]), PrincipalID: user.ID, Namespace: user.Namespace, Subject: user.Subject,
		RunnerID: selected.Runner.ID, FabricID: selected.FabricID, BindingRevision: selected.BindingRevision,
		ProviderBindingID: resource.ProviderBindingID, ProviderBindingRevision: resource.ProviderBindingRevision, Action: action,
	}, nil
}

func (s *Store) CreateManagedPauseResume(ctx context.Context, user identity.User, sessionHash, requestKey string, selected authorization.Resource, resource lifecycle.Resource, instanceID, action string) (lifecycle.ManagedPauseResume, error) {
	if action != "pause" && action != "resume" {
		return lifecycle.ManagedPauseResume{}, ErrInvalidArgument
	}
	if sessionHash == "" || !wire.ValidID(instanceID) || selected.Runner.ID == "" || selected.Runner.Kind != "managed" || selected.OwnerID == "" ||
		selected.FabricID == "" || selected.BindingRevision <= 0 || resource.RunnerID != selected.Runner.ID || resource.FabricID != selected.FabricID ||
		resource.ProviderBindingID == "" || resource.ProviderBindingRevision <= 0 || resource.Ref == "" || resource.Capabilities == nil || !resource.Capabilities.PauseResume {
		return lifecycle.ManagedPauseResume{}, ErrInvalidArgument
	}
	intent, err := pauseResumeIntent(user, requestKey, action, selected, resource)
	if err != nil || !validOperationIntent(intent) {
		return lifecycle.ManagedPauseResume{}, ErrInvalidArgument
	}
	var result lifecycle.ManagedPauseResume
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
			result.Operation = previous
			return tx.QueryRowContext(ctx, `SELECT COALESCE(m.id,'') FROM dune_runners r LEFT JOIN dune_machines m ON m.runner_id=r.id WHERE r.id=$1`, previous.RunnerID).Scan(&result.MachineID)
		}
		if !errors.Is(err, ErrNotFound) {
			return err
		}
		fabricID, revision, err := s.lockOperationRunner(ctx, tx, selected.Runner.ID)
		if err != nil {
			return err
		}
		current, err := scanManagedResource(tx.QueryRowContext(ctx, "SELECT "+resourceColumns+" FROM dune_managed_resources WHERE runner_id=$1", selected.Runner.ID))
		if err != nil {
			return err
		}
		if fabricID != selected.FabricID || revision != selected.BindingRevision || current.ProviderBindingID != resource.ProviderBindingID ||
			current.ProviderBindingRevision != resource.ProviderBindingRevision || current.Ref != resource.Ref || current.Gone || current.AccessClosed ||
			current.Capabilities == nil || !current.Capabilities.PauseResume {
			return runner.ErrBindingChanged
		}
		if action == "pause" && (current.State != string(fabric.ResourceReady) || current.AccessSuspended) {
			return lifecycle.ErrBusy
		}
		if action == "resume" && (current.State != string(fabric.ResourcePaused) || !current.AccessSuspended) {
			return lifecycle.ErrBusy
		}
		var active string
		if err := tx.QueryRowContext(ctx, `SELECT id FROM dune_operations WHERE runner_id=$1 AND exclusive=TRUE`, current.RunnerID).Scan(&active); err == nil {
			return lifecycle.ErrBusy
		} else if !errors.Is(err, sql.ErrNoRows) {
			return err
		}
		now, err := s.databaseNow(ctx, tx)
		if err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, `INSERT INTO dune_operations(id,request_key,request_digest,principal_id,identity_namespace,identity_subject,runner_id,fabric_id,binding_revision,provider_binding_id,provider_binding_revision,action,created_at) VALUES($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13)`,
			intent.ID, intent.RequestKey, intent.Digest, intent.PrincipalID, intent.Namespace, intent.Subject, intent.RunnerID, intent.FabricID, intent.BindingRevision, intent.ProviderBindingID, intent.ProviderBindingRevision, intent.Action, now); err != nil {
			return err
		}
		if err := tx.QueryRowContext(ctx, `SELECT COALESCE(m.id,'') FROM dune_runners r LEFT JOIN dune_machines m ON m.runner_id=r.id WHERE r.id=$1`, current.RunnerID).Scan(&result.MachineID); err != nil {
			return err
		}
		if action == "pause" {
			if _, err := tx.ExecContext(ctx, `UPDATE dune_managed_resources SET access_suspended=TRUE WHERE runner_id=$1`, current.RunnerID); err != nil {
				return err
			}
			if _, err := tx.ExecContext(ctx, `DELETE FROM dune_access_tickets WHERE runner_id=$1`, current.RunnerID); err != nil {
				return err
			}
			if result.MachineID != "" {
				if _, err := tx.ExecContext(ctx, `INSERT INTO dune_managed_access_closures(operation_id,instance_id,machine_id) VALUES($1,$2,$3)`, intent.ID, instanceID, result.MachineID); err != nil {
					return err
				}
				if s.postgres {
					if _, err := tx.ExecContext(ctx, `INSERT INTO dune_managed_access_closures(operation_id,instance_id,machine_id)
						SELECT $1,boot_id,$2 FROM dune_instances WHERE expires_at>`+s.databaseClock()+`
						ON CONFLICT(operation_id,instance_id) DO NOTHING`, intent.ID, result.MachineID); err != nil {
						return err
					}
				}
			}
		}
		result.Operation = lifecycle.Operation{Intent: intent, CreatedAt: time.UnixMilli(now).UTC(), Exclusive: true}
		return nil
	})
	if err != nil {
		return lifecycle.ManagedPauseResume{}, err
	}
	return result, nil
}

func (s *Store) RecoverableManagedPauseResumesFor(ctx context.Context, fabricIDs []string, limit int) ([]lifecycle.Operation, error) {
	if limit < 1 || limit > 32 {
		return nil, ErrInvalidArgument
	}
	if len(fabricIDs) == 0 {
		return []lifecycle.Operation{}, nil
	}
	filter, args, err := managedFabricFilter(fabricIDs)
	if err != nil {
		return nil, err
	}
	columns := "o." + strings.ReplaceAll(operationColumns, ",", ",o.")
	rows, err := s.db.QueryContext(ctx, `SELECT `+columns+` FROM dune_operations o LEFT JOIN dune_provider_actions a ON a.operation_id=o.id AND a.kind=o.action WHERE o.action IN ('pause','resume') AND o.finished=FALSE AND o.exclusive=TRUE AND o.lease_until<=`+s.databaseClock()+` AND (a.id IS NULL OR a.completed_at=0)`+filter+` ORDER BY o.created_at,o.id LIMIT $1`, append([]any{limit}, args...)...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var operations []lifecycle.Operation
	for rows.Next() {
		op, err := scanOperation(rows)
		if err != nil {
			return nil, err
		}
		operations = append(operations, op)
	}
	return operations, rows.Err()
}
