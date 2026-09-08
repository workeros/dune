package lifecycle

import "time"

const (
	ReviewReconcile = "reconcile"
	ReviewCandidate = "candidate"
)

// Review is one authorized, durable request to re-check an unresolved provider
// action. Candidate is untrusted until a provider adapter verifies its
// association with the original operation.
type Review struct {
	ID, RequestKey, Digest          string
	PrincipalID, Namespace, Subject string
	OperationID, ActionID, Mode     string
	Candidate, Reason               string
	CreatedAt, CompletedAt          time.Time
	Outcome, VerifiedResourceRef    string
	Lease
}

// ReviewRequest is the bounded public input accepted after current access
// checks. Operation and provider action identities are resolved by the server.
type ReviewRequest struct {
	OperationID string `json:"operation_id"`
	Mode        string `json:"mode"`
	Candidate   string `json:"candidate_resource_ref,omitempty"`
	Reason      string `json:"reason"`
}
