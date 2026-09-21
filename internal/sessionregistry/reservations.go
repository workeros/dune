package sessionregistry

import (
	"context"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/aiomni/dune/pkg/api"
)

const (
	ControlPermission = "permission"
	ControlCancel     = "cancel"
	ControlStop       = "stop"
	ControlForget     = "forget"
)

func encodeTarget(target api.SubmissionTarget) string {
	body, _ := json.Marshal(target)
	return string(body)
}

func controlResource(target api.SubmissionTarget, kind, id string) (string, error) {
	if target.Validate() != nil || target.RuntimeID == "" {
		return "", &api.Error{Code: "INVALID_ARGUMENT", Detail: "control requires a complete Runtime target"}
	}
	switch kind {
	case ControlStop, ControlForget:
		if id != "" {
			return "", fmt.Errorf("Runtime controls do not take a secondary target")
		}
	case ControlCancel, ControlPermission:
		if api.ValidateSubmissionID(id) != nil {
			return "", fmt.Errorf("control requires a bounded operation or permission target")
		}
	default:
		return "", fmt.Errorf("unknown control reservation kind")
	}
	digest := Digest(kind+":"+id, []byte(encodeTarget(target)))
	return hex.EncodeToString(digest[:]), nil
}

// Runtime slots account for the entire independent host lifetime. Fixed 8 MiB
// model/stream reservations for 16 hosts cap the machine budget at 128 MiB,
// including while no connector is running. Sealed identity evidence remains
// bounded separately and is never recycled into a new executable identity.
func reserveRuntime(ctx context.Context, tx *sql.Tx, target api.SubmissionTarget) error {
	if target.Validate() != nil || target.RuntimeID == "" {
		return fmt.Errorf("complete Runtime target required for resource reservation")
	}
	encoded := encodeTarget(target)
	var sealed int
	err := tx.QueryRowContext(ctx, `SELECT sealed FROM runtime_reservations WHERE target=?`, encoded).Scan(&sealed)
	if err == nil {
		if sealed != 0 {
			return &api.Error{Code: "STALE_RUNTIME", Detail: "Runtime admission is permanently closed"}
		}
		return nil
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return err
	}
	var total, live int
	if err := tx.QueryRowContext(ctx, `SELECT COUNT(*),COALESCE(SUM(live),0) FROM runtime_reservations`).Scan(&total, &live); err != nil {
		return err
	}
	if total >= MaxRuntimeRecords || live >= MaxLiveRuntimes {
		return &api.Error{Code: "SUBMISSION_CAPACITY_EXHAUSTED", Detail: "persistent Runtime/model reservation capacity reached"}
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO runtime_reservations(target) VALUES(?)`, encoded); err != nil {
		return err
	}
	for _, kind := range []string{ControlStop, ControlForget} {
		resource, _ := controlResource(target, kind, "")
		if _, err := tx.ExecContext(ctx, `INSERT INTO control_reservations(resource,target,kind) VALUES(?,?,?)`, resource, encoded, kind); err != nil {
			return err
		}
	}
	return nil
}

// ReserveControl runs before exposing a permission or admitting cancellable
// work. Completed control evidence consumes only its own class, never another
// target's already allocated reservation or a Runtime stop/forget slot.
func (r *Registry) ReserveControl(ctx context.Context, target api.SubmissionTarget, kind, id string) error {
	resource, err := controlResource(target, kind, id)
	if err != nil {
		return err
	}
	if kind == ControlStop || kind == ControlForget {
		return fmt.Errorf("Runtime controls are allocated with their Runtime")
	}
	tx, err := r.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if err := checkRuntimeOpen(ctx, tx, target); err != nil {
		return err
	}
	var count int
	if err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM runtime_reservations WHERE target=?`, encodeTarget(target)).Scan(&count); err != nil || count != 1 {
		return &api.Error{Code: "CONTROL_UNAVAILABLE", Detail: "Runtime resource reservation is absent"}
	}
	if err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM control_reservations WHERE resource=?`, resource).Scan(&count); err != nil {
		return err
	}
	if count == 1 {
		return nil
	}
	if err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM control_reservations WHERE kind=?`, kind).Scan(&count); err != nil {
		return err
	}
	if count >= r.maxControls {
		return &api.Error{Code: "CONTROL_CAPACITY_EXHAUSTED", Detail: kind + " reservation/evidence capacity reached; existing controls remain usable"}
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO control_reservations(resource,target,kind) VALUES(?,?,?)`, resource, encodeTarget(target), kind); err != nil {
		return err
	}
	return tx.Commit()
}

// ClaimControl uses the SAME submission-key table as ordinary operations.
// Missing/invalid targets consume nothing; their absence remains unknown.
func (r *Registry) ClaimControl(ctx context.Context, key api.SubmissionKey, digest [32]byte, receiver, kind, id string) (Claim, api.SubmissionReceipt, error) {
	if kind != ControlPermission && kind != ControlCancel {
		return Claim{}, api.SubmissionReceipt{SubmissionKey: key, Admission: api.SubmissionUnknown}, fmt.Errorf("stop and forget require atomic admission")
	}
	resource, err := controlResource(key.Target, kind, id)
	if err != nil {
		return Claim{}, api.SubmissionReceipt{SubmissionKey: key, Admission: api.SubmissionUnknown}, err
	}
	return r.claimKey(ctx, key, digest, receiver, resource, "")
}

// AcceptStop consumes the Runtime's exclusive slot and records admission in
// one transaction. A cancelled request cannot strand a claimed stop reservation
// before its owner has permission to stop. acquired grants execution only once.
func (r *Registry) AcceptStop(ctx context.Context, key api.SubmissionKey, digest [32]byte, receiver, ref string) (acquired bool, receipt api.SubmissionReceipt, err error) {
	if api.ValidateSubmissionID(ref) != nil {
		return false, api.SubmissionReceipt{SubmissionKey: key, Admission: api.SubmissionUnknown}, fmt.Errorf("bounded stop operation reference required")
	}
	resource, err := controlResource(key.Target, ControlStop, "")
	if err != nil {
		return false, api.SubmissionReceipt{SubmissionKey: key, Admission: api.SubmissionUnknown}, err
	}
	claim, receipt, err := r.claimKey(ctx, key, digest, receiver, resource, ref)
	return claim.Acquired(), receipt, err
}

// ReleaseControl is only for an irrevocably ended target. Consumed reservations
// retain their admission and closure evidence; they can never fund another key.
func (r *Registry) ReleaseControl(ctx context.Context, target api.SubmissionTarget, kind, id string) error {
	if kind != ControlPermission && kind != ControlCancel {
		return fmt.Errorf("only ended operation/permission reservations may be released")
	}
	resource, err := controlResource(target, kind, id)
	if err != nil {
		return err
	}
	_, err = r.db.ExecContext(ctx, `DELETE FROM control_reservations WHERE resource=? AND consumed_key=''`, resource)
	return err
}

func checkRuntimeOpen(ctx context.Context, db queryRow, target api.SubmissionTarget) error {
	if target.RuntimeID == "" {
		return nil
	}
	var sealed int
	err := db.QueryRowContext(ctx, `SELECT sealed FROM runtime_reservations WHERE target=?`, encodeTarget(target)).Scan(&sealed)
	if errors.Is(err, sql.ErrNoRows) {
		return nil
	}
	if err != nil {
		return err
	}
	if sealed != 0 {
		return &api.Error{Code: "STALE_RUNTIME", Detail: "Runtime admission is permanently closed"}
	}
	return nil
}
