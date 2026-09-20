package process

import (
	"errors"
	"fmt"
	"syscall"
)

// Absent is strictly observational. A live/reused PID or group is inconclusive,
// never authority to signal it. A different kernel boot proves old processes
// cannot survive; the caller must separately verify the recorded installation.
func Absent(boot string, pid, group int) (bool, error) {
	current, err := BootID()
	if err != nil {
		return false, err
	}
	if boot == "" || pid <= 1 || group < 0 {
		return false, fmt.Errorf("complete host process evidence required")
	}
	if current != boot {
		return true, nil
	}
	for _, target := range []int{pid, -group} {
		if target == 0 {
			continue
		}
		err := syscall.Kill(target, 0)
		if err == nil {
			return false, nil
		}
		if errors.Is(err, syscall.EPERM) {
			// macOS can temporarily report EPERM while a killed group is being
			// reaped. Permission denial is inconclusive, never absence proof.
			return false, nil
		}
		if !errors.Is(err, syscall.ESRCH) {
			return false, err
		}
	}
	return true, nil
}
