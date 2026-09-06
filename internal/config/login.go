package config

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"syscall"

	"github.com/aiomni/dune/pkg/login"
)

type UserCredentials struct {
	Session     login.Session `json:"session" yaml:"session"`
	Certificate string        `json:"certificate,omitempty" yaml:"certificate,omitempty"`
}

func DefaultLoginPath() string {
	return filepath.Join(filepath.Dir(DefaultPath()), "login.json")
}

// LockUserCredentials serializes login/logout without making execution hold a
// lock across a long-running command. The lock file is never deleted or reused.
func LockUserCredentials(path string) (func(), error) {
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0700); err != nil {
		return nil, err
	}
	info, err := os.Lstat(dir)
	if err != nil {
		return nil, err
	}
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok || !info.IsDir() || info.Mode().Perm()&0077 != 0 || int(stat.Uid) != os.Getuid() {
		return nil, fmt.Errorf("CLI credential directory must be private, owned and not a symlink")
	}
	f, err := os.OpenFile(path+".lock", os.O_CREATE|os.O_RDWR|syscall.O_NOFOLLOW, 0600)
	if err != nil {
		return nil, err
	}
	info, err = f.Stat()
	if err != nil {
		f.Close()
		return nil, err
	}
	stat, ok = info.Sys().(*syscall.Stat_t)
	if !ok || !info.Mode().IsRegular() || info.Mode().Perm()&0077 != 0 || int(stat.Uid) != os.Getuid() || stat.Nlink != 1 {
		f.Close()
		return nil, fmt.Errorf("invalid CLI credential lock")
	}
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		f.Close()
		return nil, fmt.Errorf("another login or logout is using this credential file")
	}
	return func() { f.Close() }, nil
}

func LoadUserCredentials(path string) (UserCredentials, error) {
	var credentials UserCredentials
	err := privateYAML(path, "CLI credential", &credentials)
	return credentials, err
}

// SaveUserCredentials never overwrites another login or machine configuration.
// Call while holding LockUserCredentials for this path.
func SaveUserCredentials(path string, credentials UserCredentials) error {
	data, err := json.MarshalIndent(credentials, "", "  ")
	if err != nil {
		return err
	}
	tmp, err := os.CreateTemp(filepath.Dir(path), ".dune-login-")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name())
	defer tmp.Close()
	if _, err = tmp.Write(append(data, '\n')); err != nil {
		return err
	}
	if err = tmp.Sync(); err != nil {
		return err
	}
	if err = tmp.Close(); err != nil {
		return err
	}
	return os.Link(tmp.Name(), path)
}
