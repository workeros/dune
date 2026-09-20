package api

// RuntimeList is a bounded observation, not evidence that absent Runtimes
// ended. Incomplete discovery preserves usable items and identifies local issues.
type RuntimeList struct {
	Items    []Runtime               `json:"items"`
	Complete bool                    `json:"complete"`
	Issues   []RuntimeDiscoveryIssue `json:"issues"`
}

// Runtime is present only when an exact original identity could be verified.
// Code never embeds registration contents, paths, credentials or process output.
type RuntimeDiscoveryIssue struct {
	Runtime *Runtime `json:"runtime,omitempty"`
	Code    string   `json:"code"`
}
