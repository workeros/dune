package api

import "time"

// RunningProgram identifies the kernel-selected executable and process start.
// It is independent of the installation's current release and retained hosts.
type RunningProgram struct {
	PID        int       `json:"pid"`
	StartID    string    `json:"start_id"`
	SHA256     string    `json:"sha256"`
	Bytes      int64     `json:"bytes"`
	Build      BuildInfo `json:"build"`
	ObservedAt time.Time `json:"observed_at"`
}
