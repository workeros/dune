// Package retainedprogram preserves an immutable private copy of a Dune helper
// for a Runtime's lifetime, independently of its original release pathname.
package retainedprogram

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"github.com/aiomni/dune/internal/runningprogram"
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
	program, err := runningprogram.Open()
	if err != nil {
		return Identity{}, err
	}
	defer program.Close()
	return copyFile(program, destination)
}

// Copy uses a distinct inode: overwriting or deleting the release cannot alter
// the retained program. Publication is exclusive and synced before execution.
func Copy(source, destination string) (identity Identity, err error) {
	input, err := os.OpenFile(source, os.O_RDONLY|syscall.O_NOFOLLOW|syscall.O_NONBLOCK, 0)
	if err != nil {
		return identity, err
	}
	defer input.Close()
	return copyFile(input, destination)
}

func copyFile(input *os.File, destination string) (identity Identity, err error) {
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
	identity.Bytes, err = io.Copy(io.MultiWriter(output, digest), io.NewSectionReader(input, 0, info.Size()))
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
	program, err := os.OpenFile(path, os.O_RDONLY|syscall.O_NOFOLLOW|syscall.O_NONBLOCK, 0)
	if err != nil {
		return err
	}
	defer program.Close()
	return verify(context.Background(), program, expected)
}

// VerifyExecuting accepts different ancestor paths to the same retained inode,
// while rejecting a separate copy even if its bytes happen to match.
func VerifyExecuting(path string, expected Identity) error {
	if err := expected.Validate(); err != nil {
		return err
	}
	program, err := os.OpenFile(path, os.O_RDONLY|syscall.O_NOFOLLOW|syscall.O_NONBLOCK, 0)
	if err != nil {
		return err
	}
	defer program.Close()
	retained, err := program.Stat()
	if err != nil {
		return err
	}
	executing, err := runningprogram.Open()
	if err != nil {
		return err
	}
	defer executing.Close()
	running, err := executing.Stat()
	if err != nil || !os.SameFile(running, retained) {
		return fmt.Errorf("executing program differs from retained file")
	}
	return verify(context.Background(), program, expected)
}

// VerifyIn checks a program through the caller's pinned Runtime directory.
func VerifyIn(ctx context.Context, root *os.Root, name string, expected Identity) error {
	if err := expected.Validate(); err != nil {
		return err
	}
	program, err := root.OpenFile(name, os.O_RDONLY|syscall.O_NOFOLLOW|syscall.O_NONBLOCK, 0)
	if err != nil {
		return err
	}
	defer program.Close()
	return verify(ctx, program, expected)
}

func verify(ctx context.Context, program *os.File, expected Identity) error {
	info, err := program.Stat()
	if err != nil {
		return err
	}
	owner, ok := info.Sys().(*syscall.Stat_t)
	if !ok || int(owner.Uid) != os.Getuid() || owner.Nlink != 1 || !info.Mode().IsRegular() || info.Mode().Perm() != 0700 || info.Size() != expected.Bytes {
		return fmt.Errorf("retained program file identity is invalid")
	}
	digest := sha256.New()
	if _, err := io.Copy(digest, io.LimitReader(contextReader{ctx, program}, MaxBytes+1)); err != nil {
		return err
	}
	if hex.EncodeToString(digest.Sum(nil)) != expected.SHA256 {
		return fmt.Errorf("retained program digest differs")
	}
	return nil
}

type contextReader struct {
	ctx context.Context
	io.Reader
}

func (r contextReader) Read(p []byte) (int, error) {
	if err := r.ctx.Err(); err != nil {
		return 0, err
	}
	return r.Reader.Read(p)
}
