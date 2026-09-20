package fabricd

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"syscall"

	"github.com/aiomni/dune/internal/sessionregistry"
	"github.com/aiomni/dune/pkg/api"
)

func cleanupIdentityError() error {
	return &api.Error{Code: "CLEANUP_IDENTITY_CHANGED", Detail: "cleanup resource no longer matches its original identity"}
}

func matchesFile(info os.FileInfo, identity sessionregistry.FileIdentity, kind os.FileMode) bool {
	stat, ok := info.Sys().(*syscall.Stat_t)
	return ok && int(stat.Uid) == os.Getuid() && info.Mode().Type() == kind && info.Mode().Perm()&0077 == 0 && uint64(stat.Dev) == identity.Device && stat.Ino == identity.Inode
}

// Cleanup holds the installation lock and acts only on a sealed Runtime. Move
// into a unique quarantine without replacing anything, then revalidate through
// a pinned directory handle. Neither a symlink nor a reused original path can
// redirect recursive deletion. An interrupted move is resumed at the same name.
func removeCleanupResource(ctx context.Context, identity sessionregistry.FileIdentity, instance string, kind os.FileMode, barrier func(string) error) error {
	parent, err := os.OpenRoot(filepath.Dir(identity.Path))
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		return err
	}
	defer parent.Close()
	parentFile, err := parent.Open(".")
	if err != nil {
		return err
	}
	defer parentFile.Close()
	name, quarantine := filepath.Base(identity.Path), ".forget-"+instance
	info, err := parent.Lstat(quarantine)
	if os.IsNotExist(err) {
		info, err = parent.Lstat(name)
		if os.IsNotExist(err) {
			return parentFile.Sync()
		}
		if err != nil {
			return err
		}
		if !matchesFile(info, identity, kind) {
			return cleanupIdentityError()
		}
		if err := ctx.Err(); err != nil {
			return err
		}
		if err := renameCleanupResource(int(parentFile.Fd()), name, quarantine); err != nil {
			return err
		}
		if err := parentFile.Sync(); err != nil {
			return err
		}
		info, err = parent.Lstat(quarantine)
	}
	if err != nil {
		return err
	}
	if !matchesFile(info, identity, kind) {
		return cleanupIdentityError()
	}
	// A new original path is never part of the accepted plan, even when the
	// original resource is already quarantined. Preserve it and report the clash.
	if _, err := parent.Lstat(name); !os.IsNotExist(err) {
		if err != nil {
			return err
		}
		return cleanupIdentityError()
	}
	if err := barrier("quarantined"); err != nil {
		return err
	}
	if kind == os.ModeDir {
		root, err := parent.OpenRoot(quarantine)
		if err != nil {
			return err
		}
		defer root.Close()
		info, err := root.Stat(".")
		if err != nil {
			return err
		}
		if !matchesFile(info, identity, kind) {
			return cleanupIdentityError()
		}
		if err := emptyCleanupDirectory(ctx, root, instance, barrier); err != nil {
			return err
		}
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	info, err = parent.Lstat(quarantine)
	if err != nil {
		return err
	}
	if !matchesFile(info, identity, kind) {
		return cleanupIdentityError()
	}
	if err := parent.Remove(quarantine); err != nil {
		return err
	}
	return parentFile.Sync()
}

func emptyCleanupDirectory(ctx context.Context, root *os.Root, instance string, barrier func(string) error) error {
	marker, err := root.OpenFile("instance.json", os.O_RDONLY|syscall.O_NOFOLLOW, 0)
	missingMarker := os.IsNotExist(err)
	if err != nil && !missingMarker {
		return err
	}
	if !missingMarker {
		defer marker.Close()
		info, err := marker.Stat()
		if err != nil {
			return err
		}
		stat, ok := info.Sys().(*syscall.Stat_t)
		if !ok || !info.Mode().IsRegular() || info.Mode().Perm() != 0600 || stat.Nlink != 1 || int(stat.Uid) != os.Getuid() || info.Size() > 256 {
			return cleanupIdentityError()
		}
		body, err := io.ReadAll(io.LimitReader(marker, 257))
		var observed string
		if err != nil || json.Unmarshal(body, &observed) != nil || observed != instance {
			return cleanupIdentityError()
		}
	}
	directory, err := root.Open(".")
	if err != nil {
		return err
	}
	defer directory.Close()
	for {
		entries, err := directory.ReadDir(128)
		if err != nil && !errors.Is(err, io.EOF) {
			return err
		}
		for _, entry := range entries {
			if missingMarker {
				// Only an empty, identity-matching quarantine can recover from
				// a crash between the last marker unlink and the final rmdir.
				return cleanupIdentityError()
			}
			if entry.Name() == "instance.json" {
				continue
			}
			if err := ctx.Err(); err != nil {
				return err
			}
			if err := root.RemoveAll(entry.Name()); err != nil {
				return fmt.Errorf("remove original Runtime cache: %w", err)
			}
		}
		if errors.Is(err, io.EOF) {
			break
		}
	}
	if err := directory.Sync(); err != nil {
		return err
	}
	if err := barrier("contents_removed"); err != nil {
		return err
	}
	if !missingMarker {
		if err := root.Remove("instance.json"); err != nil {
			return err
		}
		if err := directory.Sync(); err != nil {
			return err
		}
	}
	return barrier("marker_removed")
}
