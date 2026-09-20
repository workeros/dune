package api

// LifecycleLogUsage describes best-effort diagnostics, not admission evidence.
// Overflow never blocks Agent I/O or protected controls.
type LifecycleLogUsage struct {
	Bytes       CapacityUsage `json:"bytes"`
	Queue       CapacityUsage `json:"queue"`
	Dropped     uint64        `json:"dropped"`
	WriteErrors uint64        `json:"write_errors"`
}
