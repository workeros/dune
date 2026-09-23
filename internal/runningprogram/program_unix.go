//go:build linux || darwin

package runningprogram

import (
	"golang.org/x/sys/unix"
	"os"
)

func duplicate(file *os.File) (*os.File, error) {
	fd, err := unix.FcntlInt(file.Fd(), unix.F_DUPFD_CLOEXEC, 0)
	if err != nil {
		return nil, err
	}
	return os.NewFile(uintptr(fd), "executing-image"), nil
}
