package process

import (
	"fmt"
	"golang.org/x/sys/unix"
)

func BootID() (string, error) {
	boot, err := unix.SysctlTimeval("kern.boottime")
	if err != nil {
		return "", err
	}
	return fmt.Sprintf("%d-%d", boot.Sec, boot.Usec), nil
}
