package installation

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"time"

	"github.com/aiomni/dune/internal/launchgate"
	"github.com/aiomni/dune/internal/release"
	"github.com/aiomni/dune/pkg/api"
	"github.com/aiomni/dune/pkg/upgrade"
)

const registrationName = "runner-installation.json"

type Registration struct {
	ID   string `json:"id"`
	Root string `json:"root"`
}

// Register publishes a standard installation's identity to its connector state
// directory. Replacement requires explicit installation recreation; inspection
// never guesses a root by walking executable paths or silently changes identity.
func (s *Store) Register() error {
	record, err := s.Read()
	if err != nil {
		return err
	}
	if err := launchgate.CheckDirectory(record.Metadata.StateDir); err != nil {
		return err
	}
	registration := Registration{ID: record.Metadata.ID, Root: s.root}
	var existing Registration
	err = readJSON(filepath.Join(record.Metadata.StateDir, registrationName), &existing)
	if err == nil {
		if existing != registration {
			return fmt.Errorf("connector state belongs to another installation")
		}
		return nil
	}
	if !os.IsNotExist(err) {
		return err
	}
	return writeJSON(record.Metadata.StateDir, registrationName, registration)
}

func Find(stateDir string) (Registration, error) {
	var registration Registration
	if err := launchgate.CheckDirectory(stateDir); err != nil {
		return registration, err
	}
	if err := readJSON(filepath.Join(stateDir, registrationName), &registration); err != nil {
		return registration, err
	}
	if api.ValidateSubmissionID(registration.ID) != nil || !filepath.IsAbs(registration.Root) {
		return registration, fmt.Errorf("invalid installation registration")
	}
	record, err := readRecord(registration.Root)
	if err != nil {
		return registration, err
	}
	actualState, err := filepath.EvalSymlinks(stateDir)
	if err != nil {
		return registration, err
	}
	expectedState, err := filepath.EvalSymlinks(record.Metadata.StateDir)
	if err != nil {
		return registration, err
	}
	if record.Metadata.ID != registration.ID || expectedState != actualState {
		return registration, fmt.Errorf("installation registration no longer matches its owner")
	}
	return registration, nil
}

// View verifies an observation without acquiring the worker's installation
// lock. It is used by the routed platform probe while that worker waits for the
// result. An external change is explicitly unverifiable here; only Observe
// under the installation lock can advance its revision.
func View(ctx context.Context, root string) (upgrade.Installation, error) {
	record, err := readRecord(root)
	if err != nil {
		return upgrade.Installation{}, err
	}
	if record.Pending != nil {
		return upgrade.Installation{}, ErrRecoveryRequired
	}
	store := &Store{root: root}
	actual, err := store.currentDirectory()
	if err != nil || actual != record.Current.Directory {
		return upgrade.Installation{}, fmt.Errorf("installation pointer changed during observation")
	}
	components, complete, err := release.Observe(ctx, filepath.Join(root, actual), record.Current.Manifest.Components)
	if err != nil {
		return upgrade.Installation{}, err
	}
	if fingerprint(actual, components) != record.Fingerprint {
		return upgrade.Installation{}, fmt.Errorf("installation changed without a confirmed revision")
	}
	after, err := readRecord(root)
	if err != nil {
		return upgrade.Installation{}, err
	}
	beforeBytes, _ := json.Marshal(record)
	afterBytes, _ := json.Marshal(after)
	if !bytes.Equal(beforeBytes, afterBytes) {
		return upgrade.Installation{}, fmt.Errorf("installation record changed during observation")
	}
	selected, err := store.currentDirectory()
	if err != nil || selected != actual {
		return upgrade.Installation{}, fmt.Errorf("installation pointer changed during observation")
	}
	return upgrade.Installation{ID: record.Metadata.ID, Revision: strconv.FormatUint(record.Revision, 10), Method: record.Metadata.Method, Release: record.Current.Manifest, Components: components, Complete: complete, ObservedAt: time.Now().UTC()}, nil
}
