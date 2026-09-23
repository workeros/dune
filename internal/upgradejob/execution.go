package upgradejob

import (
	"context"
	"encoding/json"
	"fmt"
	"reflect"
	"strconv"
	"time"

	"github.com/aiomni/dune/internal/wire"
	"github.com/aiomni/dune/pkg/api"
	"github.com/aiomni/dune/pkg/upgrade"
)

// Claim fences a previous executor. The caller first obtains the installation
// OS lock; holding only this token never authorizes physical side effects.
func (s *Store) Claim(ctx context.Context, operationID string) (Record, error) {
	return s.change(ctx, operationID, "", "", func(record *Record) error {
		if record.Operation.Admission != api.SubmissionAccepted || (record.Operation.Confirmed && !record.Operation.LaunchSealed) {
			return fmt.Errorf("upgrade has no unfinished execution")
		}
		record.Owner = wire.ID()
		return nil
	}, "claim")
}

// Update changes one checkpoint only for the current owner/revision. It cannot
// alter the admitted request, selected release, source facts or confirmed result.
func (s *Store) Update(ctx context.Context, operationID, owner, revision string, update func(*Record) error) (Record, error) {
	if owner == "" || revision == "" || update == nil {
		return Record{}, fmt.Errorf("current execution owner and revision required")
	}
	return s.change(ctx, operationID, owner, revision, update, "checkpoint")
}

// ReleaseSeal finishes terminal cleanup after the owner removed the physical
// seal. Until this checkpoint the operation still excludes another upgrade.
func (s *Store) ReleaseSeal(ctx context.Context, operationID, owner, revision string) (Record, error) {
	return s.change(ctx, operationID, owner, revision, func(record *Record) error {
		if !record.Operation.Confirmed || !record.Operation.LaunchSealed {
			return fmt.Errorf("sealed terminal result required")
		}
		record.Operation.LaunchSealed = false
		return nil
	}, "release")
}

func (s *Store) change(ctx context.Context, id, owner, revision string, update func(*Record) error, action string) (Record, error) {
	var empty Record
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return empty, err
	}
	defer tx.Rollback()
	before, _, err := scan(tx.QueryRowContext(ctx, `SELECT data,digest FROM upgrades WHERE operation_id=?`, id))
	if err != nil {
		return empty, err
	}
	if (before.Operation.Confirmed && action == "checkpoint") || (action != "claim" && (owner == "" || before.Owner != owner || before.Operation.Revision != revision)) {
		return empty, &api.Error{Code: "UPGRADE_OWNER_CHANGED", Detail: "executor or operation revision is no longer current"}
	}
	// Decode a detached copy so callback edits cannot mutate the immutable facts
	// used to validate this transition through shared pointer/slice storage.
	body, err := encode(before)
	if err != nil {
		return empty, err
	}
	var after Record
	if err := json.Unmarshal(body, &after); err != nil {
		return empty, err
	}
	if err := update(&after); err != nil {
		return empty, err
	}
	if action == "checkpoint" {
		if err := validateTransition(before, after); err != nil {
			return empty, err
		}
	}
	after.Operation.Revision, err = nextRevision(before.Operation.Revision)
	if err != nil {
		return empty, err
	}
	after.Operation.UpdatedAt = time.Now().UTC()
	body, err = encode(after)
	if err != nil {
		return empty, err
	}
	_, err = tx.ExecContext(ctx, `UPDATE upgrades SET data=?,revision=revision+1,owner=?,active=? WHERE operation_id=?`, body, after.Owner, activeValue(after.Operation), id)
	if err != nil {
		return empty, err
	}
	if err := tx.Commit(); err != nil {
		return empty, err
	}
	return after, nil
}

func validateTransition(before, after Record) error {
	a, b := before.Operation, after.Operation
	if after.Owner != before.Owner || b.ID != a.ID || b.Request != a.Request || !reflect.DeepEqual(b.Target, a.Target) || !reflect.DeepEqual(b.Source, a.Source) || !reflect.DeepEqual(b.Plan, a.Plan) || b.StateContract != a.StateContract || b.Admission != a.Admission || b.StartedAt != a.StartedAt || after.Deadline != before.Deadline {
		return fmt.Errorf("execution attempted to replace admitted upgrade facts")
	}
	if a.Failure != nil && !reflect.DeepEqual(a.Failure, b.Failure) {
		return fmt.Errorf("original upgrade failure must be retained")
	}
	if (a.LaunchSealed && !b.LaunchSealed) || (before.Switched && !after.Switched) {
		return fmt.Errorf("execution cannot erase switch or launch-seal evidence")
	}
	if b.Phase == upgrade.Switching && (!b.LaunchSealed || api.ValidateSubmissionID(b.AttemptID) != nil || api.ValidateSubmissionID(b.Challenge) != nil || b.AttemptStartedAt.IsZero()) {
		return fmt.Errorf("switch requires a persistent seal and fixed target attempt")
	}
	if !beforeSwitch(a.Phase) && b.Phase != upgrade.RollingBack && b.AttemptID != a.AttemptID {
		return fmt.Errorf("execution cannot replace the current verification attempt")
	}
	if a.AttemptID != "" && a.AttemptID == b.AttemptID && !a.AttemptStartedAt.Equal(b.AttemptStartedAt) {
		return fmt.Errorf("attempt start evidence cannot change")
	}
	if !phaseAllowed(a.Phase, b.Phase) {
		return fmt.Errorf("invalid upgrade transition from %s to %s", a.Phase, b.Phase)
	}
	if b.Phase == upgrade.RollingBack && a.Phase != upgrade.RollingBack {
		if b.AttemptID == "" || b.AttemptID == a.AttemptID || b.Failure == nil || b.Rollback != upgrade.RollbackRunning {
			return fmt.Errorf("rollback requires its own attempt and original failure")
		}
	}
	if !before.RollbackDeadline.IsZero() && !before.RollbackDeadline.Equal(after.RollbackDeadline) {
		return fmt.Errorf("automatic recovery cannot extend its rollback budget")
	}
	if b.Phase == upgrade.Succeeded {
		if b.Rollback != upgrade.RollbackNotNeeded || !b.Confirmed || !b.LaunchSealed || b.Proof == nil {
			return fmt.Errorf("upgrade success requires complete target proof")
		}
		return validateProof(b, false)
	}
	if b.Phase == upgrade.Failed {
		if !b.Confirmed || b.Failure == nil {
			return fmt.Errorf("terminal failure requires durable reason")
		}
		if a.Phase == upgrade.RollbackVerifying {
			if b.Rollback != upgrade.RollbackRestored {
				return fmt.Errorf("restored rollback proof required")
			}
			return validateProof(b, true)
		}
		if before.Switched || !beforeSwitch(a.Phase) || b.Rollback != upgrade.RollbackNotNeeded {
			return fmt.Errorf("post-switch failure must complete or block recovery")
		}
		return nil
	}
	if b.Confirmed {
		return fmt.Errorf("unfinished upgrade cannot become confirmed")
	}
	if b.Phase == upgrade.RecoveryBlocked && (!b.LaunchSealed || b.RollbackFailure == nil) {
		return fmt.Errorf("blocked recovery must retain its seal and diagnostic")
	}
	return nil
}

func beforeSwitch(phase upgrade.Phase) bool {
	return phase == upgrade.Queued || phase == upgrade.Preparing || phase == upgrade.Downloading || phase == upgrade.Checking
}

func phaseAllowed(from, to upgrade.Phase) bool {
	if from == to {
		return true
	}
	if beforeSwitch(from) && to == upgrade.Failed {
		return true
	}
	if to == upgrade.RecoveryBlocked {
		return true
	}
	switch from {
	case upgrade.Queued:
		return to == upgrade.Preparing
	case upgrade.Preparing:
		return to == upgrade.Downloading
	case upgrade.Downloading:
		return to == upgrade.Checking
	case upgrade.Checking:
		return to == upgrade.Switching
	case upgrade.Switching:
		return to == upgrade.Reconnecting || to == upgrade.Verifying || to == upgrade.RollingBack
	case upgrade.Reconnecting:
		return to == upgrade.Verifying || to == upgrade.RollingBack
	case upgrade.Verifying:
		return to == upgrade.Succeeded || to == upgrade.RollingBack
	case upgrade.RollingBack:
		return to == upgrade.RollbackVerifying
	case upgrade.RollbackVerifying:
		return to == upgrade.Failed
	case upgrade.RecoveryBlocked:
		return to == upgrade.RollingBack || to == upgrade.RollbackVerifying
	}
	return false
}

func validateProof(operation upgrade.Operation, rollback bool) error {
	proof := operation.Proof
	if proof == nil || proof.OperationID != operation.ID || proof.AttemptID != operation.AttemptID || proof.Challenge != operation.Challenge || api.ValidateSubmissionID(proof.AttemptID) != nil || api.ValidateSubmissionID(proof.Challenge) != nil || proof.Binding != operation.Request.Binding || proof.InstallationID != operation.Request.InstallationID {
		return fmt.Errorf("platform proof does not match current operation attempt")
	}
	if !proof.ReleaseVerified || !proof.GatewayAccepted || !proof.Routed || proof.Running.PID <= 1 || proof.Running.StartID == "" || proof.Incarnation == "" || proof.ConnectionGeneration == 0 {
		return fmt.Errorf("platform proof lacks installation, running process or accepted routed connection")
	}
	if proof.ObservedAt.Before(operation.AttemptStartedAt) || time.Since(proof.ObservedAt) > 30*time.Second || proof.ObservedAt.After(time.Now().Add(time.Second)) {
		return fmt.Errorf("platform proof is stale")
	}
	expected := operation.Target
	if rollback {
		if operation.Source.Installation == nil {
			return fmt.Errorf("rollback source evidence unavailable")
		}
		expected = operation.Source.Installation.Release
	} else if operation.Plan.ConnectorRestartRequired && proof.Running.StartID == operation.Source.Running.StartID {
		return fmt.Errorf("connector restart was not proved")
	}
	digest, err := expected.Digest()
	if err != nil {
		return err
	}
	if proof.ManifestSHA256 != digest || proof.Running.SHA256 != expected.ProgramSHA256() {
		return fmt.Errorf("platform proof identifies another distribution")
	}
	var programBytes int64
	for _, component := range expected.Components {
		if component.Path == "dune" {
			programBytes = component.Bytes
		}
	}
	if proof.Running.Bytes != programBytes || proof.Running.Build.OS != expected.Platform.OS || proof.Running.Build.Arch != expected.Platform.Arch {
		return fmt.Errorf("running image does not match target platform or size")
	}
	revision, err := strconv.ParseUint(proof.InstallationRevision, 10, 64)
	if err != nil || revision == 0 || strconv.FormatUint(revision, 10) != proof.InstallationRevision {
		return fmt.Errorf("platform proof lacks canonical installation revision")
	}
	return nil
}
