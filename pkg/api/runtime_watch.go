package api

// RuntimeChange is a full observation or an exact membership removal. A
// removal cannot be reversed by a delayed metadata update. Stream interruption
// requires a new subscription and bulk discovery, not replay of Agent calls.
type RuntimeChange struct {
	Runtime Runtime `json:"runtime"`
	Removed bool    `json:"removed,omitempty"`
}

const MaxRuntimeWatchPending = 512
