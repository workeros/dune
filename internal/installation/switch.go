package installation

import (
	"context"
	"fmt"
	"math"
	"os"
	"path/filepath"

	"github.com/aiomni/dune/internal/launchgate"
	"github.com/aiomni/dune/internal/release"
	"github.com/aiomni/dune/internal/wire"
	"github.com/aiomni/dune/pkg/api"
	"github.com/aiomni/dune/pkg/upgrade"
)

// Switch rechecks source revision while holding the same lock used to change
// current. The caller must first persist the operation and launch seal, and
// retain From until target or rollback platform verification is durable.
func (s *Store) Switch(ctx context.Context, owner launchgate.Seal, expectedRevision string, target Location) error {
	observed, err := s.Observe(ctx)
	if err != nil {
		return err
	}
	if observed.ID != owner.InstallationID || observed.Revision != expectedRevision {
		return &api.Error{Code: "INSTALLATION_CHANGED", Detail: "complete source installation changed"}
	}
	if !target.valid() {
		return fmt.Errorf("valid target location required")
	}
	if err := launchgate.CheckDirectory(filepath.Join(s.root, target.Directory)); err != nil {
		return err
	}
	_, complete, err := release.Observe(ctx, filepath.Join(s.root, target.Directory), target.Manifest.Components)
	if err != nil {
		return err
	}
	if !complete {
		return fmt.Errorf("target distribution is incomplete")
	}
	return s.switchTo(ctx, owner, target)
}

// Restore checks the original physical observation, including originally absent
// helpers. It restores only the distribution pointer, never SessionDir/config.
func (s *Store) Restore(ctx context.Context, owner launchgate.Seal, source Location, original []upgrade.ComponentObservation) error {
	record, err := s.Read()
	if err != nil {
		return err
	}
	if record.Metadata.ID != owner.InstallationID {
		return fmt.Errorf("installation identity replaced")
	}
	if record.Pending != nil {
		return ErrRecoveryRequired
	}
	if !source.valid() {
		return fmt.Errorf("valid original location required")
	}
	if err := launchgate.CheckDirectory(filepath.Join(s.root, source.Directory)); err != nil {
		return err
	}
	actual, _, err := release.Observe(ctx, filepath.Join(s.root, source.Directory), source.Manifest.Components)
	if err != nil {
		return err
	}
	if fingerprint(source.Directory, actual) != fingerprint(source.Directory, original) {
		return fmt.Errorf("original rollback files changed")
	}
	return s.switchTo(ctx, owner, source)
}

func (s *Store) switchTo(ctx context.Context, owner launchgate.Seal, target Location) error {
	record, err := s.Read()
	if err != nil {
		return err
	}
	if api.ValidateSubmissionID(owner.OperationID) != nil {
		return fmt.Errorf("original operation required")
	}
	if record.Pending != nil {
		return ErrRecoveryRequired
	}
	// The seal is an independent fail-closed obligation, not an optional caller
	// convention. Ownership is additionally fenced by this installation lock.
	seal, err := launchgate.ReadSeal(record.Metadata.StateDir)
	if err != nil {
		return err
	}
	if seal == nil || *seal != owner || owner.InstallationID != record.Metadata.ID {
		return fmt.Errorf("matching persistent launch seal required before switch")
	}
	if record.Current.Directory == target.Directory {
		return fmt.Errorf("target is already the selected directory")
	}
	record.Pending = &Transition{OperationID: owner.OperationID, From: record.Current, To: target}
	if err := s.save(record); err != nil {
		return err
	}
	if err := s.checkpoint("intent"); err != nil {
		return err
	}
	link := filepath.Join(s.root, ".current-"+wire.ID())
	if err := os.Symlink(target.Directory, link); err != nil {
		return err
	}
	defer os.Remove(link)
	if err := os.Rename(link, filepath.Join(s.root, "current")); err != nil {
		return err
	}
	if err := syncDirectory(s.root); err != nil {
		return err
	}
	if err := s.checkpoint("switched"); err != nil {
		return err
	}
	return s.RecoverSwitch(ctx, owner)
}

// RecoverSwitch reconciles a single original write-ahead intent against disk.
// It never replaces current a second time. A different pointer requires manual
// diagnosis; missing/unreadable records are not treated as fresh installations.
func (s *Store) RecoverSwitch(ctx context.Context, owner launchgate.Seal) error {
	record, err := s.Read()
	if err != nil {
		return err
	}
	if record.Pending == nil {
		return nil
	}
	if record.Pending.OperationID != owner.OperationID {
		return fmt.Errorf("another operation owns installation recovery")
	}
	seal, err := launchgate.ReadSeal(record.Metadata.StateDir)
	if err != nil {
		return err
	}
	if seal == nil || *seal != owner || owner.InstallationID != record.Metadata.ID {
		return fmt.Errorf("matching recovery seal required")
	}
	actual, err := s.currentDirectory()
	if err != nil {
		return err
	}
	var selected Location
	switch actual {
	case record.Pending.From.Directory:
		selected = record.Pending.From
	case record.Pending.To.Directory:
		selected = record.Pending.To
	default:
		return fmt.Errorf("installation pointer differs from recorded transition")
	}
	components, _, err := release.Observe(ctx, filepath.Join(s.root, actual), selected.Manifest.Components)
	if err != nil {
		return err
	}
	hash := fingerprint(actual, components)
	if hash != record.Fingerprint {
		if record.Revision == math.MaxUint64 {
			return fmt.Errorf("installation revision exhausted")
		}
		record.Revision++
	}
	record.Current, record.Fingerprint, record.Pending = selected, hash, nil
	if err := s.save(record); err != nil {
		return err
	}
	return s.checkpoint("recorded")
}

func (s *Store) checkpoint(stage string) error {
	if s.barrier != nil {
		return s.barrier(stage)
	}
	return nil
}
