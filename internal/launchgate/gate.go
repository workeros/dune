// Package launchgate serializes connector replacement against new Runtime
// admission. Existing hosts and their control operations never take this gate.
package launchgate

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"syscall"
)

var ErrBusy = errors.New("Runtime launch or connector replacement is in progress")

// CheckDirectory observes an existing private directory without creating it.
func CheckDirectory(path string) error {
	if !filepath.IsAbs(path) || filepath.Clean(path) == "/" {
		return fmt.Errorf("absolute private state directory required")
	}
	info, err := os.Lstat(path)
	if err != nil {
		return err
	}
	owner, ok := info.Sys().(*syscall.Stat_t)
	if !info.IsDir() || info.Mode().Perm() != 0700 || !ok || int(owner.Uid) != os.Getuid() {
		return fmt.Errorf("state directory must be private, owned by this user and not a symlink")
	}
	return nil
}

func open(directory string) (*os.File, error) {
	if err := CheckDirectory(directory); err != nil {
		return nil, err
	}
	f, err := os.OpenFile(filepath.Join(directory, "launch.lock"), os.O_CREATE|os.O_RDWR|syscall.O_NOFOLLOW|syscall.O_NONBLOCK, 0600)
	if err != nil {
		return nil, err
	}
	info, err := f.Stat()
	if err == nil {
		owner, ok := info.Sys().(*syscall.Stat_t)
		if !info.Mode().IsRegular() || info.Mode().Perm() != 0600 || !ok || int(owner.Uid) != os.Getuid() || owner.Nlink != 1 {
			err = fmt.Errorf("invalid private launch gate")
		}
	}
	if err != nil {
		f.Close()
		return nil, err
	}
	return f, nil
}

// Acquire is nonblocking. Launches hold a shared gate through publication;
// upgrades hold the exclusive gate from preflight through service replacement.
// Close releases it, including when its owning process dies. It is not inherited
// by an Agent or service child.
func Acquire(directory string, exclusive bool) (*os.File, error) {
	f, err := open(directory)
	if err != nil {
		return nil, err
	}
	mode := syscall.LOCK_SH
	if exclusive {
		mode = syscall.LOCK_EX
	}
	if err = syscall.Flock(int(f.Fd()), mode|syscall.LOCK_NB); err != nil {
		f.Close()
		if errors.Is(err, syscall.EWOULDBLOCK) {
			return nil, ErrBusy
		}
		return nil, err
	}
	return f, nil
}
