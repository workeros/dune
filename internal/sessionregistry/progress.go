package sessionregistry

import (
	"context"
	"fmt"

	"github.com/aiomni/dune/pkg/api"
)

// AcceptLaunch atomically reserves the new identity and stop/forget capacity
// with launch admission, before any worktree/setup/Agent side effect.
func (r *Registry) AcceptLaunch(ctx context.Context, claim Claim, operationRef string, runtime api.Runtime) (api.SubmissionReceipt, error) {
	result := api.SubmissionReceipt{SubmissionKey: claim.key, Admission: api.SubmissionUnknown}
	if !claim.Acquired() || claim.key.Target.RuntimeID != "" || (operationRef == "" || len(operationRef) > 8192) {
		return result, fmt.Errorf("exclusive launch claim required")
	}
	target := claim.key.Target
	target.RuntimeID, target.RuntimeIncarnation, target.RuntimeGeneration = runtime.ID, runtime.Incarnation, runtime.Generation
	if target.Validate() != nil || len(api.Payload(runtime)) > 16*1024 {
		return result, fmt.Errorf("launch requires reserved Runtime identity")
	}
	tx, err := r.db.BeginTx(ctx, nil)
	if err != nil {
		return result, err
	}
	defer tx.Rollback()
	encoded, _ := encodeKey(claim.key)
	record, err := readRecord(ctx, tx, encoded)
	if err != nil {
		return result, err
	}
	if record.token != claim.token || record.state != "claimed" {
		return result, conflict()
	}
	if err := reserveRuntime(ctx, tx, target); err != nil {
		return result, err
	}
	if _, err := tx.ExecContext(ctx, `UPDATE submission_keys SET state='accepted',operation_ref=?,stage='accepted',runtime=? WHERE key=?`, operationRef, api.Payload(runtime), encoded); err != nil {
		return result, err
	}
	if err := tx.Commit(); err != nil {
		return result, err
	}
	return api.SubmissionReceipt{SubmissionKey: claim.key, Admission: api.SubmissionAccepted, OperationRef: operationRef, Stage: "accepted", Runtime: &runtime}, nil
}

// Progress records a confirmed stage of the original accepted operation. It
// never grants execution permission; reopening a receipt cannot replay a write.
func (r *Registry) Progress(ctx context.Context, key api.SubmissionKey, ref, stage, code string, runtime *api.Runtime) (api.SubmissionReceipt, error) {
	result := api.SubmissionReceipt{SubmissionKey: key, Admission: api.SubmissionUnknown}
	encoded, err := encodeKey(key)
	if err != nil || ref == "" || len(ref) > 8192 || (code != "" && api.ValidateSubmissionID(code) != nil) {
		return result, fmt.Errorf("valid original operation and bounded progress required")
	}
	switch stage {
	case "worktree", "setup", "host_starting", "started", "failed", "writing", "written", "input_unrecoverable", "not_sent", "stopping", "stopped", "cleaning", "completed":
	default:
		return result, fmt.Errorf("unknown operation progress stage")
	}
	if runtime != nil && len(api.Payload(runtime)) > 16*1024 {
		return result, fmt.Errorf("Runtime description exceeds progress budget")
	}
	tx, err := r.db.BeginTx(ctx, nil)
	if err != nil {
		return result, err
	}
	defer tx.Rollback()
	record, err := readRecord(ctx, tx, encoded)
	if err != nil {
		return result, err
	}
	if record.state != "accepted" || record.operationRef != ref {
		return result, conflict()
	}
	if len(record.cleanup) != 0 {
		return result, fmt.Errorf("cleanup progress requires its fenced step checkpoint")
	}
	previous := record.receipt(key)
	if terminalStage(previous.Stage) {
		if previous.Stage != stage {
			return result, conflict()
		}
		return previous, nil
	}
	if runtime != nil && previous.Runtime != nil && (runtime.ID != previous.Runtime.ID || runtime.Incarnation != previous.Runtime.Incarnation || runtime.Generation != previous.Runtime.Generation) {
		return result, conflict()
	}
	body := record.runtime
	if runtime != nil {
		body = api.Payload(runtime)
	}
	if _, err := tx.ExecContext(ctx, `UPDATE submission_keys SET stage=?,error_code=?,runtime=? WHERE key=?`, stage, code, body, encoded); err != nil {
		return result, err
	}
	if err := tx.Commit(); err != nil {
		return result, err
	}
	record.stage, record.errorCode, record.runtime = stage, code, body
	return record.receipt(key), nil
}

func terminalStage(stage string) bool {
	switch stage {
	case "started", "failed", "written", "input_unrecoverable", "not_sent", "stopped", "completed":
		return true
	default:
		return false
	}
}

func (r *Registry) RecordWorktree(ctx context.Context, key api.SubmissionKey, ref string, worktree api.Worktree) (api.SubmissionReceipt, error) {
	result := api.SubmissionReceipt{SubmissionKey: key, Admission: api.SubmissionUnknown}
	encoded, err := encodeKey(key)
	body := api.Payload(worktree)
	if err != nil || len(body) > 16*1024 {
		return result, fmt.Errorf("bounded worktree result required")
	}
	tx, err := r.db.BeginTx(ctx, nil)
	if err != nil {
		return result, err
	}
	defer tx.Rollback()
	record, err := readRecord(ctx, tx, encoded)
	if err != nil {
		return result, err
	}
	if record.state != "accepted" || record.stage != "worktree" || record.operationRef != ref || len(record.worktree) != 0 {
		return result, conflict()
	}
	if _, err := tx.ExecContext(ctx, `UPDATE submission_keys SET worktree=? WHERE key=?`, body, encoded); err != nil {
		return result, err
	}
	if err := tx.Commit(); err != nil {
		return result, err
	}
	record.worktree = body
	return record.receipt(key), nil
}
