package runnerupgrade

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/aiomni/dune/internal/installation"
	"github.com/aiomni/dune/internal/launchgate"
	"github.com/aiomni/dune/internal/release"
	"github.com/aiomni/dune/internal/statecontract"
	"github.com/aiomni/dune/internal/upgradejob"
	"github.com/aiomni/dune/internal/wire"
	"github.com/aiomni/dune/pkg/api"
	"github.com/aiomni/dune/pkg/runner"
	"github.com/aiomni/dune/pkg/upgrade"
)

type workerFixture struct {
	t                *testing.T
	worker           *worker
	source           installation.Location
	target           upgrade.Manifest
	template         string
	binding          runner.Binding
	running          api.RunningProgram
	started          *upgrade.Probe
	operation        upgrade.Operation
	restarts         int
	checks           int
	rejectTarget     bool
	rejectAll        bool
	mutateAfterProof bool
}

func writeDistribution(t *testing.T, path, id string) upgrade.Manifest {
	t.Helper()
	manifest := upgrade.Manifest{ID: id, Platform: upgrade.Platform{OS: "linux", Arch: "amd64"}, ArchiveURL: "https://example.test/" + id, ArchiveSHA256: strings.Repeat("a", 64), StateContract: statecontract.ID()}
	for _, name := range []string{"dune", "tmux", "rg", "licenses/NOTICE"} {
		mode := os.FileMode(0700)
		if strings.HasPrefix(name, "licenses/") {
			mode = 0600
		}
		body := []byte(id + "-" + name)
		hash := sha256.Sum256(body)
		manifest.Components = append(manifest.Components, upgrade.Component{Path: name, SHA256: hex.EncodeToString(hash[:]), Bytes: int64(len(body)), Mode: uint32(mode)})
		file := filepath.Join(path, name)
		if err := os.MkdirAll(filepath.Dir(file), 0700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(file, body, mode); err != nil {
			t.Fatal(err)
		}
	}
	return manifest
}
func newWorkerFixture(t *testing.T, auxiliaryOnly ...bool) *workerFixture {
	t.Helper()
	root := t.TempDir()
	if err := os.Chmod(root, 0700); err != nil {
		t.Fatal(err)
	}
	stateDir := filepath.Join(root, "state")
	if err := os.Mkdir(stateDir, 0700); err != nil {
		t.Fatal(err)
	}
	configPath := filepath.Join(root, "config.yaml")
	config := fmt.Sprintf("gateway: ws://127.0.0.1:7443/api/v1/ws/tunnel\ntoken: %s\ntarget: machine\nsession_dir: %s\nupgrade_control_url: https://example.test/api/v1/runner-upgrade-control\n", strings.Repeat("a", 64), stateDir)
	if err := os.WriteFile(configPath, []byte(config), 0600); err != nil {
		t.Fatal(err)
	}
	source := installation.Location{Directory: "releases/" + wire.ID()}
	source.Manifest = writeDistribution(t, filepath.Join(root, source.Directory), "v1")
	template := filepath.Join(t.TempDir(), "template")
	target := writeDistribution(t, template, "v2")
	if len(auxiliaryOnly) != 0 && auxiliaryOnly[0] {
		target.Components[0] = source.Manifest.Components[0]
		body, err := os.ReadFile(filepath.Join(root, source.Directory, "dune"))
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(template, "dune"), body, 0700); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.Symlink(source.Directory, filepath.Join(root, "current")); err != nil {
		t.Fatal(err)
	}
	store, err := installation.Lock(root)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { store.Close() })
	metadata := installation.Metadata{ID: "installation", Method: "service", ConfigPath: configPath, StateDir: stateDir, ServiceName: "test-connector"}
	if _, err := store.Initialize(t.Context(), metadata, source); err != nil {
		t.Fatal(err)
	}
	jobs, err := upgradejob.Open(t.Context(), filepath.Join(root, "upgrades"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { jobs.Close() })
	binding := runner.Binding{RunnerID: "runner", MachineID: "machine", FabricID: "fabric", Revision: 1}
	f := &workerFixture{t: t, source: source, target: target, template: template, binding: binding}
	f.running = fixtureProgram(source.Manifest, "original-process")
	w := &worker{root: root, installed: store, jobs: jobs, metadata: metadata, control: f, poll: time.Millisecond, targetTimeout: time.Second, rollbackTimeout: 2 * time.Second}
	f.worker = w
	w.download = func(ctx context.Context, m upgrade.Manifest, destination string) error {
		observed, complete, err := release.Observe(ctx, f.template, m.Components)
		if err != nil {
			return err
		}
		if !complete {
			return fmt.Errorf("bad test template")
		}
		return release.Copy(ctx, f.template, destination, observed)
	}
	w.check = func(ctx context.Context, program, config string, m upgrade.Manifest) (api.UpgradeReport, error) {
		f.checks++
		return api.UpgradeReport{Allowed: true, StateContract: statecontract.ID(), Program: fixtureProgram(m, "checker"), Hosts: []api.UpgradeHost{}}, nil
	}
	w.restart = func(ctx context.Context, m installation.Metadata) error {
		f.restarts++
		state, err := store.Read()
		if err != nil {
			return err
		}
		f.running = fixtureProgram(state.Current.Manifest, fmt.Sprintf("restart-%d", f.restarts))
		probe := f.worker.record.Operation.Probe()
		f.started = &probe
		return nil
	}
	observed, err := store.Observe(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	sourceView := upgrade.Inspection{Binding: binding, Installation: &observed, Running: f.running, Supported: true}
	digest, _ := target.Digest()
	request := upgrade.Request{SubmissionID: "request", Binding: binding, InstallationID: metadata.ID, ExpectedInstallationRevision: observed.Revision, ExpectedRunningSHA256: f.running.SHA256, Release: upgrade.ReleaseRef{ID: target.ID, ManifestSHA256: digest}}
	configurationSHA, err := configurationFingerprint(configPath)
	if err != nil {
		t.Fatal(err)
	}
	f.operation, err = jobs.Admit(t.Context(), request, target, sourceView, configurationSHA)
	if err != nil {
		t.Fatal(err)
	}
	return f
}
func fixtureProgram(m upgrade.Manifest, start string) api.RunningProgram {
	var bytes int64
	for _, c := range m.Components {
		if c.Path == "dune" {
			bytes = c.Bytes
		}
	}
	return api.RunningProgram{PID: 100, StartID: start, SHA256: m.ProgramSHA256(), Bytes: bytes, Build: api.BuildInfo{OS: m.Platform.OS, Arch: m.Platform.Arch}, ObservedAt: time.Now().UTC()}
}
func (f *workerFixture) Binding(ctx context.Context, expected runner.Binding) (runner.Binding, error) {
	if err := ctx.Err(); err != nil {
		return runner.Binding{}, err
	}
	if expected != f.binding {
		return runner.Binding{}, issue("STALE_BINDING")
	}
	return f.binding, nil
}
func (f *workerFixture) Inspect(ctx context.Context, binding runner.Binding) (upgrade.Inspection, error) {
	observed, err := installation.View(ctx, f.worker.root)
	return upgrade.Inspection{Binding: binding, Installation: &observed, Running: f.running, Supported: true, StartedForUpgrade: f.started}, err
}
func (f *workerFixture) Confirm(ctx context.Context, probe upgrade.Probe) (upgrade.Proof, error) {
	observed, err := installation.View(ctx, f.worker.root)
	if err != nil {
		return upgrade.Proof{}, err
	}
	if f.rejectAll || (f.rejectTarget && observed.Release.ID == f.target.ID) {
		return upgrade.Proof{}, issue("GATEWAY_REGISTRATION_REJECTED")
	}
	digest, _ := observed.Release.Digest()
	restored := observed.Release.ID == f.source.Manifest.ID
	proof := upgrade.Proof{OperationID: probe.OperationID, AttemptID: probe.AttemptID, Challenge: probe.Challenge, Binding: probe.Binding, InstallationID: observed.ID, InstallationRevision: observed.Revision, ManifestSHA256: digest, Running: f.running, Incarnation: "incarnation", ConnectionGeneration: 1, RouteEpoch: 1, ReleaseVerified: observed.Complete, OriginalInstallationRestored: restored, GatewayAccepted: true, Routed: true, ObservedAt: time.Now().UTC()}
	proof.StartedForAttempt = f.started != nil && *f.started == probe
	if f.mutateAfterProof {
		f.mutateAfterProof = false
		if err := os.WriteFile(filepath.Join(f.worker.root, "current", "rg"), []byte("externally changed"), 0700); err != nil {
			f.t.Fatal(err)
		}
	}
	return proof, nil
}
func (f *workerFixture) result() upgrade.Operation {
	f.t.Helper()
	op, err := f.worker.jobs.Get(f.t.Context(), upgrade.Query{Binding: f.operation.Request.Binding, InstallationID: f.operation.Request.InstallationID, OperationID: f.operation.ID})
	if err != nil {
		f.t.Fatal(err)
	}
	return op
}
func (f *workerFixture) assertUnsealed() {
	f.t.Helper()
	seal, err := launchgate.ReadSeal(f.worker.metadata.StateDir)
	if err != nil || seal != nil {
		f.t.Fatal("terminal work kept seal", seal, err)
	}
	gate, err := launchgate.Acquire(f.worker.metadata.StateDir, false)
	if err != nil {
		f.t.Fatal("launch still blocked", err)
	}
	gate.Close()
}

func TestWorkerTargetSuccessRequiresDurableRoutedConfirmation(t *testing.T) {
	f := newWorkerFixture(t)
	if err := f.worker.execute(t.Context(), f.operation.ID); err != nil {
		t.Fatal(err)
	}
	op := f.result()
	if op.Phase != upgrade.Succeeded || !op.Confirmed || op.LaunchSealed || op.Proof == nil || f.restarts != 1 || f.checks != 2 {
		t.Fatalf("unexpected result: %+v restarts=%d checks=%d", op, f.restarts, f.checks)
	}
	f.assertUnsealed()
	duplicate, found, err := f.worker.jobs.Existing(t.Context(), f.operation.Request)
	if err != nil || !found || duplicate.ID != op.ID || duplicate.Phase != op.Phase {
		t.Fatal(duplicate, found, err)
	}
	if err := f.worker.execute(t.Context(), op.ID); err == nil {
		t.Fatal("confirmed operation executed again")
	}
	if f.restarts != 1 {
		t.Fatal("duplicate restart")
	}
}

func TestWorkerGatewayRejectsTargetThenRestoresOriginal(t *testing.T) {
	f := newWorkerFixture(t)
	f.rejectTarget = true
	marker := filepath.Join(f.worker.metadata.StateDir, "new-submission")
	restart := f.worker.restart
	f.worker.restart = func(ctx context.Context, m installation.Metadata) error {
		if err := os.WriteFile(marker, []byte("accepted-during-upgrade"), 0600); err != nil {
			return err
		}
		return restart(ctx, m)
	}
	if err := f.worker.execute(t.Context(), f.operation.ID); err != nil {
		t.Fatal(err)
	}
	op := f.result()
	if op.Phase != upgrade.Failed || !op.Confirmed || op.Rollback != upgrade.RollbackRestored || op.Failure.Code != "GATEWAY_REGISTRATION_REJECTED" || f.restarts != 2 {
		t.Fatalf("unexpected rollback: %+v restarts=%d", op, f.restarts)
	}
	restored, err := f.worker.installed.Observe(t.Context())
	if err != nil || restored.Release.ID != f.source.Manifest.ID || restored.Revision != "3" {
		t.Fatal(restored, err)
	}
	data, err := os.ReadFile(marker)
	if err != nil || string(data) != "accepted-during-upgrade" {
		t.Fatal("rollback discarded new record", err)
	}
	f.assertUnsealed()
}

func TestWorkerRecoveryDoesNotRepeatCompletedSwitchOrRestart(t *testing.T) {
	for _, point := range []string{"switched", "restarted", "confirmed"} {
		t.Run(point, func(t *testing.T) {
			f := newWorkerFixture(t)
			ctx, cancel := context.WithCancel(t.Context())
			f.worker.barrier = func(stage string) error {
				if stage == point {
					cancel()
					return context.Canceled
				}
				return nil
			}
			if err := f.worker.execute(ctx, f.operation.ID); !errors.Is(err, context.Canceled) {
				t.Fatal(err)
			}
			gate, err := launchgate.Acquire(f.worker.metadata.StateDir, false)
			if err == nil {
				gate.Close()
				t.Fatal("worker death reopened launches")
			}
			f.worker.barrier = nil
			f.worker.gate = nil
			if err := f.worker.execute(t.Context(), f.operation.ID); err != nil {
				t.Fatal(err)
			}
			op := f.result()
			if !op.Confirmed || op.Phase != upgrade.Succeeded || f.restarts != 1 {
				t.Fatalf("recovery replayed effects: %+v restarts=%d", op, f.restarts)
			}
			f.assertUnsealed()
		})
	}
}

func TestWorkerRechecksExternalAuxiliaryChangeAfterDownload(t *testing.T) {
	f := newWorkerFixture(t)
	download := f.worker.download
	f.worker.download = func(ctx context.Context, m upgrade.Manifest, path string) error {
		if err := download(ctx, m, path); err != nil {
			return err
		}
		return os.WriteFile(filepath.Join(f.worker.root, f.source.Directory, "rg"), []byte("new-rg"), 0700)
	}
	if err := f.worker.execute(t.Context(), f.operation.ID); err != nil {
		t.Fatal(err)
	}
	op := f.result()
	if op.Phase != upgrade.Failed || op.Failure.Code != "INSTALLATION_CHANGED" || op.Rollback != upgrade.RollbackNotNeeded || f.restarts != 0 {
		t.Fatalf("stale installation switched: %+v", op)
	}
	f.assertUnsealed()
}

func TestWorkerUnconfirmedRollbackRetainsSealAndDoesNotLoop(t *testing.T) {
	f := newWorkerFixture(t)
	f.rejectAll = true
	if err := f.worker.execute(t.Context(), f.operation.ID); err != nil {
		t.Fatal(err)
	}
	op := f.result()
	if op.Confirmed || op.Phase != upgrade.RecoveryBlocked || op.Rollback != upgrade.RollbackUnconfirmed || !op.LaunchSealed || op.RollbackFailure == nil {
		t.Fatalf("false restored result: %+v", op)
	}
	if _, err := launchgate.Acquire(f.worker.metadata.StateDir, false); !errors.Is(err, launchgate.ErrBusy) {
		t.Fatal(err)
	}
	if f.restarts != 2 {
		t.Fatal("unexpected repeated restarts", f.restarts)
	}
}

func TestWorkerRechecksInstallationAfterReceivingProof(t *testing.T) {
	f := newWorkerFixture(t)
	f.mutateAfterProof = true
	if err := f.worker.execute(t.Context(), f.operation.ID); err != nil {
		t.Fatal(err)
	}
	op := f.result()
	if op.Phase != upgrade.Failed || op.Rollback != upgrade.RollbackRestored {
		t.Fatalf("stale proof committed: %+v", op)
	}
	f.assertUnsealed()
}

func TestWorkerRefusesUnsafeSharedStateBeforeStopAndAfterTargetWrites(t *testing.T) {
	for _, when := range []string{"before-switch", "rollback"} {
		t.Run(when, func(t *testing.T) {
			f := newWorkerFixture(t)
			original := f.worker.check
			f.worker.check = func(ctx context.Context, exe, path string, m upgrade.Manifest) (api.UpgradeReport, error) {
				if (when == "before-switch" && m.ID == f.target.ID) || (when == "rollback" && f.restarts > 0 && m.ID == f.source.Manifest.ID) {
					return api.UpgradeReport{}, issue("STATE_CONTRACT_UNSUPPORTED")
				}
				return original(ctx, exe, path, m)
			}
			f.rejectTarget = when == "rollback"
			if err := f.worker.execute(t.Context(), f.operation.ID); err != nil {
				t.Fatal(err)
			}
			op := f.result()
			if when == "before-switch" {
				if !op.Confirmed || op.Phase != upgrade.Failed || f.restarts != 0 {
					t.Fatal(op.Phase, op.Confirmed, f.restarts)
				}
				f.assertUnsealed()
			} else {
				if op.Confirmed || op.Phase != upgrade.RecoveryBlocked || op.Rollback != upgrade.RollbackBlocked || f.restarts != 1 {
					t.Fatal(op.Phase, op.Rollback, f.restarts)
				}
				state, err := f.worker.installed.Read()
				if err != nil || state.Current.Manifest.ID != f.target.ID {
					t.Fatal("unsafe source restored", state, err)
				}
			}
		})
	}
}

func TestExplicitRecoveryFencesRetriesAndCleansOnlyConvergedMaterials(t *testing.T) {
	f := newWorkerFixture(t)
	f.rejectAll = true
	if err := f.worker.execute(t.Context(), f.operation.ID); err != nil {
		t.Fatal(err)
	}
	blocked := f.result()
	candidate := filepath.Join(f.worker.root, f.worker.record.Candidate.Directory)
	if _, err := os.Stat(candidate); err != nil {
		t.Fatal("unconfirmed recovery lost its target material", err)
	}
	oldAttempt := blocked.AttemptID
	resumed, err := f.worker.jobs.ResumeRecovery(t.Context(), blocked.ID, f.worker.record.Owner, blocked.Revision)
	if err != nil || resumed.Operation.AttemptID == oldAttempt || resumed.Operation.Failure.Code != blocked.Failure.Code {
		t.Fatal("manual recovery lost original failure or attempt fencing", resumed, err)
	}
	if _, err := f.worker.jobs.ResumeRecovery(t.Context(), blocked.ID, f.worker.record.Owner, blocked.Revision); err == nil {
		t.Fatal("duplicate recovery extended the retry budget")
	}
	f.rejectAll = false
	f.worker.gate = nil
	if err := f.worker.execute(t.Context(), blocked.ID); err != nil {
		t.Fatal(err)
	}
	result := f.result()
	if result.Rollback != upgrade.RollbackRestored || !result.Confirmed || f.restarts != 2 {
		t.Fatal(result, f.restarts)
	}
	if _, err := os.Stat(candidate); !os.IsNotExist(err) {
		t.Fatal("converged candidate retained", err)
	}
	if _, err := os.Stat(filepath.Join(f.worker.root, f.source.Directory, "dune")); err != nil {
		t.Fatal("selected original removed", err)
	}
	f.assertUnsealed()
}

func TestSetupFailureAfterSwitchIsDurablyBlocked(t *testing.T) {
	f := newWorkerFixture(t)
	ctx, cancel := context.WithCancel(t.Context())
	f.worker.barrier = func(stage string) error {
		if stage == "switched" {
			cancel()
			return context.Canceled
		}
		return nil
	}
	if err := f.worker.execute(ctx, f.operation.ID); !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
	active, err := f.worker.jobs.Active(t.Context())
	if err != nil || active == nil {
		t.Fatal(active, err)
	}
	if err := recordSetupFailure(t.Context(), f.worker.root, f.worker.installed, f.worker.jobs, f.worker.metadata, *active, issue("UPGRADE_CONFIG_UNAVAILABLE")); err != nil {
		t.Fatal(err)
	}
	result := f.result()
	if result.Phase != upgrade.RecoveryBlocked || !result.LaunchSealed || result.Confirmed || result.Failure == nil || result.RollbackFailure == nil {
		t.Fatal(result)
	}
}

func TestRecoveryRemovesItsInterruptedArchiveBeforeDownloadingAgain(t *testing.T) {
	f := newWorkerFixture(t)
	download := f.worker.download
	ctx, cancel := context.WithCancel(t.Context())
	f.worker.download = func(ctx context.Context, m upgrade.Manifest, path string) error {
		if err := os.WriteFile(path+".archive", []byte("partial archive"), 0600); err != nil {
			return err
		}
		cancel()
		return context.Canceled
	}
	if err := f.worker.execute(ctx, f.operation.ID); err == nil {
		t.Fatal("interrupted download became a result")
	}
	f.worker.download = download
	if err := f.worker.execute(t.Context(), f.operation.ID); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(f.worker.root, f.worker.record.Candidate.Directory) + ".archive"
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatal("orphan archive retained", err)
	}
	if result := f.result(); result.Phase != upgrade.Succeeded {
		t.Fatal(result)
	}
}

func TestAuxiliaryUpdateRequiresThisAttemptStartup(t *testing.T) {
	for _, rollback := range []bool{false, true} {
		t.Run(fmt.Sprintf("rollback-%t", rollback), func(t *testing.T) {
			f := newWorkerFixture(t, true)
			f.rejectTarget = rollback
			// An unrelated restart after admission must not discharge this job's
			// dependency-directory restart, even though Dune's digest is equal.
			f.running.StartID = "external-restart-before-switch"
			if !f.operation.RequiresStartupEvidence() {
				t.Fatal("test must exercise equal-image dependency update")
			}
			if err := f.worker.execute(t.Context(), f.operation.ID); err != nil {
				t.Fatal(err)
			}
			op := f.result()
			wantRestarts := 1
			if rollback {
				wantRestarts = 2
			}
			if !op.Confirmed || f.restarts != wantRestarts || op.Proof == nil || !op.Proof.StartedForAttempt {
				t.Fatalf("attempt startup missing: %+v restarts=%d", op, f.restarts)
			}
			proof := *op.Proof
			proof.StartedForAttempt = false
			if err := upgradejob.ValidateProof(op, proof, rollback); err == nil {
				t.Fatal("equal SHA and changed StartID accepted without startup evidence")
			}
		})
	}
}

func TestSwitchIntentFailureDoesNotRestartUntouchedSource(t *testing.T) {
	f := newWorkerFixture(t)
	check := f.worker.check
	f.worker.check = func(ctx context.Context, executable, path string, m upgrade.Manifest) (api.UpgradeReport, error) {
		report, err := check(ctx, executable, path, m)
		if m.ID == f.target.ID {
			// Model an external auxiliary change immediately before Switch's
			// complete-source comparison. No target file has become selected.
			if err := os.WriteFile(filepath.Join(f.worker.root, f.source.Directory, "rg"), []byte("external"), 0700); err != nil {
				t.Fatal(err)
			}
		}
		return report, err
	}
	if err := f.worker.execute(t.Context(), f.operation.ID); err != nil {
		t.Fatal(err)
	}
	op := f.result()
	if !op.Confirmed || op.Phase != upgrade.Failed || op.Rollback != upgrade.RollbackNotNeeded || f.restarts != 0 {
		t.Fatalf("untouched source restarted: %+v restarts=%d", op, f.restarts)
	}
	f.assertUnsealed()
}

func TestOfflineStatusPreservesOriginalCheckpointWithoutConfig(t *testing.T) {
	f := newWorkerFixture(t)
	if err := os.Remove(f.worker.metadata.ConfigPath); err != nil {
		t.Fatal(err)
	}
	for _, selector := range []string{"", f.operation.ID} {
		observed, err := Status(t.Context(), f.worker.root, selector)
		if err != nil || observed.ID != f.operation.ID || observed.Revision != f.operation.Revision || observed.Confirmed {
			t.Fatal("diagnosis changed execution", observed, err)
		}
	}
	if _, err := Status(t.Context(), f.worker.root, "missing"); err == nil {
		t.Fatal("invented local operation")
	}
	if f.restarts != 0 || f.result().Revision != f.operation.Revision {
		t.Fatal("local diagnosis restarted work")
	}
}

func TestTerminalCleanupSurvivesLostConfiguration(t *testing.T) {
	f := newWorkerFixture(t)
	ctx, cancel := context.WithCancel(t.Context())
	f.worker.barrier = func(stage string) error {
		if stage == "confirmed" {
			cancel()
			return context.Canceled
		}
		return nil
	}
	if err := f.worker.execute(ctx, f.operation.ID); !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
	if err := os.Remove(f.worker.metadata.ConfigPath); err != nil {
		t.Fatal(err)
	}
	if err := f.worker.installed.Close(); err != nil {
		t.Fatal(err)
	}
	if err := Run(t.Context(), f.worker.root); err != nil {
		t.Fatal("confirmed cleanup depended on missing config", err)
	}
	if op := f.result(); op.Phase != upgrade.Succeeded || !op.Confirmed || op.LaunchSealed {
		t.Fatal(op)
	}
	f.assertUnsealed()
}
