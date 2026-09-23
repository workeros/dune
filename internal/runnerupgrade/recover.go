package runnerupgrade

import (
	"context"
	"path/filepath"

	"github.com/aiomni/dune/internal/installation"
	"github.com/aiomni/dune/internal/launchgate"
	"github.com/aiomni/dune/internal/upgradejob"
	"github.com/aiomni/dune/pkg/upgrade"
)

// Recover explicitly grants a new bounded rollback attempt after an operator
// has repaired the reported issue. It never resubmits the original upgrade or
// changes binding, release, config identity, or any retained shared records.
func Recover(ctx context.Context, root, operationID, expectedRevision string) error {
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
	if err != nil {
		return err
	}
	if active == nil || active.Operation.ID != operationID || active.Operation.Revision != expectedRevision || active.Operation.Phase != upgrade.RecoveryBlocked {
		return issue("UPGRADE_RECOVERY_CHANGED")
	}
	w, close, err := newWorker(root, installed, jobs, state.Metadata)
	if err != nil {
		return err
	}
	defer close()
	w.record = *active
	if err := w.validateContext(ctx); err != nil {
		return err
	}
	seal, err := launchgate.ReadSeal(state.Metadata.StateDir)
	if err != nil || seal == nil || seal.InstallationID != state.Metadata.ID || seal.OperationID != operationID {
		return issue("UPGRADE_SEAL_UNVERIFIABLE")
	}
	if _, err := jobs.ResumeRecovery(ctx, operationID, active.Owner, expectedRevision); err != nil {
		return err
	}
	return w.execute(ctx, operationID)
}
