// Package release verifies and stages complete immutable Dune distributions.
// It has no service, configuration, session or operation side effects.
package release

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"os"
	"path"
	"strings"
	"syscall"

	"github.com/aiomni/dune/pkg/upgrade"
)

func Observe(ctx context.Context, directory string, components []upgrade.Component) ([]upgrade.ComponentObservation, bool, error) {
	if err := upgrade.ValidateComponents(components); err != nil {
		return nil, false, err
	}
	root, err := os.OpenRoot(directory)
	if err != nil {
		return nil, false, err
	}
	defer root.Close()
	observations := make([]upgrade.ComponentObservation, 0, len(components))
	complete := true
	for _, component := range components {
		observation, err := observeFile(ctx, root, component.Path)
		if err != nil {
			return nil, false, err
		}
		observation.Matches = observation.Present && observation.SHA256 == component.SHA256 && observation.Bytes == component.Bytes && observation.Mode == component.Mode
		complete = complete && observation.Matches
		observations = append(observations, observation)
	}
	return observations, complete, nil
}

func observeFile(ctx context.Context, root *os.Root, name string) (upgrade.ComponentObservation, error) {
	observation := upgrade.ComponentObservation{Path: name}
	// os.Root prevents escapes; additionally reject intermediate symlink aliases
	// so a component's declared path has exactly one installation meaning.
	for parent := path.Dir(name); parent != "."; parent = path.Dir(parent) {
		info, err := root.Lstat(parent)
		if os.IsNotExist(err) {
			return observation, nil
		}
		if err != nil {
			return observation, err
		}
		if !info.IsDir() {
			return observation, fmt.Errorf("release component parent is not a directory")
		}
	}
	file, err := root.OpenFile(name, os.O_RDONLY|syscall.O_NOFOLLOW|syscall.O_NONBLOCK, 0)
	if os.IsNotExist(err) {
		return observation, nil
	}
	if err != nil {
		return observation, err
	}
	defer file.Close()
	info, err := file.Stat()
	if err != nil {
		return observation, err
	}
	owner, ok := info.Sys().(*syscall.Stat_t)
	if !info.Mode().IsRegular() || !ok || int(owner.Uid) != os.Getuid() || owner.Nlink != 1 || info.Size() > upgrade.MaxProgramBytes {
		return observation, fmt.Errorf("unverifiable release component file")
	}
	digest := sha256.New()
	n, err := io.Copy(digest, &contextReader{ctx, io.LimitReader(file, upgrade.MaxProgramBytes+1)})
	if err != nil {
		return observation, err
	}
	after, err := file.Stat()
	if err != nil {
		return observation, err
	}
	if n != info.Size() || after.Size() != info.Size() || after.Mode() != info.Mode() || !after.ModTime().Equal(info.ModTime()) {
		return observation, fmt.Errorf("release component changed while observing")
	}
	observation.Present, observation.Bytes, observation.Mode = true, n, uint32(info.Mode().Perm())
	observation.SHA256 = hex.EncodeToString(digest.Sum(nil))
	return observation, nil
}

// Copy creates an installation-local staging directory without cross-device
// renames. Source holes remain holes, which also supports exact rollback images.
func Copy(ctx context.Context, source, destination string, observations []upgrade.ComponentObservation) (err error) {
	if len(observations) == 0 || len(observations) > upgrade.MaxComponents {
		return fmt.Errorf("bounded source observation required")
	}
	input, err := os.OpenRoot(source)
	if err != nil {
		return err
	}
	defer input.Close()
	if err := os.Mkdir(destination, 0700); err != nil {
		return err
	}
	defer func() {
		if err != nil {
			_ = os.RemoveAll(destination)
		}
	}()
	output, err := os.OpenRoot(destination)
	if err != nil {
		return err
	}
	defer output.Close()
	for _, expected := range observations {
		if !validPath(expected.Path) {
			return fmt.Errorf("invalid observed component path")
		}
		actual, err := observeFile(ctx, input, expected.Path)
		if err != nil {
			return err
		}
		actual.Matches, expected.Matches = false, false
		if actual != expected {
			return fmt.Errorf("source installation changed before staging")
		}
		if !expected.Present {
			continue
		}
		if err := output.MkdirAll(path.Dir(expected.Path), 0700); err != nil {
			return err
		}
		file, err := input.OpenFile(expected.Path, os.O_RDONLY|syscall.O_NOFOLLOW, 0)
		if err != nil {
			return err
		}
		err = writeFile(ctx, output, expected.Path, file, upgrade.Component{Path: expected.Path, SHA256: expected.SHA256, Bytes: expected.Bytes, Mode: expected.Mode})
		file.Close()
		if err != nil {
			return err
		}
	}
	return syncTree(output, observations)
}

func validPath(name string) bool {
	return name != "" && len(name) <= 256 && path.Clean(name) == name && !strings.ContainsAny(name, "\\\x00\r\n") && (name == "dune" || name == "tmux" || name == "rg" || strings.HasPrefix(name, "licenses/"))
}

func writeFile(ctx context.Context, root *os.Root, name string, input io.Reader, expected upgrade.Component) error {
	file, err := root.OpenFile(name, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0600)
	if err != nil {
		return err
	}
	defer file.Close()
	digest := sha256.New()
	n, err := io.Copy(io.MultiWriter(file, digest), &contextReader{ctx, io.LimitReader(input, expected.Bytes+1)})
	if err != nil {
		return err
	}
	if n != expected.Bytes || hex.EncodeToString(digest.Sum(nil)) != expected.SHA256 {
		return fmt.Errorf("release component digest or size mismatch")
	}
	if err := file.Chmod(os.FileMode(expected.Mode)); err != nil {
		return err
	}
	if err := file.Sync(); err != nil {
		return err
	}
	return file.Close()
}

func syncTree(root *os.Root, observations []upgrade.ComponentObservation) error {
	seen := make(map[string]bool)
	for _, c := range observations {
		if !c.Present {
			continue
		}
		for dir := path.Dir(c.Path); !seen[dir]; dir = path.Dir(dir) {
			file, err := root.Open(dir)
			if err != nil {
				return err
			}
			err = file.Sync()
			file.Close()
			if err != nil {
				return err
			}
			seen[dir] = true
			if dir == "." {
				break
			}
		}
	}
	return nil
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
