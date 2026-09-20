package fabricd

import (
	"fmt"
	"os"
	"path/filepath"
	"syscall"

	"github.com/aiomni/dune/internal/sessionregistry"
)

func fileIdentity(path string, kind os.FileMode) (sessionregistry.FileIdentity, error) {
	info, err := os.Lstat(path)
	if err != nil {
		return sessionregistry.FileIdentity{}, err
	}
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok || int(stat.Uid) != os.Getuid() || info.Mode().Type() != kind || info.Mode().Perm()&0077 != 0 {
		return sessionregistry.FileIdentity{}, fmt.Errorf("session resource is not private or has changed type")
	}
	return sessionregistry.FileIdentity{Path: path, Device: uint64(stat.Dev), Inode: stat.Ino}, nil
}

// Capture before RegisterHost and before opening the guardian start gate. The
// independent cleanup plan must never infer resource identity from a path that
// first became visible after the original host was lost.
func captureSessionResources(directory, socket string, reg sessionRegistration) (sessionregistry.CleanupResources, error) {
	result := sessionregistry.CleanupResources{Instance: reg.Instance, TmuxSession: "acp-" + reg.Runtime.ID}
	var instance string
	if err := privateFile(filepath.Join(directory, "instance.json"), 256, &instance); err != nil || instance != reg.Instance {
		return result, fmt.Errorf("Runtime directory instance does not match its host")
	}
	var err error
	result.Directory, err = fileIdentity(directory, os.ModeDir)
	if err != nil {
		return result, err
	}
	result.Socket, err = fileIdentity(socket, os.ModeSocket)
	if err != nil {
		return result, err
	}
	return result, result.Validate(reg.Target, reg.Instance)
}
