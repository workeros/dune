package runnerupgrade

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"time"

	"github.com/aiomni/dune/internal/config"
	"github.com/aiomni/dune/internal/installation"
	"github.com/aiomni/dune/internal/launchgate"
	"github.com/aiomni/dune/internal/release"
	"github.com/aiomni/dune/internal/service"
	"github.com/aiomni/dune/internal/statecontract"
	"github.com/aiomni/dune/internal/upgradecontrol"
	"github.com/aiomni/dune/internal/upgradejob"
	"github.com/aiomni/dune/internal/wire"
	"github.com/aiomni/dune/pkg/api"
	"github.com/aiomni/dune/pkg/runner"
	"github.com/aiomni/dune/pkg/upgrade"
)

const (
	TargetTimeout   = 90 * time.Second
	RollbackTimeout = 2 * time.Minute
)

type platformControl interface {
	Binding(context.Context, runner.Binding) (runner.Binding, error)
	Inspect(context.Context, runner.Binding) (upgrade.Inspection, error)
	Confirm(context.Context, upgrade.Probe) (upgrade.Proof, error)
}

// worker's seams describe effects, not service-manager command details. Normal
// execution always supplies the real checker, downloader, control and restart.
type worker struct {
	root            string
	installed       *installation.Store
	jobs            *upgradejob.Store
	metadata        installation.Metadata
	record          upgradejob.Record
	gate            *launchgate.Gate
	seal            launchgate.Seal
	control         platformControl
	check           func(context.Context, string, string, upgrade.Manifest) (api.UpgradeReport, error)
	download        func(context.Context, upgrade.Manifest, string) error
	restart         func(context.Context, installation.Metadata) error
	poll            time.Duration
	targetTimeout   time.Duration
	rollbackTimeout time.Duration
	// Used only by process/fault tests at durable boundaries.
	barrier func(string) error
}

// Run resumes at most the installation's original active job. It never admits
// work. RecoveryBlocked requires explicit repair; a supervisor must not retry
// an unsuccessful rollback forever.
func Run(ctx context.Context, root string) error {
	installed, err := installation.Lock(root)
	if err != nil {
		return err
	}
	defer installed.Close()
	state, err := installed.Read()
	if err != nil {
		return err
	}
	jobs, err := upgradejob.Open(ctx, filepath.Join(root, "upgrades"))
	if err != nil {
		return err
	}
	defer jobs.Close()
	active, err := jobs.Active(ctx)
	if err != nil || active == nil {
		return err
	}
	if active.Operation.Phase == upgrade.RecoveryBlocked {
		return nil
	}
	cfg, err := config.Load(state.Metadata.ConfigPath)
	if err != nil {
		return issue("UPGRADE_CONFIG_UNAVAILABLE")
	}
	trust, err := cfg.TLS()
	if err != nil {
		return issue("UPGRADE_CONFIG_UNAVAILABLE")
	}
	control, err := upgradecontrol.New(cfg.UpgradeControlURL, cfg.Token, trust)
	if err != nil {
		return issue("UPGRADE_CONTROL_UNAVAILABLE")
	}
	defer control.Close()
	w := worker{root: root, installed: installed, jobs: jobs, metadata: state.Metadata, control: control, check: checkProgram, poll: time.Second,
		download: func(ctx context.Context, m upgrade.Manifest, path string) error {
			return release.Download(ctx, nil, m, path)
		},
		restart: func(ctx context.Context, m installation.Metadata) error {
			return service.Run(ctx, "restart", m.ServiceName)
		},
	}
	return w.execute(ctx, active.Operation.ID)
}

// Watch belongs to a separate service/job, never a fabricd goroutine or exec
// guardian. A restart of this process resumes the original persisted operation.
func Watch(ctx context.Context, root string) error {
	ticker := time.NewTicker(2 * time.Second)
	defer ticker.Stop()
	for {
		err := Run(ctx, root)
		if ctx.Err() != nil {
			return nil
		}
		// A failed checkpoint remains durable. Wait before retrying acquisition;
		// confirmed/blocked jobs cannot be restarted by the next iteration.
		if err != nil && !errors.Is(err, installation.ErrBusy) {
			return issue(failureIssue(err, "worker").Code)
		}
		select {
		case <-ctx.Done():
			return nil
		case <-ticker.C:
		}
	}
}

func (w *worker) execute(ctx context.Context, id string) error {
	if w.targetTimeout <= 0 {
		w.targetTimeout = TargetTimeout
	}
	if w.rollbackTimeout <= 0 {
		w.rollbackTimeout = RollbackTimeout
	}
	if w.poll <= 0 {
		w.poll = time.Second
	}
	var err error
	w.record, err = w.jobs.Claim(ctx, id)
	if err != nil {
		return err
	}
	defer func() {
		if w.gate != nil {
			w.gate.Close()
		}
	}()
	if w.metadata.ID != w.record.Operation.Request.InstallationID {
		return issue("INSTALLATION_CHANGED")
	}
	seal, err := launchgate.ReadSeal(w.metadata.StateDir)
	if err != nil {
		return err
	}
	if seal != nil || w.record.Operation.LaunchSealed {
		if err := w.claimSeal(ctx); err != nil {
			return err
		}
	}
	if w.record.Operation.Confirmed {
		return w.unseal(ctx)
	}
	if err := w.checkpoint("claimed"); err != nil {
		return err
	}
	for {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		op := w.record.Operation
		if beforeSwitch(op.Phase) && time.Now().After(w.record.Deadline) {
			return w.failBeforeSwitch(ctx, issue("UPGRADE_DEADLINE"))
		}
		switch op.Phase {
		case upgrade.Queued:
			current, err := w.installed.Read()
			if err != nil {
				return w.failBeforeSwitch(ctx, err)
			}
			fingerprint, err := configurationFingerprint(w.metadata.ConfigPath)
			if err != nil {
				return w.failBeforeSwitch(ctx, issue("UPGRADE_CONFIG_UNAVAILABLE"))
			}
			err = w.update(ctx, func(r *upgradejob.Record) {
				r.Original = current.Current
				r.Candidate = installation.Location{Directory: "releases/" + wire.ID(), Manifest: op.Target}
				r.ConfigurationSHA256 = fingerprint
				r.Operation.Phase = upgrade.Preparing
			})
			if err != nil {
				return err
			}
		case upgrade.Preparing:
			if err := w.validateContext(ctx); err != nil {
				return w.failBeforeSwitch(ctx, err)
			}
			if err := w.update(ctx, func(r *upgradejob.Record) { r.Operation.Phase = upgrade.Downloading }); err != nil {
				return err
			}
		case upgrade.Downloading:
			path := filepath.Join(w.root, w.record.Candidate.Directory)
			// This directory was reserved by this original job before download. It is
			// never selected while downloading, so interrupted staging can be discarded.
			current, err := w.installed.Read()
			if err != nil || current.Current.Directory == w.record.Candidate.Directory || current.Pending != nil {
				return issue("INSTALLATION_RECOVERY_REQUIRED")
			}
			if err := os.RemoveAll(path); err != nil {
				return w.failBeforeSwitch(ctx, err)
			}
			if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
				return w.failBeforeSwitch(ctx, err)
			}
			deadline, cancel := context.WithDeadline(ctx, w.record.Deadline)
			err = w.download(deadline, op.Target, path)
			cancel()
			if err != nil {
				return w.failBeforeSwitch(ctx, issue("RELEASE_DOWNLOAD_FAILED"))
			}
			if err := w.update(ctx, func(r *upgradejob.Record) { r.Operation.Phase = upgrade.Checking }); err != nil {
				return err
			}
		case upgrade.Checking:
			if err := w.prepareSwitch(ctx); err != nil {
				return w.failBeforeSwitch(ctx, err)
			}
		case upgrade.Switching:
			if err := w.switchTarget(ctx); err != nil {
				return w.startRollback(ctx, err)
			}
		case upgrade.Reconnecting:
			if err := w.ensureConnector(ctx, false); err != nil {
				return w.startRollback(ctx, err)
			}
			if err := w.update(ctx, func(r *upgradejob.Record) { r.Operation.Phase = upgrade.Verifying }); err != nil {
				return err
			}
		case upgrade.Verifying:
			deadline := w.record.Operation.AttemptStartedAt.Add(w.targetTimeout)
			if w.record.Deadline.Before(deadline) {
				deadline = w.record.Deadline
			}
			proof, err := w.waitProof(ctx, deadline, false)
			if err != nil {
				return w.startRollback(ctx, err)
			}
			if err := w.update(ctx, func(r *upgradejob.Record) {
				r.Operation.Proof = &proof
				r.Operation.Phase = upgrade.Succeeded
				r.Operation.Confirmed = true
			}); err != nil {
				return err
			}
			if err := w.checkpoint("confirmed"); err != nil {
				return err
			}
			return w.unseal(ctx)
		case upgrade.RollingBack, upgrade.RollbackVerifying:
			return w.rollback(ctx)
		default:
			return issue("UPGRADE_PHASE_UNSUPPORTED")
		}
	}
}

func beforeSwitch(phase upgrade.Phase) bool {
	return phase == upgrade.Queued || phase == upgrade.Preparing || phase == upgrade.Downloading || phase == upgrade.Checking
}

func (w *worker) update(ctx context.Context, change func(*upgradejob.Record)) error {
	record, err := w.jobs.Update(ctx, w.record.Operation.ID, w.record.Owner, w.record.Operation.Revision, func(next *upgradejob.Record) error { change(next); return nil })
	if err == nil {
		w.record = record
	}
	return err
}

func (w *worker) checkpoint(name string) error {
	if w.barrier != nil {
		return w.barrier(name)
	}
	return nil
}

func (w *worker) claimSeal(ctx context.Context) error {
	if w.gate != nil {
		return nil
	}
	var gate *launchgate.Gate
	for {
		var err error
		gate, err = launchgate.Acquire(w.metadata.StateDir, true)
		if !errors.Is(err, launchgate.ErrBusy) {
			if err != nil {
				return err
			}
			break
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(50 * time.Millisecond):
		}
	}
	w.gate = gate
	previous, err := launchgate.ReadSeal(w.metadata.StateDir)
	if err != nil {
		return err
	}
	w.seal = launchgate.Seal{InstallationID: w.metadata.ID, OperationID: w.record.Operation.ID, Owner: w.record.Owner}
	if err := gate.ClaimSeal(previous, w.seal); err != nil {
		return err
	}
	if w.record.Operation.LaunchSealed {
		return nil
	}
	return w.update(ctx, func(r *upgradejob.Record) { r.Operation.LaunchSealed = true })
}

func (w *worker) validateContext(ctx context.Context) error {
	if w.record.Operation.StateContract != statecontract.ID() {
		return issue("STATE_CONTRACT_UNSUPPORTED")
	}
	fingerprint, err := configurationFingerprint(w.metadata.ConfigPath)
	if err != nil || fingerprint != w.record.ConfigurationSHA256 {
		return issue("UPGRADE_CONFIG_CHANGED")
	}
	current, err := w.installed.Read()
	if err != nil || current.Metadata != w.metadata {
		return issue("INSTALLATION_CHANGED")
	}
	binding, err := w.control.Binding(ctx, w.record.Operation.Request.Binding)
	if err != nil {
		return err
	}
	if binding != w.record.Operation.Request.Binding {
		return issue("STALE_BINDING")
	}
	return nil
}

func (w *worker) prepareSwitch(ctx context.Context) error {
	gateCtx, cancel := context.WithTimeout(ctx, 20*time.Second)
	defer cancel()
	if err := w.claimSeal(gateCtx); err != nil {
		return err
	}
	if err := w.validateContext(gateCtx); err != nil {
		return err
	}
	source, err := w.installed.Observe(gateCtx)
	if err != nil {
		return err
	}
	request := w.record.Operation.Request
	if source.Revision != request.ExpectedInstallationRevision || source.ID != request.InstallationID {
		return issue("INSTALLATION_CHANGED")
	}
	observed, err := w.control.Inspect(gateCtx, request.Binding)
	if err != nil {
		return err
	}
	if observed.Installation == nil || observed.Installation.Revision != source.Revision || observed.Running.SHA256 != request.ExpectedRunningSHA256 || observed.Running.SHA256 != source.Release.ProgramSHA256() {
		return issue("INSTALLATION_CHANGED")
	}
	original, err := w.check(gateCtx, filepath.Join(w.root, w.record.Original.Directory, "dune"), w.metadata.ConfigPath, w.record.Original.Manifest)
	if err != nil {
		return err
	}
	target, err := w.check(gateCtx, filepath.Join(w.root, w.record.Candidate.Directory, "dune"), w.metadata.ConfigPath, w.record.Candidate.Manifest)
	if err != nil {
		return err
	}
	if !original.Allowed || !target.Allowed || original.StateContract != statecontract.ID() || target.StateContract != statecontract.ID() {
		return issue("STATE_CONTRACT_UNSUPPORTED")
	}
	// Registrations can retire, but cannot newly launch inside this gate. Each
	// checker independently validates all still-live and accepted participants.
	if err := w.validateContext(gateCtx); err != nil {
		return err
	}
	return w.update(ctx, func(r *upgradejob.Record) {
		r.Operation.Participants = target.Hosts
		r.Operation.AttemptID = wire.ID()
		r.Operation.Challenge = wire.ID()
		r.Operation.AttemptStartedAt = time.Now().UTC()
		r.Operation.Phase = upgrade.Switching
	})
}

func (w *worker) switchTarget(ctx context.Context) error {
	if err := w.validateContext(ctx); err != nil {
		return err
	}
	if err := w.installed.RecoverSwitch(ctx, w.seal); err != nil {
		return err
	}
	current, err := w.installed.Read()
	if err != nil {
		return err
	}
	switch current.Current.Directory {
	case w.record.Original.Directory:
		if w.record.Switched {
			return issue("INSTALLATION_CHANGED")
		}
		if err := w.installed.Switch(ctx, w.seal, w.record.Operation.Request.ExpectedInstallationRevision, w.record.Candidate); err != nil {
			return err
		}
	case w.record.Candidate.Directory:
	default:
		return issue("INSTALLATION_CHANGED")
	}
	if err := w.checkpoint("switched"); err != nil {
		return err
	}
	if err := w.update(ctx, func(r *upgradejob.Record) { r.Switched = true; r.Operation.Phase = upgrade.Reconnecting }); err != nil {
		return err
	}
	return nil
}

func (w *worker) failBeforeSwitch(ctx context.Context, cause error) error {
	current, err := w.installed.Read()
	if err != nil || current.Pending != nil || w.record.Switched {
		return issue("INSTALLATION_RECOVERY_REQUIRED")
	}
	failure := failureIssue(cause, string(w.record.Operation.Phase))
	if err := w.update(ctx, func(r *upgradejob.Record) {
		r.Operation.Failure = &failure
		r.Operation.Phase = upgrade.Failed
		r.Operation.Confirmed = true
	}); err != nil {
		return err
	}
	return w.unseal(ctx)
}

func (w *worker) startRollback(ctx context.Context, cause error) error {
	if ctx.Err() != nil {
		return ctx.Err()
	}
	failure := failureIssue(cause, string(w.record.Operation.Phase))
	if err := w.update(ctx, func(r *upgradejob.Record) {
		r.Operation.Failure = &failure
		r.Operation.Phase = upgrade.RollingBack
		r.Operation.Rollback = upgrade.RollbackRunning
		r.Operation.AttemptID = wire.ID()
		r.Operation.Challenge = wire.ID()
		r.Operation.AttemptStartedAt = time.Now().UTC()
		r.RollbackDeadline = time.Now().UTC().Add(w.rollbackTimeout)
	}); err != nil {
		return err
	}
	return w.rollback(ctx)
}

func (w *worker) rollback(ctx context.Context) error {
	recovery, cancel := context.WithDeadline(ctx, w.record.RollbackDeadline)
	defer cancel()
	if err := w.validateContext(recovery); err != nil {
		return w.block(ctx, err, upgrade.RollbackBlocked)
	}
	if w.record.Operation.Phase == upgrade.RollingBack {
		report, err := w.check(recovery, filepath.Join(w.root, w.record.Original.Directory, "dune"), w.metadata.ConfigPath, w.record.Original.Manifest)
		if err != nil || !report.Allowed {
			return w.block(ctx, issue("ROLLBACK_STATE_UNVERIFIABLE"), upgrade.RollbackBlocked)
		}
		if err := w.installed.RecoverSwitch(recovery, w.seal); err != nil {
			return w.block(ctx, err, upgrade.RollbackBlocked)
		}
		current, err := w.installed.Read()
		if err != nil {
			return w.block(ctx, err, upgrade.RollbackBlocked)
		}
		if current.Current.Directory != w.record.Original.Directory {
			if current.Current.Directory != w.record.Candidate.Directory {
				return w.block(ctx, issue("INSTALLATION_CHANGED"), upgrade.RollbackBlocked)
			}
			if err := w.installed.Restore(recovery, w.seal, w.record.Original, w.record.Operation.Source.Installation.Components); err != nil {
				return w.block(ctx, err, upgrade.RollbackFailed)
			}
		}
		if err := w.update(ctx, func(r *upgradejob.Record) { r.Operation.Phase = upgrade.RollbackVerifying }); err != nil {
			return err
		}
		if err := w.checkpoint("restored"); err != nil {
			return err
		}

	}
	if err := w.ensureConnector(recovery, true); err != nil {
		return w.block(ctx, err, upgrade.RollbackFailed)
	}
	proof, err := w.waitProof(recovery, w.record.RollbackDeadline, true)
	if err != nil {
		return w.block(ctx, err, upgrade.RollbackUnconfirmed)
	}
	if err := w.update(ctx, func(r *upgradejob.Record) {
		r.Operation.Phase = upgrade.Failed
		r.Operation.Confirmed = true
		r.Operation.Rollback = upgrade.RollbackRestored
		r.Operation.Proof = &proof
	}); err != nil {
		return err
	}
	return w.unseal(ctx)
}

func (w *worker) waitProof(ctx context.Context, deadline time.Time, rollback bool) (upgrade.Proof, error) {
	wait, cancel := context.WithDeadline(ctx, deadline)
	defer cancel()
	op := w.record.Operation
	probe := upgrade.Probe{Binding: op.Request.Binding, InstallationID: op.Request.InstallationID, OperationID: op.ID, AttemptID: op.AttemptID, Challenge: op.Challenge}
	var last error = issue("PLATFORM_CONFIRMATION_TIMEOUT")
	for wait.Err() == nil {
		proof, err := w.control.Confirm(wait, probe)
		if err == nil {
			err = upgradejob.ValidateProof(op, proof, rollback)
			if err == nil {
				err = w.validateProofInstallation(wait, proof, rollback)
			}
			if err == nil {
				return proof, nil
			}
		}
		last = err
		select {
		case <-wait.Done():
		case <-time.After(w.poll):
		}
	}
	return upgrade.Proof{}, last
}

func (w *worker) validateProofInstallation(ctx context.Context, proof upgrade.Proof, rollback bool) error {
	if err := w.validateContext(ctx); err != nil {
		return err
	}
	observed, err := w.installed.Observe(ctx)
	if err != nil {
		return err
	}
	digest, err := observed.Release.Digest()
	if err != nil || digest != proof.ManifestSHA256 || observed.Revision != proof.InstallationRevision {
		return issue("INSTALLATION_CHANGED")
	}
	if rollback {
		if !sameComponents(observed.Components, w.record.Operation.Source.Installation.Components) {
			return issue("ROLLBACK_FILES_CHANGED")
		}
	} else if !observed.Complete {
		return issue("INSTALLATION_INCOMPLETE")
	}
	return nil
}
func sameComponents(a, b []upgrade.ComponentObservation) bool {
	// Both observations are in frozen manifest order. Matches is meaningful here
	// because the original and restored distribution share exactly that manifest.
	return reflect.DeepEqual(a, b)
}

func (w *worker) block(ctx context.Context, cause error, state upgrade.RollbackState) error {
	if ctx.Err() != nil {
		return ctx.Err()
	}
	failure := failureIssue(cause, string(w.record.Operation.Phase))
	return w.update(ctx, func(r *upgradejob.Record) {
		r.Operation.Phase = upgrade.RecoveryBlocked
		r.Operation.Rollback = state
		r.Operation.RollbackFailure = &failure
	})
}

func (w *worker) unseal(ctx context.Context) error {
	if !w.record.Operation.Confirmed {
		return fmt.Errorf("cannot release unfinished upgrade")
	}
	if !w.record.Operation.LaunchSealed {
		return nil
	}
	if err := w.checkpoint("unseal"); err != nil {
		return err
	}
	if err := w.gate.ClearSeal(w.seal); err != nil {
		return err
	}
	record, err := w.jobs.ReleaseSeal(ctx, w.record.Operation.ID, w.record.Owner, w.record.Operation.Revision)
	if err == nil {
		w.record = record
	}
	return err
}
func failureIssue(err error, stage string) upgrade.Issue {
	code := "UPGRADE_STAGE_FAILED"
	var protocol *api.Error
	if errors.As(err, &protocol) && api.ValidateSubmissionID(protocol.Code) == nil {
		code = protocol.Code
	}
	return upgrade.Issue{Code: code, Stage: stage}
}
func configurationFingerprint(path string) (string, error) {
	cfg, err := config.Load(path)
	if err != nil {
		return "", err
	}
	hash := sha256.New()
	for _, file := range []string{path, cfg.Certificate, cfg.Key} {
		if file == "" {
			continue
		}
		data, err := os.ReadFile(file)
		if err != nil || len(data) > 1<<20 {
			return "", issue("UPGRADE_CONFIG_UNAVAILABLE")
		}
		fmt.Fprintf(hash, "%d:", len(data))
		hash.Write(data)
	}
	return hex.EncodeToString(hash.Sum(nil)), nil
}

// A recovered restart checkpoint first observes the original routed connector.
// If the desired process is already serving, recovery does not restart it again.
func (w *worker) ensureConnector(ctx context.Context, rollback bool) error {
	expected := w.record.Operation.Target.ProgramSHA256()
	if rollback {
		expected = w.record.Original.Manifest.ProgramSHA256()
	}
	observed, err := w.control.Inspect(ctx, w.record.Operation.Request.Binding)
	if err == nil && observed.Running.SHA256 == expected && (rollback || observed.Running.StartID != w.record.Operation.Source.Running.StartID) {
		return nil
	}
	restartCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	if err := w.restart(restartCtx, w.metadata); err != nil {
		return issue("CONNECTOR_RESTART_FAILED")
	}
	return w.checkpoint("restarted")
}
