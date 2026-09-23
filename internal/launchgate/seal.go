package launchgate

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"syscall"

	"github.com/aiomni/dune/pkg/api"
)

const sealName = "upgrade-seal.json"

var ErrOwnerChanged = errors.New("upgrade seal ownership changed")

// Seal remains until the current exclusive recovery owner commits a verified
// terminal result. It has no expiry: elapsed time is not recovery evidence.
type Seal struct {
	InstallationID string `json:"installation_id"`
	OperationID    string `json:"operation_id"`
	Owner          string `json:"owner"`
}

func (s Seal) valid() bool {
	return api.ValidateSubmissionID(s.InstallationID) == nil && api.ValidateSubmissionID(s.OperationID) == nil && api.ValidateSubmissionID(s.Owner) == nil
}

// ReadSeal is diagnostic only. Admission must use Acquire to avoid a check/use
// race. Invalid, unreadable, linked or incomplete records fail closed.
func ReadSeal(directory string) (*Seal, error) {
	if err := CheckDirectory(directory); err != nil {
		return nil, err
	}
	f, err := os.OpenFile(filepath.Join(directory, sealName), os.O_RDONLY|syscall.O_NOFOLLOW|syscall.O_NONBLOCK, 0)
	if os.IsNotExist(err) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil {
		return nil, err
	}
	owner, ok := info.Sys().(*syscall.Stat_t)
	if !info.Mode().IsRegular() || info.Mode().Perm() != 0600 || !ok || int(owner.Uid) != os.Getuid() || owner.Nlink != 1 || info.Size() > 2048 {
		return nil, fmt.Errorf("invalid private upgrade seal")
	}
	var seal Seal
	decoder := json.NewDecoder(io.LimitReader(f, 2049))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&seal); err != nil {
		return nil, err
	}
	if !seal.valid() || decoder.Decode(new(any)) != io.EOF {
		return nil, fmt.Errorf("invalid upgrade seal")
	}
	return &seal, nil
}

// ClaimSeal creates a seal or fences the previous owner during recovery. The
// caller holds the installation lock as well as this exclusive launch gate.
// Recovery must name the exact existing seal; it cannot follow another operation.
func (g *Gate) ClaimSeal(previous *Seal, next Seal) error {
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.file == nil || !g.exclusive || !next.valid() {
		return ErrOwnerChanged
	}
	current, err := ReadSeal(g.directory)
	if err != nil {
		return err
	}
	if (current == nil) != (previous == nil) || (current != nil && *current != *previous) {
		return ErrOwnerChanged
	}
	if current != nil && (current.InstallationID != next.InstallationID || current.OperationID != next.OperationID || current.Owner == next.Owner) {
		return ErrOwnerChanged
	}
	body, err := json.Marshal(next)
	if err != nil {
		return err
	}
	f, err := os.CreateTemp(g.directory, ".upgrade-seal-")
	if err != nil {
		return err
	}
	defer os.Remove(f.Name())
	if _, err = f.Write(body); err == nil {
		err = f.Sync()
	}
	if closeErr := f.Close(); err == nil {
		err = closeErr
	}
	if err == nil {
		err = os.Rename(f.Name(), filepath.Join(g.directory, sealName))
	}
	if err != nil {
		return err
	}
	return syncDirectory(g.directory)
}

// ClearSeal must follow the durable terminal commit, never precede it. A crash
// between those steps leaves a sealed terminal operation for recovery to finish.
func (g *Gate) ClearSeal(expected Seal) error {
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.file == nil || !g.exclusive || !expected.valid() {
		return ErrOwnerChanged
	}
	current, err := ReadSeal(g.directory)
	if err != nil {
		return err
	}
	if current == nil || *current != expected {
		return ErrOwnerChanged
	}
	if err := os.Remove(filepath.Join(g.directory, sealName)); err != nil {
		return err
	}
	return syncDirectory(g.directory)
}

func syncDirectory(path string) error {
	directory, err := os.Open(path)
	if err != nil {
		return err
	}
	defer directory.Close()
	return directory.Sync()
}
