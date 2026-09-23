package fabricd

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"time"

	"github.com/aiomni/dune/internal/lifecycle"
	"github.com/aiomni/dune/internal/process"
	"github.com/aiomni/dune/internal/sessionregistry"
	"github.com/aiomni/dune/internal/wire"
	"github.com/aiomni/dune/pkg/api"
	pb "github.com/aiomni/dune/proto/dune/dtp/v1"
)

type cleanupExecution struct{ done chan struct{} }

func (d *Engine) submitForget(s *executionStream, message *pb.Message, machine string) {
	var request api.SubmissionRequest
	if wire.Decode(message, &request) != nil || request.SubmissionKey.Validate() != nil || request.Target.RuntimeID == "" || request.Operation != "runtime.forget" || (len(request.Payload) != 0 && string(request.Payload) != "null") {
		s.Fail("INVALID_ARGUMENT", errors.New("forget requires the original Runtime submission key and no payload"))
		return
	}
	key := request.SubmissionKey
	receipt := api.SubmissionReceipt{SubmissionKey: key, Admission: api.SubmissionUnknown}
	if !matchesSubmissionTarget(key.Target, message, machine) {
		failSubmission(s, receipt, &api.Error{Code: "STALE_BINDING", Detail: "forget differs from the routed target"})
		return
	}
	if d.registry == nil || d.lock == nil {
		failSubmission(s, receipt, &api.Error{Code: "SESSION_UNAVAILABLE", Detail: "independent cleanup registry is unavailable"})
		return
	}
	if s.Send(&pb.Message{Kind: "accepted", RequestId: message.RequestId}) != nil {
		return
	}
	receipt, found, err := d.registry.Lookup(s.ctx, key, sessionregistry.Digest("runtime.forget", nil), sessionregistry.ForgetReceiver)
	acquired := false
	if err == nil && !found {
		_, hostErr := d.registry.Host(s.ctx, key.Target)
		if hostErr == nil {
			acquired, receipt, err = d.registry.AcceptForget(s.ctx, key, wire.ID(), d.verifyHostCleanup)
		} else if errors.Is(hostErr, sql.ErrNoRows) {
			acquired, receipt, err = d.registry.AcceptLocalForget(s.ctx, key, wire.ID(), func() (sessionregistry.LocalCleanup, error) { return d.localCleanupPlan(message, key.Target) })
		} else {
			err = hostErr
		}
	}
	if err == nil {
		err = submissionDecisionError(receipt)
	}
	if err == nil && acquired {
		d.events.Record(lifecycle.Entry{Kind: "cleanup_accepted", RuntimeID: key.Target.RuntimeID, RuntimeIncarnation: key.Target.RuntimeIncarnation, OperationRef: receipt.OperationRef})
		err = d.cleanupPoint(key, "accepted")
		if err == nil {
			execution := d.scheduleCleanup(key, receipt.OperationRef)
			if execution != nil {
				select {
				case <-execution.done:
				case <-s.ctx.Done():
				}
			}
			queryCtx, cancel := context.WithTimeout(context.WithoutCancel(s.ctx), 5*time.Second)
			observed, queryErr := d.registry.Get(queryCtx, key)
			cancel()
			if queryErr == nil {
				receipt = observed
			} else {
				err = queryErr
			}
		}
	}
	if err != nil {
		failSubmission(s, receipt, err)
		return
	}
	_ = s.Send(&pb.Message{Kind: "result", RequestId: message.RequestId, Payload: api.Payload(receipt)})
}

func (d *Engine) verifyHostResources(host sessionregistry.HostRecord) error {
	var reg sessionRegistration
	if json.Unmarshal(host.Registration, &reg) != nil || reg.Target != host.Target || reg.Instance != host.Instance || reg.Installation != installationID(d.stateDir) {
		return cleanupIdentityError()
	}
	socket, err := sessionSocketPath(reg)
	if err != nil || host.Resources.Validate(host.Target, host.Instance) != nil || host.Resources.Directory.Path != filepath.Join(d.stateDir, "acp", "runtimes", host.Target.RuntimeID) || host.Resources.Socket.Path != socket {
		return cleanupIdentityError()
	}
	return nil
}

func (d *Engine) verifyHostCleanup(host sessionregistry.HostRecord) error {
	if err := d.verifyHostResources(host); err != nil {
		return err
	}
	if host.Phase == "exited" || host.Phase == "failed" {
		return nil
	}
	absent, err := process.Absent(host.BootID, host.PID, host.GroupID)
	if err != nil {
		return err
	}
	if !absent {
		return &api.Error{Code: "SESSION_UNAVAILABLE", Detail: "forget requires confirmed Agent exit or host and process-group loss"}
	}
	return nil
}

func localCachePaths(stateDir string, runtime api.Runtime) []string {
	key := fmt.Sprintf("%x", sha256.Sum256([]byte(runtime.ID+"\x00"+runtime.Incarnation)))
	return []string{filepath.Join(stateDir, "pty-timeouts", key), filepath.Join(stateDir, "native-agents", key)}
}

func (d *Engine) localCleanupPlan(message *pb.Message, target api.SubmissionTarget) (sessionregistry.LocalCleanup, error) {
	r, err := d.lookup(message)
	if err != nil {
		return sessionregistry.LocalCleanup{}, err
	}
	if r.host != nil || (r.target != (api.SubmissionTarget{}) && r.target != target) {
		return sessionregistry.LocalCleanup{}, cleanupIdentityError()
	}
	current := r.info()
	if current.State != "exited" {
		return sessionregistry.LocalCleanup{}, &api.Error{Code: "RUNTIME_RUNNING", Detail: "stop the Runtime before forgetting retained history"}
	}
	plan := sessionregistry.LocalCleanup{Runtime: current, Directories: []sessionregistry.FileIdentity{}}
	if r.tmux != nil {
		pane, err := r.tmux.Inspect()
		if err != nil {
			return plan, err
		}
		if !pane.Dead {
			return plan, &api.Error{Code: "RUNTIME_RUNNING", Detail: "terminal exit is not confirmed"}
		}
		for _, path := range r.tmux.CleanupDirectories() {
			identity, err := fileIdentity(path, os.ModeDir)
			if os.IsNotExist(err) {
				continue
			}
			if err != nil {
				return plan, err
			}
			plan.Directories = append(plan.Directories, identity)
		}
	}
	return plan, nil
}

func (d *Engine) cleanupPoint(key api.SubmissionKey, point string) error {
	if d.cleanupBarrier != nil {
		return d.cleanupBarrier(key, point)
	}
	return nil
}

// A schedule belongs to an accepted original key. Reads and duplicate submits
// never call this. active keeps fabricd.lock held until every old action drains;
// the per-key map prevents concurrent executors within the same connector term.
func (d *Engine) scheduleCleanup(key api.SubmissionKey, ref string) *cleanupExecution {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.ctx.Err() != nil {
		return nil
	}
	if existing := d.cleanups[key]; existing != nil {
		return existing
	}
	execution := &cleanupExecution{done: make(chan struct{})}
	d.cleanups[key] = execution
	d.active.Add(1)
	go func() {
		defer d.active.Done()
		defer func() { d.mu.Lock(); delete(d.cleanups, key); close(execution.done); d.mu.Unlock() }()
		ctx, cancel := context.WithTimeout(d.ctx, 20*time.Second)
		defer cancel()
		job, err := d.registry.BindCleanup(ctx, key, ref, d.sessionTerm)
		if err == nil {
			d.executeCleanup(ctx, job)
		}
	}()
	return execution
}

func (d *Engine) recoverCleanups() error {
	jobs, err := d.registry.PendingCleanups(d.ctx)
	if err != nil {
		return err
	}
	for _, job := range jobs {
		d.scheduleCleanup(job.Key, job.Reference)
	}
	return nil
}

func (d *Engine) runCleanupRecovery() {
	defer d.active.Done()
	ticker := time.NewTicker(time.Second)
	ticks := 0
	defer ticker.Stop()
	for {
		select {
		case <-d.ctx.Done():
			return
		case <-ticker.C:
			d.recoverHostStartups()
			ticks++
			if ticks%30 != 0 {
				continue
			}
			if err := d.recoverCleanups(); err != nil && d.ctx.Err() == nil {
				d.recordLifecycle("cleanup_recovery_failed", nil, "", "REGISTRY_UNAVAILABLE", d.sessionTerm)
			}
		}
	}
}

func (d *Engine) executeCleanup(ctx context.Context, job sessionregistry.CleanupJob) {
	steps := job.Steps()
	for job.Confirmed < len(steps) {
		step := steps[job.Confirmed]
		err := d.cleanupPoint(job.Key, step+":before")
		if err == nil {
			err = d.cleanupStep(ctx, job, step)
		}
		if err == nil {
			err = d.cleanupPoint(job.Key, step+":removed")
		}
		code := ""
		if err != nil {
			code = "CLEANUP_UNCONFIRMED"
			var failure *api.Error
			if errors.As(err, &failure) {
				code = failure.Code
			}
		}
		checkpointCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
		_, checkpointErr := d.registry.CheckpointCleanup(checkpointCtx, job, step, code)
		cancel()
		if checkpointErr == nil {
			d.events.Record(lifecycle.Entry{Kind: "cleanup_" + step, RuntimeID: job.Key.Target.RuntimeID, RuntimeIncarnation: job.Key.Target.RuntimeIncarnation, OperationRef: job.Reference, Code: code})
		}
		if err != nil || checkpointErr != nil {
			return
		}
		job.Confirmed++
		if d.cleanupPoint(job.Key, step+":confirmed") != nil {
			return
		}
	}
	d.discoveryScanMu.Lock()
	d.events.Record(lifecycle.Entry{Kind: "cleanup_completed", RuntimeID: job.Key.Target.RuntimeID, RuntimeIncarnation: job.Key.Target.RuntimeIncarnation, OperationRef: job.Reference})
	d.mu.Lock()
	r := d.runtimes[job.Key.Target.RuntimeID]
	if r != nil && r.inc == job.Key.Target.RuntimeIncarnation {
		delete(d.runtimes, r.id)
		d.runtimeWatches.publish(api.RuntimeChange{Runtime: api.Runtime{ID: r.id, Incarnation: r.inc, Generation: 1, Adapter: r.adapter}, Removed: true})
	}
	d.mu.Unlock()
	d.discoveryScanMu.Unlock()
	if r != nil && r.inc == job.Key.Target.RuntimeIncarnation {
		if r.host != nil {
			r.host.close()
		}
		r.closePTYInput()
		if r.acp != nil {
			r.acp.conversation.remove()
		}
	}
}

func (d *Engine) cleanupStep(ctx context.Context, job sessionregistry.CleanupJob, step string) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	barrier := func(point string) error { return d.cleanupPoint(job.Key, step+":"+point) }
	if job.Local != nil {
		if step == "terminal" {
			if job.Local.Runtime.Adapter == "pty" {
				return d.tmux.RetireTerminal(job.Local.Runtime, true)
			}
			return nil
		}
		for _, directory := range job.Local.Directories {
			if !slices.Contains(localCachePaths(d.stateDir, job.Local.Runtime), directory.Path) {
				return cleanupIdentityError()
			}
			if err := removeCleanupResource(ctx, directory, job.Local.Runtime.Incarnation, os.ModeDir, barrier); err != nil {
				return err
			}
		}
		return nil
	}
	host := job.Host
	if err := d.verifyHostResources(host); err != nil {
		return err
	}
	switch step {
	case "host":
		if err := d.acpTmux.DestroyHost(host.Target.RuntimeID, host.Instance); err != nil {
			return err
		}
		if host.Phase == "failed" && host.PID == 0 {
			return nil
		}
		for {
			absent, err := process.Absent(host.BootID, host.PID, host.GroupID)
			if err != nil || absent {
				return err
			}
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-time.After(20 * time.Millisecond):
			}
		}
	case "ipc":
		if host.Resources.Socket.Inode == 0 {
			if _, err := os.Lstat(host.Resources.Socket.Path); os.IsNotExist(err) {
				return nil
			}
			return cleanupIdentityError()
		}
		return removeCleanupResource(ctx, host.Resources.Socket, host.Instance, os.ModeSocket, barrier)
	case "runtime_directory":
		return removeCleanupResource(ctx, host.Resources.Directory, host.Instance, os.ModeDir, barrier)
	}
	return fmt.Errorf("unknown cleanup step")
}
