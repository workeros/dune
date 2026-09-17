package process

import (
	"time"

	"golang.org/x/sys/unix"
)

// Darwin's MONOTONIC_RAW uses continuous time, including sleep. UPTIME_RAW
// (and Go's ordinary monotonic clock) would pause while the machine sleeps.
func elapsedTime() (time.Duration, error) {
	var ts unix.Timespec
	err := unix.ClockGettime(unix.CLOCK_MONOTONIC_RAW, &ts)
	return time.Duration(ts.Nano()), err
}
