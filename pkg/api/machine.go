package api

// MachineInfo describes the execution user of the current concrete Runner.
// UserID and Home locate native Agent storage; they do not grant access.
type MachineInfo struct {
	Home               string                   `json:"home"`
	UserID             string                   `json:"user_id"`
	OS                 string                   `json:"os"`
	Arch               string                   `json:"arch"`
	Connector          *ConnectorInfo           `json:"connector,omitempty"`
	Tmux               []TmuxVersion            `json:"tmux,omitempty"`
	ACPConversations   *ACPConversationUsage    `json:"acp_conversations,omitempty"`
	StreamCapacity     map[string]CapacityUsage `json:"stream_capacity,omitempty"`
	SubmissionCapacity *SubmissionCapacity      `json:"submission_capacity,omitempty"`
}

// CapacityUsage reports a bounded resource's currently occupied units and hard
// limit. Categories are independent: free reserved units cannot admit ordinary
// work. A snapshot is diagnostic, not a promise of subsequent admission.
type CapacityUsage struct {
	Used  int `json:"used"`
	Limit int `json:"limit"`
}
