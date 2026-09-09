package lifecycle

// ManagedPauseResume is the durable acceptance of one pause or resume request.
// MachineID is returned so the Web host can immediately close user streams
// after accepting Pause; the fabricd control connection remains authorized.
type ManagedPauseResume struct {
	Operation
	MachineID string
}
