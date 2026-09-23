// Package installation owns the physical distribution and its monotonic source
// revision. Session/configuration data is never copied into rollback materials.
package installation

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/aiomni/dune/internal/launchgate"
	"github.com/aiomni/dune/internal/release"
	"github.com/aiomni/dune/internal/wire"
	"github.com/aiomni/dune/pkg/api"
	"github.com/aiomni/dune/pkg/upgrade"
)

var ErrBusy = errors.New("installation modification is in progress")
var ErrRecoveryRequired = errors.New("installation switch requires recovery")

type Metadata struct {
	ID          string `json:"id"`
	Method      string `json:"method"`
	ConfigPath  string `json:"config_path"`
	StateDir    string `json:"state_dir"`
	ServicePATH string `json:"service_path"`
	ServiceName string `json:"service_name,omitempty"`
}

// Location names an owned release directory beneath the installation root.
type Location struct {
	Directory string           `json:"directory"`
	Manifest  upgrade.Manifest `json:"manifest"`
}

// Transition is persisted before replacing current. Recovery observes the
// actual symlink and consumes this exact intent; it never repeats admission.
type Transition struct {
	OperationID string   `json:"operation_id"`
	From        Location `json:"from"`
	To          Location `json:"to"`
}

type Record struct {
	Metadata    Metadata    `json:"metadata"`
	Revision    uint64      `json:"revision"`
	Current     Location    `json:"current"`
	Fingerprint string      `json:"fingerprint"`
	Pending     *Transition `json:"pending,omitempty"`
}

// Store owns the exclusive installation lock. Use a separate operation store
// for progress reads; those must stay available while this lock is held.
type Store struct {
	root string
	lock *os.File
	// Tests may interrupt only physical/durable boundaries. No runtime endpoint
	// or environment variable can install this barrier in a production worker.
	barrier func(string) error
}

func Lock(root string) (*Store, error) {
	if err := launchgate.CheckDirectory(root); err != nil {
		return nil, err
	}
	lock, err := os.OpenFile(filepath.Join(root, ".install.lock"), os.O_CREATE|os.O_RDWR|syscall.O_NOFOLLOW|syscall.O_NONBLOCK, 0600)
	if err != nil {
		return nil, err
	}
	info, err := lock.Stat()
	if err == nil {
		err = privateFile(info)
	}
	if err != nil {
		lock.Close()
		return nil, err
	}
	if err := syscall.Flock(int(lock.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		lock.Close()
		if errors.Is(err, syscall.EWOULDBLOCK) {
			return nil, ErrBusy
		}
		return nil, err
	}
	return &Store{root: root, lock: lock}, nil
}

func (s *Store) Close() error {
	if s.lock == nil {
		return nil
	}
	err := s.lock.Close()
	s.lock = nil
	return err
}

func (m Metadata) Validate(root string) error {
	if api.ValidateSubmissionID(m.ID) != nil || (m.Method != "service" && m.Method != "managed") || !filepath.IsAbs(m.ConfigPath) || !filepath.IsAbs(m.StateDir) {
		return fmt.Errorf("complete standard installation metadata required")
	}
	if m.ServiceName == "" || len(m.ServiceName) > 64 || strings.Trim(m.ServiceName, "abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQRSTUVWXYZ0123456789-_") != "" {
		return fmt.Errorf("standard service identity required")
	}
	for _, persistent := range []string{m.ConfigPath, m.StateDir} {
		resolved, err := filepath.EvalSymlinks(persistent)
		if err != nil {
			return fmt.Errorf("persistent installation path cannot be verified")
		}
		resolvedRoot, err := filepath.EvalSymlinks(root)
		if err != nil {
			return err
		}
		relative, err := filepath.Rel(filepath.Join(resolvedRoot, "releases"), resolved)
		if err != nil || (relative != ".." && !strings.HasPrefix(relative, ".."+string(filepath.Separator))) {
			return fmt.Errorf("persistent state must be outside release directories")
		}
	}
	return nil
}

func (l Location) valid() bool {
	return filepath.Clean(l.Directory) == l.Directory && filepath.Dir(l.Directory) == "releases" && wire.ValidID(filepath.Base(l.Directory)) && l.Manifest.Validate() == nil
}

func (s *Store) Read() (Record, error) {
	if s.lock == nil {
		return Record{}, os.ErrClosed
	}
	return readRecord(s.root)
}

func readRecord(root string) (Record, error) {
	var record Record
	if err := launchgate.CheckDirectory(root); err != nil {
		return record, err
	}
	if err := readJSON(filepath.Join(root, "installation.json"), &record); err != nil {
		return record, err
	}
	if record.Metadata.Validate(root) != nil || record.Revision == 0 || !record.Current.valid() || !upgrade.ValidSHA256(record.Fingerprint) {
		return record, fmt.Errorf("invalid installation record")
	}
	if pending := record.Pending; pending != nil && (api.ValidateSubmissionID(pending.OperationID) != nil || !pending.From.valid() || !pending.To.valid()) {
		return record, fmt.Errorf("invalid installation transition")
	}
	return record, nil
}

// Initialize records only a new standard installation. It never converts a
// pre-existing record or manufactures installation identity from a pathname.
func (s *Store) Initialize(ctx context.Context, metadata Metadata, current Location) (Record, error) {
	var record Record
	if s.lock == nil {
		return record, os.ErrClosed
	}
	if metadata.Validate(s.root) != nil || !current.valid() {
		return record, fmt.Errorf("valid initial installation required")
	}
	if _, err := os.Lstat(filepath.Join(s.root, "installation.json")); !os.IsNotExist(err) {
		return record, fmt.Errorf("installation identity already exists or cannot be verified")
	}
	actual, err := s.currentDirectory()
	if err != nil || actual != current.Directory {
		return record, fmt.Errorf("initial current directory differs")
	}
	observed, _, err := release.Observe(ctx, filepath.Join(s.root, actual), current.Manifest.Components)
	if err != nil {
		return record, err
	}
	record = Record{Metadata: metadata, Revision: 1, Current: current, Fingerprint: fingerprint(actual, observed)}
	return record, s.save(record)
}

func fingerprint(directory string, components []upgrade.ComponentObservation) string {
	// Matches depends on the selected manifest, not physical content.
	physical := append([]upgrade.ComponentObservation(nil), components...)
	for i := range physical {
		physical[i].Matches = false
	}
	slices.SortFunc(physical, func(a, b upgrade.ComponentObservation) int { return strings.Compare(a.Path, b.Path) })
	body, _ := json.Marshal(struct {
		Directory  string
		Components []upgrade.ComponentObservation
	}{directory, physical})
	sum := sha256.Sum256(body)
	return hex.EncodeToString(sum[:])
}

// Observe atomically advances the revision on external file/attribute changes.
// A plain read or an operation-progress write cannot advance this counter.
func (s *Store) Observe(ctx context.Context) (upgrade.Installation, error) {
	record, err := s.Read()
	if err != nil {
		return upgrade.Installation{}, err
	}
	if record.Pending != nil {
		return upgrade.Installation{}, ErrRecoveryRequired
	}
	actual, err := s.currentDirectory()
	if err != nil || actual != record.Current.Directory {
		return upgrade.Installation{}, fmt.Errorf("unrecorded installation directory replacement")
	}
	components, complete, err := release.Observe(ctx, filepath.Join(s.root, actual), record.Current.Manifest.Components)
	if err != nil {
		return upgrade.Installation{}, err
	}
	hash := fingerprint(actual, components)
	if hash != record.Fingerprint {
		if record.Revision == math.MaxUint64 {
			return upgrade.Installation{}, fmt.Errorf("installation revision exhausted")
		}
		record.Revision++
		record.Fingerprint = hash
		if err := s.save(record); err != nil {
			return upgrade.Installation{}, err
		}
	}
	return upgrade.Installation{ID: record.Metadata.ID, Revision: strconv.FormatUint(record.Revision, 10), Method: record.Metadata.Method, Release: record.Current.Manifest, Components: components, Complete: complete, ObservedAt: time.Now().UTC()}, nil
}

func (s *Store) currentDirectory() (string, error) {
	current, err := os.Readlink(filepath.Join(s.root, "current"))
	if err != nil {
		return "", err
	}
	if filepath.Clean(current) != current || filepath.Dir(current) != "releases" || !wire.ValidID(filepath.Base(current)) {
		return "", fmt.Errorf("current must select an owned relative release directory")
	}
	if err := launchgate.CheckDirectory(filepath.Join(s.root, current)); err != nil {
		return "", err
	}
	return current, nil
}

func (s *Store) save(record Record) error {
	if s.lock == nil {
		return os.ErrClosed
	}
	return writeJSON(s.root, "installation.json", record)
}

func readJSON(path string, value any) error {
	file, err := os.OpenFile(path, os.O_RDONLY|syscall.O_NOFOLLOW|syscall.O_NONBLOCK, 0)
	if err != nil {
		return err
	}
	defer file.Close()
	info, err := file.Stat()
	if err != nil {
		return err
	}
	if err := privateFile(info); err != nil {
		return err
	}
	if info.Size() > 256<<10 {
		return fmt.Errorf("installation record exceeds bound")
	}
	decoder := json.NewDecoder(io.LimitReader(file, (256<<10)+1))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(value); err != nil {
		return err
	}
	if decoder.Decode(new(any)) != io.EOF {
		return fmt.Errorf("invalid installation record trailer")
	}
	return nil
}

func privateFile(info os.FileInfo) error {
	owner, ok := info.Sys().(*syscall.Stat_t)
	if !info.Mode().IsRegular() || info.Mode().Perm() != 0600 || !ok || int(owner.Uid) != os.Getuid() || owner.Nlink != 1 {
		return fmt.Errorf("invalid private installation file")
	}
	return nil
}

func writeJSON(directory, name string, value any) error {
	body, err := json.Marshal(value)
	if err != nil {
		return err
	}
	file, err := os.CreateTemp(directory, ".installation-")
	if err != nil {
		return err
	}
	defer os.Remove(file.Name())
	if _, err = file.Write(body); err == nil {
		err = file.Sync()
	}
	if closeErr := file.Close(); err == nil {
		err = closeErr
	}
	if err == nil {
		err = os.Rename(file.Name(), filepath.Join(directory, name))
	}
	if err != nil {
		return err
	}
	return syncDirectory(directory)
}

func syncDirectory(path string) error {
	directory, err := os.Open(path)
	if err != nil {
		return err
	}
	defer directory.Close()
	return directory.Sync()
}
