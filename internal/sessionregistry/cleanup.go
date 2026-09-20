package sessionregistry

import (
	"context"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"path/filepath"
	"strings"

	"github.com/aiomni/dune/internal/wire"
	"github.com/aiomni/dune/pkg/api"
)

const ForgetReceiver = "registry:forget"

// FileIdentity describes a resource created by this installation. Inode checks
// must be paired with the instance marker for directories, and cleanup must
// use checked directory handles rather than recursively following saved paths.
type FileIdentity struct {
	Path   string `json:"path"`
	Device uint64 `json:"device"`
	Inode  uint64 `json:"inode"`
}

type CleanupResources struct {
	Directory   FileIdentity `json:"directory"`
	Socket      FileIdentity `json:"socket"`
	TmuxSession string       `json:"tmux_session"`
	Instance    string       `json:"instance"`
}

func (r CleanupResources) Validate(target api.SubmissionTarget, instance string) error {
	if api.ValidateSubmissionID(instance) != nil || r.Instance != instance || r.TmuxSession != "acp-"+target.RuntimeID {
		return fmt.Errorf("cleanup requires the original host resource instance")
	}
	for _, file := range []FileIdentity{r.Directory, r.Socket} {
		if !filepath.IsAbs(file.Path) || filepath.Clean(file.Path) != file.Path || file.Path == "/" || len(file.Path) > 4096 || strings.ContainsRune(file.Path, 0) || file.Inode == 0 {
			return fmt.Errorf("cleanup requires bounded absolute resource identities")
		}
	}
	if r.Directory.Path == r.Socket.Path || filepath.Base(r.Directory.Path) != target.RuntimeID {
		return fmt.Errorf("cleanup resource paths do not match their Runtime")
	}
	return nil
}

var cleanupSteps = [...]string{"host", "ipc", "runtime_directory"}

func cleanupProgress(confirmed int) api.CleanupProgress {
	return api.CleanupProgress{Confirmed: append([]string{}, cleanupSteps[:confirmed]...), Remaining: append([]string{}, cleanupSteps[confirmed:]...)}
}

// AcceptForget binds the original Runtime key, consumes only its reserved
// cleanup slot, freezes resources, and seals further admissions atomically.
// verify must prove exit/loss and installation ownership from this exact host
// snapshot using read-only local evidence. It runs inside the SAME transaction
// as RecordGroup, preventing a replacement process from crossing that proof.
// Duplicate lookup precedes verification, even after resources are removed.
func (r *Registry) AcceptForget(ctx context.Context, key api.SubmissionKey, ref string, verify func(HostRecord) error) (bool, api.SubmissionReceipt, error) {
	result := api.SubmissionReceipt{SubmissionKey: key, Admission: api.SubmissionUnknown}
	encoded, err := encodeKey(key)
	if err != nil || key.Target.RuntimeID == "" || api.ValidateSubmissionID(ref) != nil || verify == nil {
		return false, result, fmt.Errorf("original Runtime key, cleanup reference and verifier required")
	}
	tx, err := r.db.BeginTx(ctx, nil)
	if err != nil {
		return false, result, err
	}
	defer tx.Rollback()
	digest := Digest("runtime.forget", nil)
	stored, err := readRecord(ctx, tx, encoded)
	if err == nil {
		if stored.digest != hex.EncodeToString(digest[:]) || stored.receiver != ForgetReceiver {
			return false, result, conflict()
		}
		return false, stored.receipt(key), nil
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return false, result, err
	}
	if err := checkRuntimeOpen(ctx, tx, key.Target); err != nil {
		return false, result, err
	}
	host, err := scanHost(tx.QueryRowContext(ctx, `SELECT `+hostColumns+` FROM session_hosts h WHERE h.target=?`, encodeTarget(key.Target)))
	if err != nil {
		return false, result, err
	}
	if err := verify(host); err != nil {
		// No irreversible rejection was saved. A delayed original request may
		// still arrive, so the public admission remains unknown.
		return false, result, err
	}
	resource, _ := controlResource(key.Target, ControlForget, "")
	var consumed string
	if err := tx.QueryRowContext(ctx, `SELECT consumed_key FROM control_reservations WHERE resource=?`, resource).Scan(&consumed); err != nil || consumed != "" {
		return false, result, &api.Error{Code: "CONTROL_UNAVAILABLE", Detail: "cleanup reservation is absent or already consumed"}
	}
	progress := cleanupProgress(0)
	if _, err := tx.ExecContext(ctx, `INSERT INTO submission_keys(key,digest,receiver,token,state,control_resource,operation_ref,stage,cleanup) VALUES(?,?,?,?,'accepted',?,?,'cleaning',?)`, encoded, hex.EncodeToString(digest[:]), ForgetReceiver, wire.ID(), resource, ref, api.Payload(progress)); err != nil {
		return false, result, err
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO cleanup_jobs(key,host) VALUES(?,?)`, encoded, api.Payload(host)); err != nil {
		return false, result, err
	}
	if _, err := tx.ExecContext(ctx, `UPDATE control_reservations SET consumed_key=? WHERE resource=?`, encoded, resource); err != nil {
		return false, result, err
	}
	if _, err := tx.ExecContext(ctx, `UPDATE runtime_reservations SET sealed=1 WHERE target=?`, encodeTarget(key.Target)); err != nil {
		return false, result, err
	}
	if err := tx.Commit(); err != nil {
		return false, result, err
	}
	result.Admission, result.OperationRef, result.Stage, result.Cleanup = api.SubmissionAccepted, ref, "cleaning", &progress
	return true, result, nil
}

// CleanupJob is an immutable plan plus its latest checkpoint. Term fences
// result publication. The executor must additionally retain fabricd.lock until
// all its filesystem/tmux actions finish; a checkpoint is not a process lock.
type CleanupJob struct {
	Key       api.SubmissionKey
	Host      HostRecord
	Reference string
	Confirmed int
	Term      uint64
}

func scanCleanup(row interface{ Scan(...any) error }) (CleanupJob, error) {
	var job CleanupJob
	var key, host []byte
	if err := row.Scan(&key, &host, &job.Reference, &job.Confirmed, &job.Term); err != nil {
		return job, err
	}
	if json.Unmarshal(key, &job.Key) != nil || json.Unmarshal(host, &job.Host) != nil || job.Key.Validate() != nil || job.Host.Target != job.Key.Target || job.Host.Resources.Validate(job.Key.Target, job.Host.Instance) != nil || api.ValidateSubmissionID(job.Reference) != nil || job.Confirmed < 0 || job.Confirmed > len(cleanupSteps) {
		return job, fmt.Errorf("invalid persisted cleanup plan")
	}
	return job, nil
}

const cleanupColumns = `j.key,j.host,k.operation_ref,j.confirmed,j.executor_term`

// PendingCleanups is observational. Only the recovery scheduler may hand these
// original plans to an executor; submission queries never call that scheduler.
func (r *Registry) PendingCleanups(ctx context.Context) ([]CleanupJob, error) {
	rows, err := r.db.QueryContext(ctx, `SELECT `+cleanupColumns+` FROM cleanup_jobs j JOIN submission_keys k ON k.key=j.key WHERE j.confirmed<? ORDER BY j.key LIMIT ?`, len(cleanupSteps), MaxRuntimeRecords)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	jobs := []CleanupJob{}
	for rows.Next() {
		job, err := scanCleanup(rows)
		if err != nil {
			return nil, err
		}
		jobs = append(jobs, job)
	}
	return jobs, rows.Err()
}

func (r *Registry) BindCleanup(ctx context.Context, key api.SubmissionKey, ref string, term uint64) (CleanupJob, error) {
	encoded, err := encodeKey(key)
	if err != nil || term == 0 || term >= 1<<63 {
		return CleanupJob{}, fmt.Errorf("original cleanup key and bounded executor term required")
	}
	tx, err := r.db.BeginTx(ctx, nil)
	if err != nil {
		return CleanupJob{}, err
	}
	defer tx.Rollback()
	job, err := scanCleanup(tx.QueryRowContext(ctx, `SELECT `+cleanupColumns+` FROM cleanup_jobs j JOIN submission_keys k ON k.key=j.key WHERE j.key=?`, encoded))
	if err != nil {
		return job, err
	}
	if job.Term > term || job.Reference != ref || job.Confirmed == len(cleanupSteps) {
		return CleanupJob{}, conflict()
	}
	if _, err := tx.ExecContext(ctx, `UPDATE cleanup_jobs SET executor_term=? WHERE key=?`, term, encoded); err != nil {
		return CleanupJob{}, err
	}
	if err := tx.Commit(); err != nil {
		return CleanupJob{}, err
	}
	job.Term = term
	return job, nil
}

// CheckpointCleanup records one confirmed step, or a bounded failure code
// without advancing. Only the final checkpoint marks completed and releases
// the live Runtime slot. The plan, identity seal and receipt remain retained.
func (r *Registry) CheckpointCleanup(ctx context.Context, job CleanupJob, step, code string) (api.SubmissionReceipt, error) {
	result := api.SubmissionReceipt{SubmissionKey: job.Key, Admission: api.SubmissionUnknown}
	encoded, err := encodeKey(job.Key)
	if err != nil || job.Term == 0 || job.Confirmed < 0 || job.Confirmed >= len(cleanupSteps) || cleanupSteps[job.Confirmed] != step || (code != "" && api.ValidateSubmissionID(code) != nil) {
		return result, fmt.Errorf("original cleanup step and bounded result required")
	}
	tx, err := r.db.BeginTx(ctx, nil)
	if err != nil {
		return result, err
	}
	defer tx.Rollback()
	stored, err := scanCleanup(tx.QueryRowContext(ctx, `SELECT `+cleanupColumns+` FROM cleanup_jobs j JOIN submission_keys k ON k.key=j.key WHERE j.key=?`, encoded))
	if err != nil {
		return result, err
	}
	if job.Term != stored.Term || job.Confirmed != stored.Confirmed || job.Reference != stored.Reference {
		return result, conflict()
	}
	confirmed := stored.Confirmed
	if code == "" {
		confirmed++
	}
	progress := cleanupProgress(confirmed)
	stage := "cleaning"
	if confirmed == len(cleanupSteps) {
		stage = "completed"
		if _, err := tx.ExecContext(ctx, `UPDATE runtime_reservations SET live=0 WHERE target=? AND sealed=1`, encodeTarget(job.Key.Target)); err != nil {
			return result, err
		}
	}
	if _, err := tx.ExecContext(ctx, `UPDATE cleanup_jobs SET confirmed=? WHERE key=?`, confirmed, encoded); err != nil {
		return result, err
	}
	if _, err := tx.ExecContext(ctx, `UPDATE submission_keys SET stage=?,error_code=?,cleanup=? WHERE key=?`, stage, code, api.Payload(progress), encoded); err != nil {
		return result, err
	}
	if err := tx.Commit(); err != nil {
		return result, err
	}
	result.Admission, result.OperationRef, result.Stage, result.ErrorCode, result.Cleanup = api.SubmissionAccepted, job.Reference, stage, code, &progress
	return result, nil
}
