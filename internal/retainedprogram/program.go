// Package retainedprogram preserves an immutable private copy of a Dune helper
// for a Runtime's lifetime, independently of its original release pathname.
package retainedprogram

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"syscall"
)

const MaxBytes = 256 * 1024 * 1024

type Identity struct {
	SHA256 string `json:"sha256"`
	Bytes  int64  `json:"bytes"`
}

func (id Identity) Validate() error {
	decoded, err := hex.DecodeString(id.SHA256)
	if err != nil || len(decoded) != sha256.Size || hex.EncodeToString(decoded) != id.SHA256 || id.Bytes <= 0 || id.Bytes > MaxBytes {
		return fmt.Errorf("invalid retained program identity")
	}
	return nil
}

func Current(destination string) (Identity, error) {
	program, err := os.Executable()
	if err != nil {
		return Identity{}, err
	}
	return Copy(program, destination)
}

// Copy uses a distinct inode: overwriting or deleting the release cannot alter
// the retained program. Publication is exclusive and synced before execution.
func Copy(source, destination string) (identity Identity, err error) {
	input, err := os.OpenFile(source, os.O_RDONLY|syscall.O_NOFOLLOW, 0)
	if err != nil {
		return identity, err
	}
	defer input.Close()
	info, err := input.Stat()
	if err != nil {
		return identity, err
	}
	if !info.Mode().IsRegular() || info.Size() <= 0 || info.Size() > MaxBytes {
		return identity, fmt.Errorf("helper program exceeds retained file contract")
	}
	output, err := os.OpenFile(destination, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0600)
	if err != nil {
		return identity, err
	}
	defer func() {
		output.Close()
		if err != nil {
			os.Remove(destination)
		}
	}()
	digest := sha256.New()
	identity.Bytes, err = io.Copy(io.MultiWriter(output, digest), io.LimitReader(input, MaxBytes+1))
	if err != nil {
		return identity, err
	}
	if identity.Bytes != info.Size() {
		return identity, fmt.Errorf("helper program changed size while retaining")
	}
	identity.SHA256 = hex.EncodeToString(digest.Sum(nil))
	if err = output.Chmod(0700); err != nil {
		return identity, err
	}
	if err = output.Sync(); err != nil {
		return identity, err
	}
	if err = output.Close(); err != nil {
		return identity, err
	}
	directory, err := os.Open(filepath.Dir(destination))
	if err != nil {
		return identity, err
	}
	defer directory.Close()
	err = directory.Sync()
	return identity, err
}

func Verify(path string, expected Identity) error {
	if err := expected.Validate(); err != nil {
		return err
	}
	program, err := os.OpenFile(path, os.O_RDONLY|syscall.O_NOFOLLOW, 0)
	if err != nil {
		return err
	}
	defer program.Close()
	info, err := program.Stat()
	if err != nil {
		return err
	}
	owner, ok := info.Sys().(*syscall.Stat_t)
	if !ok || int(owner.Uid) != os.Getuid() || owner.Nlink != 1 || !info.Mode().IsRegular() || info.Mode().Perm() != 0700 || info.Size() != expected.Bytes {
		return fmt.Errorf("retained program file identity is invalid")
	}
	digest := sha256.New()
	if _, err := io.Copy(digest, io.LimitReader(program, MaxBytes+1)); err != nil {
		return err
	}
	if hex.EncodeToString(digest.Sum(nil)) != expected.SHA256 {
		return fmt.Errorf("retained program digest differs")
	}
	return nil
}
