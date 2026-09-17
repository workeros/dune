package fabricd

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"os"
	"path/filepath"
	"syscall"

	"github.com/aiomni/dune/pkg/api"
)

func validateWriteIntent(intent, expected string) error {
	switch intent {
	case "create", "unconditional":
		if expected != "" {
			return fmt.Errorf("%s does not accept expected_revision", intent)
		}
	case "conditional":
		if !validHash(expected) {
			return fmt.Errorf("conditional requires a lowercase SHA256 expected_revision")
		}
	default:
		return fmt.Errorf("intent must be create, conditional or unconditional")
	}
	return nil
}

// Called under fileMu, including the full conditional digest. This coordinates
// Files and Upload commits in this Engine, not external processes.
func writeTargetPermissions(path, intent, expected string) (os.FileMode, error) {
	before, err := os.Lstat(path)
	if os.IsNotExist(err) {
		if intent == "conditional" {
			return 0, changedFile("file no longer exists")
		}
		return 0600, nil
	}
	if err != nil {
		return 0, err
	}
	if intent == "create" {
		return 0, &api.Error{Code: "FILE_EXISTS", Detail: "create target already exists"}
	}
	if !before.Mode().IsRegular() {
		return 0, &api.Error{Code: "UNSUPPORTED", Detail: "replacement requires a regular file, not a symbolic link or special file"}
	}
	if intent == "conditional" {
		f, err := os.OpenFile(path, os.O_RDONLY|syscall.O_NOFOLLOW|syscall.O_NONBLOCK, 0)
		if err != nil {
			if os.IsNotExist(err) {
				return 0, changedFile("file no longer exists")
			}
			return 0, err
		}
		defer f.Close()
		opened, err := f.Stat()
		if err != nil {
			return 0, err
		}
		if !opened.Mode().IsRegular() || !os.SameFile(before, opened) {
			return 0, changedFile("file was replaced while checking revision")
		}
		current, err := descriptorFileInfo(f, path, opened, true)
		if err != nil {
			return 0, err
		}
		if current.Revision != expected {
			return 0, changedFile("file changed since it was opened")
		}
		return filePermissions(os.FileMode(current.Mode)), nil
	}
	return filePermissions(before.Mode()), nil
}

func (d *Engine) commitPreparedFile(f *os.File, path, intent, expected, digest string) (api.FileInfo, error) {
	d.fileMu.Lock()
	defer d.fileMu.Unlock()
	mode, err := writeTargetPermissions(path, intent, expected)
	if err != nil {
		return api.FileInfo{}, err
	}
	if err := f.Chmod(mode); err != nil {
		return api.FileInfo{}, err
	}
	if err := f.Sync(); err != nil {
		return api.FileInfo{}, err
	}
	rawInfo, err := f.Stat()
	if err != nil {
		return api.FileInfo{}, err
	}
	info := withFileRevision(fileInfo(path, rawInfo), digest)
	if intent == "create" {
		if err := os.Link(f.Name(), path); err != nil {
			return api.FileInfo{}, err
		}
		// The link has committed the file. Temporary-name cleanup cannot undo it
		// and must not turn an acknowledged commit into a misleading failure.
		_ = os.Remove(f.Name())
	} else if err := os.Rename(f.Name(), path); err != nil {
		return api.FileInfo{}, err
	}
	return info, nil
}

func (d *Engine) writeFile(a api.File) (api.FileInfo, error) {
	if err := validateWriteIntent(a.Intent, a.ExpectedRevision); err != nil {
		return api.FileInfo{}, err
	}
	if a.Overwrite {
		return api.FileInfo{}, fmt.Errorf("overwrite is only used by rename; use a write intent")
	}
	f, err := os.CreateTemp(filepath.Dir(a.Path), ".dune-write-*")
	if err != nil {
		return api.FileInfo{}, err
	}
	defer os.Remove(f.Name())
	defer f.Close()
	if _, err := f.Write(a.Data); err != nil {
		return api.FileInfo{}, err
	}
	// Flush the content before taking the commit lock. Final permissions are
	// selected and synced while the target is protected from other Dune writes.
	if err := f.Sync(); err != nil {
		return api.FileInfo{}, err
	}
	digest := sha256.Sum256(a.Data)
	return d.commitPreparedFile(f, a.Path, a.Intent, a.ExpectedRevision, hex.EncodeToString(digest[:]))
}
