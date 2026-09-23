package release

import (
	"context"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/aiomni/dune/pkg/upgrade"
)

// LocalManifest measures a freshly supplied distribution. It is an installation
// receipt, not a fabricated downloadable release. All resulting files use the
// current private installation modes irrespective of archive umask.
func LocalManifest(ctx context.Context, directory, id, contract string, platform upgrade.Platform) (upgrade.Manifest, error) {
	manifest := upgrade.Manifest{ID: id, Platform: platform, StateContract: contract}
	root, err := os.OpenRoot(directory)
	if err != nil {
		return manifest, err
	}
	defer root.Close()
	paths := []string{"dune", "tmux", "rg"}
	err = fs.WalkDir(root.FS(), "licenses", func(path string, entry fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if entry.Type()&os.ModeSymlink != 0 {
			return fmt.Errorf("license symlink is not a release component")
		}
		if entry.IsDir() {
			return nil
		}
		paths = append(paths, path)
		if len(paths) > upgrade.MaxComponents {
			return fmt.Errorf("too many release files")
		}
		return nil
	})
	if err != nil {
		return manifest, err
	}
	sort.Strings(paths)
	for _, path := range paths {
		observed, err := observeFile(ctx, root, path)
		if err != nil {
			return manifest, err
		}
		if !observed.Present {
			return manifest, fmt.Errorf("missing required release component")
		}
		mode := uint32(0700)
		if strings.HasPrefix(path, "licenses/") {
			mode = 0600
		}
		manifest.Components = append(manifest.Components, upgrade.Component{Path: path, SHA256: observed.SHA256, Bytes: observed.Bytes, Mode: mode})
	}
	return manifest, manifest.Validate()
}

// StageLocal copies verified bytes into private files on the installation
// filesystem; source modes are normalized, not treated as installed metadata.
func StageLocal(ctx context.Context, source, destination string, manifest upgrade.Manifest) (err error) {
	if err := manifest.Validate(); err != nil {
		return err
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
	for _, component := range manifest.Components {
		actual, err := observeFile(ctx, input, component.Path)
		if err != nil {
			return err
		}
		if !actual.Present || actual.SHA256 != component.SHA256 || actual.Bytes != component.Bytes {
			return fmt.Errorf("local release changed before copy")
		}
		if err := output.MkdirAll(filepath.Dir(component.Path), 0700); err != nil {
			return err
		}
		file, err := input.Open(component.Path)
		if err != nil {
			return err
		}
		err = writeFile(ctx, output, component.Path, file, component)
		file.Close()
		if err != nil {
			return err
		}
	}
	observations, complete, err := Observe(ctx, destination, manifest.Components)
	if err != nil {
		return err
	}
	if !complete {
		return fmt.Errorf("local staged release differs")
	}
	return syncTree(output, observations)
}
