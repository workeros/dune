package agents

import (
	"context"

	"github.com/aiomni/dune/pkg/api"
)

type ResumeRequest struct {
	SessionID string `json:"session_record_id"`
	Revision  int64  `json:"revision"`
}

// ResumeResult preserves confirmed partial progress. A pending/unknown attempt
// must be inspected rather than started again. Only Session.Status=available
// with a ready resume attempt confirms recovery of the original conversation.
type ResumeResult struct {
	Session   *Summary     `json:"session,omitempty"`
	Runtime   *api.Runtime `json:"runtime,omitempty"`
	Operation *Operation   `json:"operation,omitempty"`
}

type Restorer interface {
	Resume(context.Context, Scope, ResumeRequest) (ResumeResult, error)
}
