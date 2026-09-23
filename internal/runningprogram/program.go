// Package runningprogram measures the executable selected by the kernel, never
// the installation's current symlink. The process pins that file before serving.
package runningprogram

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"os"
	"sync"
	"time"

	"github.com/aiomni/dune/internal/buildinfo"
	"github.com/aiomni/dune/internal/process"
	"github.com/aiomni/dune/pkg/api"
)

const MaxBytes = 256 << 20

type image struct {
	file    *os.File
	sha256  string
	bytes   int64
	startID string
}

// Process-lifetime ownership is deliberate. A rename/unlink of the release must
// not lose the original executable, and all descriptors are close-on-exec.
var current = sync.OnceValues(func() (*image, error) {
	file, err := openExecuting()
	if err != nil {
		return nil, err
	}
	identity, err := measure(context.Background(), file)
	if err != nil {
		file.Close()
		return nil, err
	}
	boot, err := process.BootID()
	if err != nil {
		file.Close()
		return nil, err
	}
	started, err := processStart()
	if err != nil {
		file.Close()
		return nil, err
	}
	return &image{file: file, sha256: identity.SHA256, bytes: identity.Bytes, startID: fmt.Sprintf("%s/%d/%s", boot, os.Getpid(), started)}, nil
})

// Capture must run before serving or retaining a helper. A failure disables
// trustworthy inspection rather than substituting a file at the old pathname.
func Capture() error { _, err := current(); return err }

func Inspect(ctx context.Context) (api.RunningProgram, error) {
	image, err := current()
	if err != nil {
		return api.RunningProgram{}, err
	}
	identity, err := measure(ctx, image.file)
	if err != nil {
		return identity, err
	}
	if identity.SHA256 != image.sha256 || identity.Bytes != image.bytes {
		return api.RunningProgram{}, fmt.Errorf("executing image was modified after capture")
	}
	identity.PID, identity.StartID = os.Getpid(), image.startID
	identity.Build, identity.ObservedAt = buildinfo.Current(), time.Now().UTC()
	return identity, nil
}

// Open returns another descriptor for the pinned kernel image, not its old
// pathname. The caller owns it. No platform pathname fallback is permitted.
func Open() (*os.File, error) {
	image, err := current()
	if err != nil {
		return nil, err
	}
	return duplicate(image.file)
}

// IsExecuting compares the selected pathname with the pinned kernel file. Equal
// digests do not establish that a process started from a new release directory.
func IsExecuting(path string) bool {
	image, err := current()
	if err != nil {
		return false
	}
	running, err := image.file.Stat()
	selected, selectedErr := os.Stat(path)
	return err == nil && selectedErr == nil && os.SameFile(running, selected)
}

func measure(ctx context.Context, file *os.File) (api.RunningProgram, error) {
	var identity api.RunningProgram
	before, err := file.Stat()
	if err != nil {
		return identity, err
	}
	if !before.Mode().IsRegular() || before.Size() <= 0 || before.Size() > MaxBytes {
		return identity, fmt.Errorf("invalid executing image")
	}
	hash := sha256.New()
	reader := &contextReader{ctx, io.NewSectionReader(file, 0, before.Size())}
	n, err := io.Copy(hash, reader)
	if err != nil {
		return identity, err
	}
	after, err := file.Stat()
	if err != nil {
		return identity, err
	}
	if n != before.Size() || after.Size() != before.Size() || !after.ModTime().Equal(before.ModTime()) {
		return identity, fmt.Errorf("executing image changed while reading")
	}
	identity.SHA256, identity.Bytes = hex.EncodeToString(hash.Sum(nil)), n
	return identity, nil
}

type contextReader struct {
	context.Context
	io.Reader
}

func (r *contextReader) Read(p []byte) (int, error) {
	if err := r.Err(); err != nil {
		return 0, err
	}
	return r.Reader.Read(p)
}
