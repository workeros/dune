package sessionregistry

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"

	"github.com/aiomni/dune/pkg/api"
)

// PrepareHost records the original resources before tmux can execute anything.
// PID zero means no process observation was durably recorded, never absence.
func (r *Registry) PrepareHost(ctx context.Context, host HostRecord) error {
	if host.Target.Validate() != nil || !matchingRuntime(host.Target, host.Runtime) || api.ValidateSubmissionID(host.Instance) != nil || api.ValidateSubmissionID(host.BootID) != nil || host.PID != 0 || host.Resources.Directory.Inode == 0 || host.Resources.Socket.Inode != 0 || host.Resources.Validate(host.Target, host.Instance) != nil || len(host.Registration) > 64*1024 || !json.Valid(host.Registration) {
		return fmt.Errorf("complete original host preparation required")
	}
	tx, err := r.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if err := checkRuntimeOpen(ctx, tx, host.Target); err != nil {
		return err
	}
	if _, _, err := startupLaunch(ctx, tx, host.Target); err != nil {
		return err
	}
	_, err = tx.ExecContext(ctx, `INSERT INTO session_hosts(target,instance,boot_id,pid,phase,runtime,registration,resources) VALUES(?,?,?,0,'starting',?,?,?)`, encodeTarget(host.Target), host.Instance, host.BootID, api.Payload(host.Runtime), host.Registration, api.Payload(host.Resources))
	if err != nil {
		return err
	}
	if err := publishStartupRuntime(ctx, tx, host); err != nil {
		return err
	}
	return tx.Commit()
}

// EnterHost is a one-use gate before consuming bootstrap, validation or Agent
// startup. The launch PID, when observed, must match this exact original process.
func (r *Registry) EnterHost(ctx context.Context, directory, boot string, pid int) (HostRecord, error) {
	tx, err := r.db.BeginTx(ctx, nil)
	if err != nil {
		return HostRecord{}, err
	}
	defer tx.Rollback()
	host, err := scanHost(tx.QueryRowContext(ctx, `SELECT `+hostColumns+` FROM session_hosts h WHERE json_extract(h.resources,'$.directory.path')=?`, directory))
	if err != nil {
		return host, err
	}
	if pid <= 1 || host.BootID != boot || host.Phase != "starting" || (host.PID != 0 && host.PID != pid) {
		return host, conflict()
	}
	if err := checkRuntimeOpen(ctx, tx, host.Target); err != nil {
		return host, err
	}
	if _, _, err := startupLaunch(ctx, tx, host.Target); err != nil {
		return host, err
	}
	host.PID, host.Phase = pid, "validating"
	_, err = tx.ExecContext(ctx, `UPDATE session_hosts SET pid=?,phase='validating' WHERE target=?`, pid, encodeTarget(host.Target))
	if err != nil {
		return host, err
	}
	return host, tx.Commit()
}

// ObserveHostPID stores tmux's original new-session result. It cannot replace a
// process that already entered, nor can it grant another execution permission.
func (r *Registry) ObserveHostPID(ctx context.Context, target api.SubmissionTarget, instance string, pid int) error {
	if pid <= 1 {
		return fmt.Errorf("original pane process required")
	}
	result, err := r.db.ExecContext(ctx, `UPDATE session_hosts SET pid=? WHERE target=? AND instance=? AND (pid=0 OR pid=?)`, pid, encodeTarget(target), instance, pid)
	if err != nil {
		return err
	}
	if n, err := result.RowsAffected(); err != nil || n != 1 {
		return conflict()
	}
	return nil
}

func startupLaunch(ctx context.Context, tx *sql.Tx, target api.SubmissionTarget) (string, record, error) {
	var key string
	if err := tx.QueryRowContext(ctx, `SELECT launch_key FROM runtime_reservations WHERE target=?`, encodeTarget(target)).Scan(&key); err != nil {
		return "", record{}, err
	}
	launch, err := readRecord(ctx, tx, key)
	if err == nil && (launch.state != "accepted" || launch.stage != "host_starting") {
		err = conflict()
	}
	return key, launch, err
}

func publishStartupRuntime(ctx context.Context, tx *sql.Tx, host HostRecord) error {
	key, _, err := startupLaunch(ctx, tx, host.Target)
	if err != nil {
		return err
	}
	_, err = tx.ExecContext(ctx, `UPDATE submission_keys SET runtime=? WHERE key=?`, api.Payload(host.Runtime), key)
	return err
}

// StartupProgress accepts a fixed vocabulary only. No exception text, private
// bootstrap, ACP response or environment can enter this bounded diagnostic.
func (r *Registry) StartupProgress(ctx context.Context, target api.SubmissionTarget, instance, phase string, timeout bool) error {
	switch phase {
	case "host_pending", "host_validation", "agent_start", "agent_initialization", "ready":
	default:
		return fmt.Errorf("invalid startup phase")
	}
	tx, err := r.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	host, err := scanHost(tx.QueryRowContext(ctx, `SELECT `+hostColumns+` FROM session_hosts h WHERE target=?`, encodeTarget(target)))
	if err != nil {
		return err
	}
	if host.Instance != instance || host.Phase == "failed" || host.Phase == "exited" {
		return conflict()
	}
	if host.Runtime.ACPHost == nil {
		return conflict()
	}
	if timeout {
		if host.Runtime.ACPHost.Startup == nil {
			return conflict()
		}
		host.Runtime.ACPHost.Startup.ConfirmationTimeout = true
	} else {
		host.Runtime.ACPHost.Startup = &api.ACPStartupDiagnostic{Phase: phase}
	}
	if err := publishStartupRuntime(ctx, tx, host); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `UPDATE session_hosts SET runtime=? WHERE target=?`, api.Payload(host.Runtime), encodeTarget(target)); err != nil {
		return err
	}
	return tx.Commit()
}

// FailHost proves irreversible failure under the same write transaction as
// EnterHost, RegisterHost and RecordGroup. The verifier must either own this
// returning host (and prove its Agent group absent), or prove both host and
// group absent. Neither a timeout nor a tmux observation alone is sufficient.
func (r *Registry) FailHost(ctx context.Context, target api.SubmissionTarget, instance, code string, verify func(HostRecord) error) error {
	switch code {
	case "HOST_EXITED_BEFORE_ENTRY", "HOST_VALIDATION_FAILED", "HOST_START_FAILED", "AGENT_START_FAILED", "AGENT_INITIALIZATION_FAILED":
	default:
		return fmt.Errorf("invalid startup failure")
	}
	if verify == nil {
		return fmt.Errorf("failure proof required")
	}
	tx, err := r.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	host, err := scanHost(tx.QueryRowContext(ctx, `SELECT `+hostColumns+` FROM session_hosts h WHERE target=?`, encodeTarget(target)))
	if err != nil {
		return err
	}
	if host.Instance != instance {
		return conflict()
	}
	if host.Phase == "failed" {
		return nil
	}
	key, _, err := startupLaunch(ctx, tx, target)
	if err != nil {
		return err
	}
	if err := verify(host); err != nil {
		return err
	}
	host.Runtime.State, host.Runtime.StopReason = "exited", code
	if host.Runtime.ACPHost == nil {
		return conflict()
	}
	if host.Runtime.ACPHost.Startup == nil {
		host.Runtime.ACPHost.Startup = &api.ACPStartupDiagnostic{Phase: "host_pending"}
	}
	host.Runtime.ACPHost.Startup.Code = code
	if _, err := tx.ExecContext(ctx, `UPDATE session_hosts SET phase='failed',runtime=? WHERE target=?`, api.Payload(host.Runtime), encodeTarget(target)); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `UPDATE submission_keys SET stage='failed',error_code=?,runtime=? WHERE key=?`, code, api.Payload(host.Runtime), key); err != nil {
		return err
	}
	return tx.Commit()
}

// PendingLaunch is read-only and retains the caller's original launch key.
func (r *Registry) PendingLaunch(ctx context.Context, target api.SubmissionTarget) (api.SubmissionReceipt, error) {
	var encoded string
	if target.Validate() != nil || target.RuntimeID == "" {
		return api.SubmissionReceipt{}, conflict()
	}
	if err := r.db.QueryRowContext(ctx, `SELECT launch_key FROM runtime_reservations WHERE target=?`, encodeTarget(target)).Scan(&encoded); err != nil {
		return api.SubmissionReceipt{}, err
	}
	var key api.SubmissionKey
	if json.Unmarshal([]byte(encoded), &key) != nil {
		return api.SubmissionReceipt{}, conflict()
	}
	return r.Get(ctx, key)
}

// ConfirmUnregisteredFailure supports explicitly retiring an original launch
// predating preparation records. Never adopt an unregistered live host. The
// verifier checks its original bootstrap/marked pane and kernel absence, while
// this transaction proves no host (and thus no Agent group) has registered and
// prevents any late RegisterHost from crossing the terminal failure record.
func (r *Registry) ConfirmUnregisteredFailure(ctx context.Context, host HostRecord, verify func() error) error {
	if host.Target.Validate() != nil || !matchingRuntime(host.Target, host.Runtime) || host.PID <= 1 || api.ValidateSubmissionID(host.Instance) != nil || api.ValidateSubmissionID(host.BootID) != nil || host.Resources.Validate(host.Target, host.Instance) != nil || host.Resources.Socket.Inode != 0 || len(host.Registration) > 64*1024 || !json.Valid(host.Registration) || host.Runtime.ACPHost == nil || verify == nil {
		return conflict()
	}
	tx, err := r.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if err := checkRuntimeOpen(ctx, tx, host.Target); err != nil {
		return err
	}
	key, _, err := startupLaunch(ctx, tx, host.Target)
	if err != nil {
		return err
	}
	var count int
	if err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM session_hosts WHERE target=?`, encodeTarget(host.Target)).Scan(&count); err != nil {
		return err
	}
	if count != 0 {
		return conflict()
	}
	if err := verify(); err != nil {
		return err
	}
	host.Runtime.State, host.Runtime.StopReason = "exited", "HOST_EXITED_BEFORE_ENTRY"
	host.Runtime.ACPHost.Startup = &api.ACPStartupDiagnostic{Phase: "host_pending", Code: "HOST_EXITED_BEFORE_ENTRY"}
	if _, err := tx.ExecContext(ctx, `INSERT INTO session_hosts(target,instance,boot_id,pid,phase,runtime,registration,resources) VALUES(?,?,?,?,'failed',?,?,?)`, encodeTarget(host.Target), host.Instance, host.BootID, host.PID, api.Payload(host.Runtime), host.Registration, api.Payload(host.Resources)); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `UPDATE submission_keys SET stage='failed',error_code='HOST_EXITED_BEFORE_ENTRY',runtime=? WHERE key=?`, api.Payload(host.Runtime), key); err != nil {
		return err
	}
	return tx.Commit()
}
