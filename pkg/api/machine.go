package api

// MachineInfo describes the execution user of the current concrete Runner.
// UserID and Home locate native Agent storage; they do not grant access.
type MachineInfo struct {
	Home   string `json:"home"`
	UserID string `json:"user_id"`
	OS     string `json:"os"`
	Arch   string `json:"arch"`
}
