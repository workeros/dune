package process

import (
	"time"

	"golang.org/x/sys/unix"
)

// CLOCK_BOOTTIME includes suspended time without following wall-clock changes.
func elapsedTime() (time.Duration, error) {
	var ts unix.Timespec
	err := unix.ClockGettime(unix.CLOCK_BOOTTIME, &ts)
	return time.Duration(ts.Nano()), err
}
