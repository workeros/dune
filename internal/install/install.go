// Package install switches a single per-user connector to a verified program
// directory. Persistent configuration and Runtime state remain outside releases.
package install

import (
	"context"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/aiomni/dune/internal/config"
	"github.com/aiomni/dune/internal/service"
	"github.com/aiomni/dune/internal/wire"
)

func Run(ctx context.Context, action, configPath, root, name string) error {
	if action != "repair" && action != "upgrade" {
		return fmt.Errorf("installation action must be repair or upgrade")
	}
	cfg, err := config.Load(configPath)
	if err != nil {
		return fmt.Errorf("valid existing configuration required; do not repeat enrollment after an unknown result: %w", err)
	}
	if _, err = cfg.TLS(); err != nil {
		return err
	}
	root, err = filepath.Abs(root)
	if err != nil {
		return err
	}
	if err = os.MkdirAll(root, 0700); err != nil {
		return err
	}
	root, err = filepath.EvalSymlinks(root)
	if err != nil {
		return err
	}
	releases := filepath.Join(root, "releases")
	if info, statErr := os.Lstat(releases); statErr == nil {
		if !info.IsDir() {
			return fmt.Errorf("releases must be an owned directory, not a symlink")
		}
	} else if !os.IsNotExist(statErr) {
		return statErr
	}

	absoluteConfig, err := filepath.Abs(configPath)
	if err != nil {
		return err
	}
	for _, persistent := range []string{absoluteConfig, cfg.SessionDir, cfg.Certificate, cfg.Key} {
		if persistent == "" {
			continue
		}
		persistent, err = filepath.Abs(persistent)
		if err != nil {
			return err
		}
		persistent, err = resolveExistingParent(persistent)
		if err != nil {
			return err
		}
		if persistent == releases || strings.HasPrefix(persistent, releases+string(filepath.Separator)) {
			return fmt.Errorf("persistent configuration and Runtime state cannot be inside releases")
		}
	}
	lock, err := os.OpenFile(filepath.Join(root, ".install.lock"), os.O_CREATE|os.O_RDWR, 0600)
	if err != nil {
		return err
	}
	defer lock.Close()
	if err = syscall.Flock(int(lock.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		return fmt.Errorf("another installation is active: %w", err)
	}
	defer syscall.Flock(int(lock.Fd()), syscall.LOCK_UN)
	if action == "upgrade" {
		if _, err = os.Stat(filepath.Join(root, "current", "dune")); err != nil {
			return fmt.Errorf("no installed connector; use repair with the existing configuration")
		}
	}
	exe, err := os.Executable()
	if err != nil {
		return err
	}
	source := filepath.Dir(exe)
	if err = os.MkdirAll(releases, 0700); err != nil {
		return err
	}
	directory := filepath.Join(releases, wire.ID())
	if err = os.Mkdir(directory, 0700); err != nil {
		return err
	}
	// A failed switch retains the new directory for repair and never deletes old programs.
	for _, binary := range []string{"dune", "tmux", "rg"} {
		if err = copyProgram(filepath.Join(source, binary), filepath.Join(directory, binary)); err != nil {
			return err
		}
	}
	if err = os.CopyFS(filepath.Join(directory, "licenses"), os.DirFS(filepath.Join(source, "licenses"))); err != nil {
		return err
	}
	if err = os.WriteFile(filepath.Join(directory, ".dune-release"), []byte("1\n"), 0600); err != nil {
		return err
	}
	nonce := wire.ID()
	receipt := filepath.Join(root, ".startup-"+nonce+".json")
	defer os.Remove(receipt)
	started := time.Now()
	command := exec.CommandContext(ctx, filepath.Join(directory, "dune"), "--config", absoluteConfig, "service", "install", "--name", name, "--ready-file", receipt, "--ready-nonce", nonce)
	command.Stdout, command.Stderr = os.Stdout, os.Stderr
	paths := []string{directory}
	for _, path := range filepath.SplitList(os.Getenv("PATH")) {
		if path != source && path != releases && !strings.HasPrefix(path, releases+string(filepath.Separator)) {
			paths = append(paths, path)
		}
	}
	command.Env = append(os.Environ(), "PATH="+strings.Join(paths, string(os.PathListSeparator)))
	// The service implementation stops/drains the old connector before starting
	// this executable. tmux and timeout helpers have independent lifetimes.
	if err = command.Run(); err != nil {
		return fmt.Errorf("service switch failed; repair using the existing configuration: %w", err)
	}
	wait, cancel := context.WithTimeout(ctx, 20*time.Second)
	defer cancel()
	ticker := time.NewTicker(100 * time.Millisecond)
	defer ticker.Stop()
	for {
		if err = service.VerifyStartupReceipt(receipt, nonce, directory, started); err == nil {
			break
		}
		select {
		case <-wait.Done():
			return fmt.Errorf("new connector did not confirm local initialization; old programs retained: %w", err)
		case <-ticker.C:
		}
	}
	link := filepath.Join(root, ".current-"+nonce)
	if err = os.Symlink(directory, link); err != nil {
		return err
	}
	defer os.Remove(link)
	if err = os.Rename(link, filepath.Join(root, "current")); err != nil {
		return err
	}
	if err = cleanupReleases(releases, directory); err != nil {
		return err
	}
	fmt.Printf("Dune locally initialized: %s\nConfiguration preserved: %s\nGateway connectivity is reported separately by the workbench.\n", filepath.Join(root, "current", "dune"), absoluteConfig)
	return nil
}
func copyProgram(source, destination string) error {
	file, err := os.Open(source)
	if err != nil {
		return err
	}
	defer file.Close()
	info, err := file.Stat()
	if err != nil {
		return err
	}
	if !info.Mode().IsRegular() {
		return fmt.Errorf("program is not a regular file: %s", source)
	}
	out, err := os.OpenFile(destination, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0700)
	if err != nil {
		return err
	}
	defer out.Close()
	if _, err = io.Copy(out, file); err != nil {
		return err
	}
	return out.Sync()
}
func cleanupReleases(root, current string) error {
	entries, err := os.ReadDir(root)
	if err != nil {
		return err
	}
	for _, entry := range entries {
		path := filepath.Join(root, entry.Name())
		if path == current || !entry.IsDir() {
			continue
		}
		marker, err := os.ReadFile(filepath.Join(path, ".dune-release"))
		if err != nil || string(marker) != "1\n" {
			continue
		}
		if err = os.RemoveAll(path); err != nil {
			return fmt.Errorf("new connector started but old program cleanup failed: %w", err)
		}
	}
	return nil
}

// Resolve parents even when the last component does not yet exist.
func resolveExistingParent(path string) (string, error) {
	resolved, err := filepath.EvalSymlinks(path)
	if err == nil {
		return resolved, nil
	}
	if !os.IsNotExist(err) {
		return "", err
	}
	parent := filepath.Dir(path)
	if parent == path {
		return "", err
	}
	resolved, err = resolveExistingParent(parent)
	if err != nil {
		return "", err
	}
	return filepath.Join(resolved, filepath.Base(path)), nil
}
