package upgradejob

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/aiomni/dune/internal/statecontract"
	"github.com/aiomni/dune/pkg/api"
	"github.com/aiomni/dune/pkg/runner"
	"github.com/aiomni/dune/pkg/upgrade"
)

func digestText(text string) string {
	digest := sha256.Sum256([]byte(text))
	return hex.EncodeToString(digest[:])
}

func manifest(id string) upgrade.Manifest {
	m := upgrade.Manifest{ID: id, Platform: upgrade.Platform{OS: "linux", Arch: "amd64"}, ArchiveURL: "https://releases.example/" + id, ArchiveSHA256: digestText(id), StateContract: statecontract.ID()}
	for _, path := range []string{"dune", "tmux", "rg", "licenses/NOTICE"} {
		content := path + id
		mode := uint32(0700)
		if path == "dune" {
			content = "same Dune for all three releases"
		}
		if path == "licenses/NOTICE" {
			mode = 0600
		}
		m.Components = append(m.Components, upgrade.Component{Path: path, SHA256: digestText(content), Bytes: int64(len(content)), Mode: mode})
	}
	return m
}

func inspection(m upgrade.Manifest, revision string) upgrade.Inspection {
	observations := make([]upgrade.ComponentObservation, 0, len(m.Components))
	for _, c := range m.Components {
		observations = append(observations, upgrade.ComponentObservation{Path: c.Path, SHA256: c.SHA256, Bytes: c.Bytes, Mode: c.Mode, Present: true, Matches: true})
	}
	return upgrade.Inspection{RunningFromSelectedRelease: true, Supported: true, Binding: runner.Binding{RunnerID: "runner", MachineID: "machine", FabricID: "fabric", Revision: 1}, Running: api.RunningProgram{PID: 101, StartID: "source-process", SHA256: m.ProgramSHA256()}, Installation: &upgrade.Installation{ID: "installation", Revision: revision, Method: "service", Release: m, Components: observations, Complete: true}}
}

func jobFixture(t *testing.T) (*Store, upgrade.Request, upgrade.Manifest, upgrade.Inspection) {
	t.Helper()
	store, err := Open(t.Context(), filepath.Join(t.TempDir(), "jobs"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { store.Close() })
	source, target := inspection(manifest("R1"), "1"), manifest("R2")
	digest, err := target.Digest()
	if err != nil {
		t.Fatal(err)
	}
	request := upgrade.Request{SubmissionID: "caller-key", Binding: source.Binding, InstallationID: "installation", ExpectedInstallationRevision: "1", ExpectedRunningSHA256: source.Running.SHA256, Release: upgrade.ReleaseRef{ID: target.ID, ManifestSHA256: digest}}
	return store, request, target, source
}

func TestAdmissionKeepsOriginalKeyBeforeRecheckingSource(t *testing.T) {
	s, request, target, source := jobFixture(t)
	original, err := s.Admit(t.Context(), upgrade.Submission{Request: request, ReservedAt: time.Now().UTC()}, target, source, strings.Repeat("c", 64))
	if err != nil || original.Admission != api.SubmissionAccepted || original.Confirmed {
		t.Fatal(original, err)
	}
	changed := inspection(manifest("R3"), "7")
	duplicate, err := s.Admit(t.Context(), upgrade.Submission{Request: request, ReservedAt: time.Now().UTC()}, target, changed, strings.Repeat("c", 64))
	if err != nil || duplicate.ID != original.ID || duplicate.Revision != original.Revision {
		t.Fatal("duplicate replaced original admission", duplicate, err)
	}
	conflicting := request
	conflicting.ExpectedInstallationRevision = "7"
	if _, err := s.Admit(t.Context(), upgrade.Submission{Request: conflicting, ReservedAt: time.Now().UTC()}, target, changed, strings.Repeat("c", 64)); err == nil {
		t.Fatal("same key changed request")
	}
	second := request
	second.SubmissionID = "other-client"
	rejected, err := s.Admit(t.Context(), upgrade.Submission{Request: second, ReservedAt: time.Now().UTC()}, target, source, strings.Repeat("c", 64))
	if err != nil || rejected.Admission != api.SubmissionNotAccepted || rejected.ActiveOperationID != original.ID || rejected.Failure.Code != "UPGRADE_CONFLICT" {
		t.Fatal(rejected, err)
	}
	page, err := s.List(t.Context(), request.Binding, request.InstallationID, "", 10)
	if err != nil || page.Active == nil || page.Active.ID != original.ID || len(page.Items) != 1 {
		t.Fatal(page, err)
	}
}

func TestStaleHelperRevisionConflictsUnlessTargetIsFullyCurrent(t *testing.T) {
	for _, installed := range []string{"R3", "R2"} {
		t.Run(installed, func(t *testing.T) {
			s, request, target, _ := jobFixture(t)
			source := inspection(manifest(installed), "9")
			operation, err := s.Admit(t.Context(), upgrade.Submission{Request: request, ReservedAt: time.Now().UTC()}, target, source, strings.Repeat("c", 64))
			if err != nil {
				t.Fatal(err)
			}
			if installed == "R2" {
				if operation.Phase != upgrade.AlreadyCurrent || !operation.Confirmed || operation.Plan.ConnectorRestartRequired {
					t.Fatal(operation)
				}
			} else if operation.Admission != api.SubmissionNotAccepted || operation.Failure.Code != "INSTALLATION_CHANGED" {
				t.Fatal(operation)
			}
			// Repairing the source later must not reactivate a rejected original key.
			again, err := s.Admit(t.Context(), upgrade.Submission{Request: request, ReservedAt: time.Now().UTC()}, target, inspection(target, "10"), strings.Repeat("c", 64))
			if err != nil || again.ID != operation.ID || again.Phase != operation.Phase {
				t.Fatal(again, err)
			}
		})
	}
}

func TestConcurrentAdmissionsHaveOnlyOneExecutor(t *testing.T) {
	s, request, target, source := jobFixture(t)
	other, err := Open(t.Context(), s.directory)
	if err != nil {
		t.Fatal(err)
	}
	defer other.Close()
	var wg sync.WaitGroup
	results := make(chan upgrade.Operation, 12)
	errors := make(chan error, 12)
	for i := range 12 {
		wg.Go(func() {
			own := request
			own.SubmissionID = fmt.Sprintf("client-%d", i)
			store := s
			if i%2 == 1 {
				store = other
			}
			result, err := store.Admit(t.Context(), upgrade.Submission{Request: own, ReservedAt: time.Now().UTC()}, target, source, strings.Repeat("c", 64))
			results <- result
			errors <- err
		})
	}
	wg.Wait()
	close(results)
	close(errors)
	for err := range errors {
		if err != nil {
			t.Fatal(err)
		}
	}
	accepted := 0
	for op := range results {
		if op.Admission == api.SubmissionAccepted {
			accepted++
		}
	}
	if accepted != 1 {
		t.Fatal("competing upgrade executors", accepted)
	}
}

func TestReopenFindsOriginalAdmissionWithoutStartingAnotherJob(t *testing.T) {
	s, request, target, source := jobFixture(t)
	original, err := s.Admit(t.Context(), upgrade.Submission{Request: request, ReservedAt: time.Now().UTC()}, target, source, strings.Repeat("c", 64))
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	reopened, err := Open(t.Context(), s.directory)
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	operation, err := reopened.Get(t.Context(), upgrade.Query{Binding: request.Binding, InstallationID: request.InstallationID, SubmissionID: request.SubmissionID})
	if err != nil || operation.ID != original.ID || operation.Revision != original.Revision || operation.Phase != upgrade.Queued {
		t.Fatal(operation, err)
	}
	byID, err := reopened.Get(t.Context(), upgrade.Query{Binding: request.Binding, InstallationID: request.InstallationID, OperationID: original.ID})
	if err != nil || byID.ID != original.ID {
		t.Fatal(byID, err)
	}
	existing, found, err := reopened.Existing(t.Context(), request)
	if err != nil || !found || existing.ID != original.ID {
		t.Fatal(existing, found, err)
	}
}

func verifying(t *testing.T, s *Store, request upgrade.Request, target upgrade.Manifest, source upgrade.Inspection) Record {
	t.Helper()
	op, err := s.Admit(t.Context(), upgrade.Submission{Request: request, ReservedAt: time.Now().UTC()}, target, source, strings.Repeat("c", 64))
	if err != nil {
		t.Fatal(err)
	}
	record, err := s.Claim(t.Context(), op.ID)
	if err != nil {
		t.Fatal(err)
	}
	for _, phase := range []upgrade.Phase{upgrade.Preparing, upgrade.Downloading, upgrade.Checking, upgrade.Switching, upgrade.Reconnecting, upgrade.Verifying} {
		record, err = s.Update(t.Context(), op.ID, record.Owner, record.Operation.Revision, func(r *Record) error {
			r.Operation.Phase = phase
			if phase == upgrade.Switching {
				r.Operation.LaunchSealed = true
				r.Operation.AttemptID, r.Operation.Challenge, r.Operation.AttemptStartedAt = "target-attempt", "fresh-challenge", time.Now().UTC()
			}
			if phase == upgrade.Reconnecting {
				r.Switched = true
			}
			return nil
		})
		if err != nil {
			t.Fatal(phase, err)
		}
	}
	return record
}

func proofFor(t *testing.T, operation upgrade.Operation, target upgrade.Manifest) upgrade.Proof {
	t.Helper()
	digest, err := target.Digest()
	if err != nil {
		t.Fatal(err)
	}
	var size int64
	for _, component := range target.Components {
		if component.Path == "dune" {
			size = component.Bytes
		}
	}
	return upgrade.Proof{StartedForAttempt: true, OperationID: operation.ID, AttemptID: operation.AttemptID, Challenge: operation.Challenge, Binding: operation.Request.Binding, InstallationID: operation.Request.InstallationID, InstallationRevision: "2", ManifestSHA256: digest, Running: api.RunningProgram{PID: 202, StartID: "target-process", SHA256: target.ProgramSHA256(), Bytes: size, Build: api.BuildInfo{OS: target.Platform.OS, Arch: target.Platform.Arch}}, Incarnation: "target-incarnation", ConnectionGeneration: 2, RouteEpoch: 3, OriginalInstallationRestored: true, ReleaseVerified: true, GatewayAccepted: true, Routed: true, ObservedAt: time.Now().UTC()}
}

func TestTerminalSuccessRequiresAllProofAndCannotRegress(t *testing.T) {
	s, request, target, source := jobFixture(t)
	record := verifying(t, s, request, target, source)
	for _, fault := range []string{"gateway_rejected", "unroutable", "incomplete_release", "wrong_startup", "previous_attempt", "previous_challenge", "old_process", "old_manifest", "stale"} {
		t.Run(fault, func(t *testing.T) {
			proof := proofFor(t, record.Operation, target)
			switch fault {
			case "wrong_startup":
				proof.StartedForAttempt = false
			case "gateway_rejected":
				proof.GatewayAccepted = false
			case "unroutable":
				proof.Routed = false
			case "incomplete_release":
				proof.ReleaseVerified = false
			case "previous_attempt":
				proof.AttemptID = "old"
			case "previous_challenge":
				proof.Challenge = "old"
			case "old_process":
				proof.Running.StartID = source.Running.StartID
			case "old_manifest":
				proof.ManifestSHA256 = digestText("old")
			case "stale":
				proof.ObservedAt = time.Now().Add(-time.Minute)
			}
			_, err := s.Update(t.Context(), record.Operation.ID, record.Owner, record.Operation.Revision, func(r *Record) error {
				r.Operation.Phase, r.Operation.Confirmed, r.Operation.Proof = upgrade.Succeeded, true, &proof
				return nil
			})
			if err == nil {
				t.Fatal("incomplete/late proof completed upgrade")
			}
		})
	}
	proof := proofFor(t, record.Operation, target)
	terminal, err := s.Update(t.Context(), record.Operation.ID, record.Owner, record.Operation.Revision, func(r *Record) error {
		r.Operation.Phase, r.Operation.Confirmed, r.Operation.Proof = upgrade.Succeeded, true, &proof
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if active, err := s.Active(t.Context()); err != nil || active == nil {
		t.Fatal("terminal commit opened admission before seal cleanup", active, err)
	}
	if _, err := s.Update(t.Context(), terminal.Operation.ID, terminal.Owner, terminal.Operation.Revision, func(r *Record) error { r.Operation.Phase = upgrade.RollingBack; return nil }); err == nil {
		t.Fatal("confirmed terminal result regressed")
	}
	if _, err := s.ReleaseSeal(t.Context(), terminal.Operation.ID, terminal.Owner, terminal.Operation.Revision); err != nil {
		t.Fatal(err)
	}
	if active, err := s.Active(t.Context()); err != nil || active != nil {
		t.Fatal(active, err)
	}
}

func TestRecoveryFencesWorkerAndRejectsLateUpgradeProofDuringRollback(t *testing.T) {
	s, request, target, source := jobFixture(t)
	previous := verifying(t, s, request, target, source)
	late := proofFor(t, previous.Operation, target)
	recovered, err := s.Claim(t.Context(), previous.Operation.ID)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.Update(t.Context(), previous.Operation.ID, previous.Owner, previous.Operation.Revision, func(r *Record) error { return nil }); err == nil {
		t.Fatal("old worker wrote after recovery")
	}
	rollback, err := s.Update(t.Context(), recovered.Operation.ID, recovered.Owner, recovered.Operation.Revision, func(r *Record) error {
		r.Operation.Phase, r.Operation.Rollback = upgrade.RollingBack, upgrade.RollbackRunning
		r.Operation.AttemptID, r.Operation.Challenge, r.Operation.AttemptStartedAt = "rollback-attempt", "rollback-challenge", time.Now().UTC()
		r.Operation.Failure = &upgrade.Issue{Code: "PLATFORM_VERIFY_TIMEOUT", Stage: "verifying"}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.Update(t.Context(), rollback.Operation.ID, rollback.Owner, rollback.Operation.Revision, func(r *Record) error {
		r.Operation.Phase, r.Operation.Confirmed, r.Operation.Proof = upgrade.Succeeded, true, &late
		return nil
	}); err == nil {
		t.Fatal("late target proof defeated rollback")
	}
	rollback, err = s.Update(t.Context(), rollback.Operation.ID, rollback.Owner, rollback.Operation.Revision, func(r *Record) error { r.Operation.Phase = upgrade.RollbackVerifying; return nil })
	if err != nil {
		t.Fatal(err)
	}
	proof := proofFor(t, rollback.Operation, source.Installation.Release)
	restored, err := s.Update(t.Context(), rollback.Operation.ID, rollback.Owner, rollback.Operation.Revision, func(r *Record) error {
		r.Operation.Phase, r.Operation.Confirmed, r.Operation.Rollback, r.Operation.Proof = upgrade.Failed, true, upgrade.RollbackRestored, &proof
		return nil
	})
	if err != nil || restored.Operation.Failure.Code != "PLATFORM_VERIFY_TIMEOUT" {
		t.Fatal(restored, err)
	}
}

func TestExpiredHistoryRetainsKeysAndActiveRecovery(t *testing.T) {
	s, request, target, _ := jobFixture(t)
	source := inspection(target, "2")
	for i := range 130 {
		request.SubmissionID = fmt.Sprintf("completed-%d", i)
		if _, err := s.Admit(t.Context(), upgrade.Submission{Request: request, ReservedAt: time.Now().UTC()}, target, source, strings.Repeat("c", 64)); err != nil {
			t.Fatal(err)
		}
	}
	request.SubmissionID = "unfinished"
	active, err := s.Admit(t.Context(), upgrade.Submission{Request: request, ReservedAt: time.Now().UTC()}, target, inspection(manifest("R1"), "1"), strings.Repeat("c", 64))
	if err != nil {
		t.Fatal(err)
	}
	pruned, err := s.Prune(t.Context(), time.Now().Add(25*time.Hour))
	if err != nil || len(pruned) != 2 {
		t.Fatal(pruned, err)
	}
	request.SubmissionID = "completed-0"
	expired, err := s.Admit(t.Context(), upgrade.Submission{Request: request, ReservedAt: time.Now().UTC()}, target, source, strings.Repeat("c", 64))
	if err != nil || expired.Admission != api.SubmissionExpired {
		t.Fatal("expired key admitted work again", expired, err)
	}
	page, err := s.List(t.Context(), request.Binding, request.InstallationID, "", 100)
	if err != nil || page.Active == nil || page.Active.ID != active.ID || len(page.Items) != 100 || page.NextCursor == "" {
		t.Fatal(page, err)
	}
	more, err := s.List(t.Context(), request.Binding, request.InstallationID, page.NextCursor, 100)
	if err != nil || len(more.Items) != 28 || more.NextCursor != "" {
		t.Fatal(more, err)
	}
}

func TestReadOnlyStoreCannotCreateOrAdvanceUpgrade(t *testing.T) {
	directory := filepath.Join(t.TempDir(), "absent")
	if store, err := OpenReadOnly(t.Context(), directory); err == nil {
		store.Close()
		t.Fatal("created missing store")
	}
	if _, err := os.Stat(directory); !os.IsNotExist(err) {
		t.Fatal("read created filesystem state", err)
	}
	writer, err := Open(t.Context(), directory)
	if err != nil {
		t.Fatal(err)
	}
	defer writer.Close()
	reader, err := OpenReadOnly(t.Context(), directory)
	if err != nil {
		t.Fatal(err)
	}
	defer reader.Close()
	if _, err := reader.db.ExecContext(t.Context(), `DELETE FROM upgrade_settings`); err == nil {
		t.Fatal("read-only handle permitted shared state mutation")
	}
}

func TestEqualFilesStillRequireRestartOfOldReleaseProcess(t *testing.T) {
	s, request, target, source := jobFixture(t)
	source = inspection(target, source.Installation.Revision)
	source.RunningFromSelectedRelease = false
	request.ExpectedRunningSHA256 = source.Running.SHA256
	op, err := s.Admit(t.Context(), upgrade.Submission{Request: request, ReservedAt: time.Now().UTC()}, target, source, digestText("config"))
	if err != nil {
		t.Fatal(err)
	}
	if op.Phase == upgrade.AlreadyCurrent || op.Confirmed || op.Plan.ReleaseUpdateRequired || !op.Plan.ConnectorRestartRequired || op.Plan.RestartReason != "RUNNING_RELEASE_DIRECTORY_CHANGED" {
		t.Fatal("stale process counted as already current", op)
	}
}
