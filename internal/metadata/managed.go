package metadata

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/aiomni/dune/internal/identity"
	"github.com/aiomni/dune/internal/lifecycle"
	"github.com/aiomni/dune/internal/wire"
)

// CreateManaged atomically records an authorized human creation, its new Runner
// and immutable template snapshot. The caller must check current fabric/template
// access and validate public parameter fields; this transaction rechecks the
// exact browser identity/session while holding the actor lock. No provider I/O
// is permitted before a confirmed commit. On ErrCommitUnknown, inspect the
// original actor/request key through ManagedCreation; never resubmit a provider
// Create merely because this method returned an error.
func (s *Store) CreateManaged(ctx context.Context, user identity.User, sessionHash, requestKey string, spec lifecycle.CreateSpec) (lifecycle.Creation, error) {
	encoded, err := spec.Encode()
	if err != nil {
		return lifecycle.Creation{}, fmt.Errorf("%w: %v", ErrInvalidArgument, err)
	}
	sum := sha256.Sum256([]byte(encoded))
	intent := lifecycle.Intent{ID: wire.ID(), RunnerID: wire.ID(), PrincipalID: user.ID, Namespace: user.Namespace, Subject: user.Subject, RequestKey: requestKey, Digest: hex.EncodeToString(sum[:]), FabricID: spec.FabricID, BindingRevision: 1, Action: "create"}
	if sessionHash == "" || !validOperationIntent(intent) {
		return lifecycle.Creation{}, ErrInvalidArgument
	}
	var result lifecycle.Creation
	err = s.transaction(ctx, func(tx *sql.Tx) error {
		if err := s.lockPrincipal(ctx, tx, user.ID); err != nil {
			return err
		}
		if err := s.checkBrowserSession(ctx, tx, user.ID, sessionHash, user.Namespace, user.Subject); err != nil {
			return err
		}
		previous, err := scanOperation(tx.QueryRowContext(ctx, "SELECT "+operationColumns+" FROM dune_operations WHERE principal_id=$1 AND request_key=$2", user.ID, requestKey))
		if err == nil {
			if previous.Action != "create" || previous.Namespace != user.Namespace || previous.Subject != user.Subject || previous.Digest != intent.Digest {
				return lifecycle.ErrIntentConflict
			}
			result, err = readManagedCreation(ctx, tx, previous)
			return err
		}
		if !errors.Is(err, ErrNotFound) {
			return err
		}
		var count int
		if err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM dune_runners WHERE owner_id=$1`, user.ID).Scan(&count); err != nil {
			return err
		}
		if count >= 32 {
			return fmt.Errorf("%w: runner limit reached (32 per account)", ErrInvalidArgument)
		}
		now, err := s.databaseNow(ctx, tx)
		if err != nil {
			return err
		}
		var frozen lifecycle.CreateSpec
		if err := json.Unmarshal([]byte(encoded), &frozen); err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, `INSERT INTO dune_runners(id,owner_id,name,kind,fabric_id,binding_revision,created_at) VALUES($1,$2,$3,'managed',$4,1,$5)`, intent.RunnerID, user.ID, frozen.Name, frozen.FabricID, now/1000); err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, `INSERT INTO dune_operations(id,request_key,request_digest,principal_id,identity_namespace,identity_subject,runner_id,fabric_id,binding_revision,action,created_at) VALUES($1,$2,$3,$4,$5,$6,$7,$8,1,'create',$9)`, intent.ID, requestKey, intent.Digest, user.ID, user.Namespace, user.Subject, intent.RunnerID, frozen.FabricID, now); err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, `INSERT INTO dune_managed_creations(runner_id,operation_id,specification) VALUES($1,$2,$3)`, intent.RunnerID, intent.ID, encoded); err != nil {
			return err
		}
		result, err = readManagedCreation(ctx, tx, lifecycle.Operation{Intent: intent, CreatedAt: time.UnixMilli(now).UTC(), Exclusive: true})
		return err
	})
	if err != nil {
		return lifecycle.Creation{}, err
	}
	return result, nil
}

type creationReader interface {
	QueryRowContext(context.Context, string, ...any) *sql.Row
}

func readManagedCreation(ctx context.Context, db creationReader, op lifecycle.Operation) (lifecycle.Creation, error) {
	var encoded string
	err := db.QueryRowContext(ctx, `SELECT specification FROM dune_managed_creations WHERE runner_id=$1 AND operation_id=$2`, op.RunnerID, op.ID).Scan(&encoded)
	if errors.Is(err, sql.ErrNoRows) {
		return lifecycle.Creation{}, ErrNotFound
	}
	if err != nil {
		return lifecycle.Creation{}, err
	}
	var spec lifecycle.CreateSpec
	if err := json.Unmarshal([]byte(encoded), &spec); err != nil {
		return lifecycle.Creation{}, err
	}
	canonical, err := spec.Encode()
	sum := sha256.Sum256([]byte(encoded))
	if err != nil || canonical != encoded || hex.EncodeToString(sum[:]) != op.Digest || spec.FabricID != op.FabricID || op.Action != "create" {
		return lifecycle.Creation{}, fmt.Errorf("invalid managed creation snapshot")
	}
	r, err := scanRunner(db.QueryRowContext(ctx, runnerSelect+` WHERE r.id=$1 AND r.owner_id=$2 AND r.kind='managed'`, op.RunnerID, op.PrincipalID))
	if errors.Is(err, sql.ErrNoRows) {
		err = ErrNotFound
	}
	return lifecycle.Creation{Runner: r, Operation: op, Spec: spec}, err
}

// ManagedCreation is a trusted recovery read by the original actor/request key.
// A user-facing caller must authenticate and authorize access to the resulting
// Runner/operation. Missing records do not prove that an external call never ran.
func (s *Store) ManagedCreation(ctx context.Context, principalID, requestKey string) (lifecycle.Creation, error) {
	var result lifecycle.Creation
	err := s.transaction(ctx, func(tx *sql.Tx) error {
		op, err := scanOperation(tx.QueryRowContext(ctx, "SELECT "+operationColumns+" FROM dune_operations WHERE principal_id=$1 AND request_key=$2", principalID, requestKey))
		if err != nil {
			return err
		}
		result, err = readManagedCreation(ctx, tx, op)
		return err
	})
	if err != nil {
		return lifecycle.Creation{}, err
	}
	return result, nil
}
