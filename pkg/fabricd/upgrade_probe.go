package fabricd

import (
	"fmt"
	"path/filepath"
	"reflect"

	"github.com/aiomni/dune/internal/installation"
	"github.com/aiomni/dune/internal/launchgate"
	"github.com/aiomni/dune/internal/runningprogram"
	"github.com/aiomni/dune/internal/upgradejob"
	"github.com/aiomni/dune/internal/wire"
	"github.com/aiomni/dune/pkg/api"
	"github.com/aiomni/dune/pkg/upgrade"
	pb "github.com/aiomni/dune/proto/dune/dtp/v1"
)

func (d *Engine) probeUpgrade(s *executionStream, message *pb.Message) {
	var probe upgrade.Probe
	if wire.Decode(message, &probe) != nil || !probe.Binding.Valid() || probe.Binding.MachineID != message.Target || message.RuntimeId != "" || api.ValidateSubmissionID(probe.InstallationID) != nil || api.ValidateSubmissionID(probe.OperationID) != nil || api.ValidateSubmissionID(probe.AttemptID) != nil || api.ValidateSubmissionID(probe.Challenge) != nil {
		s.Fail("INVALID_ARGUMENT", fmt.Errorf("complete original upgrade attempt required"))
		return
	}
	if err := s.Send(&pb.Message{Kind: "accepted", RequestId: message.RequestId}); err != nil {
		return
	}
	registration, err := installation.Find(d.stateDir)
	if err != nil || registration.ID != probe.InstallationID {
		s.Fail("UPGRADE_UNSUPPORTED", fmt.Errorf("standard installation identity is unavailable"))
		return
	}
	jobs, err := upgradejob.OpenReadOnly(s.ctx, filepath.Join(registration.Root, "upgrades"))
	if err != nil {
		s.Fail("UPGRADE_UNAVAILABLE", fmt.Errorf("durable upgrade evidence unavailable"))
		return
	}
	defer jobs.Close()
	operation, err := jobs.Get(s.ctx, upgrade.Query{Binding: probe.Binding, InstallationID: probe.InstallationID, OperationID: probe.OperationID})
	if err != nil {
		s.Fail("UPGRADE_NOT_FOUND", fmt.Errorf("original upgrade operation unavailable"))
		return
	}
	if operation.Confirmed || !operation.LaunchSealed || operation.AttemptID != probe.AttemptID || operation.Challenge != probe.Challenge || (operation.Phase != upgrade.Verifying && operation.Phase != upgrade.RollbackVerifying) {
		s.Fail("UPGRADE_ATTEMPT_CHANGED", fmt.Errorf("upgrade is not verifying this attempt"))
		return
	}
	seal, err := launchgate.ReadSeal(d.stateDir)
	if err != nil || seal == nil || seal.InstallationID != probe.InstallationID || seal.OperationID != probe.OperationID {
		s.Fail("UPGRADE_SEAL_UNVERIFIABLE", fmt.Errorf("original launch seal cannot be verified"))
		return
	}
	observed, err := installation.View(s.ctx, registration.Root)
	if err != nil {
		s.Fail("INSTALLATION_UNVERIFIABLE", fmt.Errorf("complete installation cannot be verified"))
		return
	}
	running, err := runningprogram.Inspect(s.ctx)
	if err != nil {
		s.Fail("RUNNING_PROGRAM_UNVERIFIABLE", fmt.Errorf("kernel executable cannot be verified"))
		return
	}
	restored := false
	expected := operation.Target
	if operation.Phase == upgrade.RollbackVerifying {
		if operation.Source.Installation == nil {
			s.Fail("ROLLBACK_SOURCE_UNAVAILABLE", fmt.Errorf("original distribution evidence unavailable"))
			return
		}
		expected = operation.Source.Installation.Release
		restored = reflect.DeepEqual(observed.Components, operation.Source.Installation.Components)
		if !restored {
			s.Fail("ROLLBACK_FILES_CHANGED", fmt.Errorf("original installation observation differs"))
			return
		}
	}
	if !restored && !observed.Complete {
		s.Fail("INSTALLATION_INCOMPLETE", fmt.Errorf("target installation is incomplete"))
		return
	}
	digest, err := observed.Release.Digest()
	wanted, wantedErr := expected.Digest()
	if err != nil || wantedErr != nil || digest != wanted || running.SHA256 != expected.ProgramSHA256() {
		s.Fail("INSTALLATION_CHANGED", fmt.Errorf("actual distribution or process differs from current attempt"))
		return
	}
	// Confirm this read did not cross a rollback or recovery checkpoint. Only
	// the host can add GatewayAccepted/Routed after receiving the response.
	current, err := jobs.Get(s.ctx, upgrade.Query{Binding: probe.Binding, InstallationID: probe.InstallationID, OperationID: probe.OperationID})
	if err != nil || current.Revision != operation.Revision {
		s.Fail("UPGRADE_ATTEMPT_CHANGED", fmt.Errorf("upgrade changed during probe"))
		return
	}
	proof := upgrade.Proof{OperationID: operation.ID, AttemptID: probe.AttemptID, Challenge: probe.Challenge, Binding: probe.Binding, InstallationID: observed.ID, InstallationRevision: observed.Revision, ManifestSHA256: digest, Running: running, Incarnation: d.inc, ConnectionGeneration: s.generation, RouteEpoch: s.epoch, OriginalInstallationRestored: restored, ReleaseVerified: observed.Complete, ObservedAt: running.ObservedAt}
	proof.StartedForAttempt = d.upgradeStartup != nil && *d.upgradeStartup == probe
	_ = s.Send(&pb.Message{Kind: "result", Payload: api.Payload(proof)})
}
