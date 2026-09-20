package agents

import "github.com/aiomni/dune/pkg/api"

// SubmissionRequest sends an explicitly identified managed ACP action. Save
// both selectors before sending. The action is frozen; querying never repeats it.
type SubmissionRequest struct {
	SubmissionID string `json:"submission_id"`
	AgentRef     string `json:"agent_ref"`
	api.ACPAction
}

// SubmissionQuery uses the original reference even after its native session has
// changed or the Runtime has been forgotten. Current authorization still applies.
type SubmissionQuery struct {
	SubmissionID string `json:"submission_id"`
	AgentRef     string `json:"agent_ref"`
}
