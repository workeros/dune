package fabricd

import (
	"slices"

	"github.com/aiomni/dune/pkg/api"
)

const (
	maxACPElicitationCompletions  = 64
	acpElicitationCompletionBytes = 128 * 1024
)

// URL completion is optional. Its bounded history correlation must not retain
// an answered request's payload or consume capacity reserved for human input.
type acpElicitationCompletion struct {
	urlID          string
	requestID      string
	conversationID string
	turnID         string
	record         api.ACPInteractionRecord
}

func (c acpElicitationCompletion) bytes() int {
	return len(c.urlID) + len(c.requestID) + len(c.conversationID) + len(c.turnID) +
		len(c.record.ID) + len(c.record.Title) + len(c.record.ToolCallID)
}

func (a *acpController) retainElicitationCompletionLocked(e *acpElicitation) {
	completion := acpElicitationCompletion{
		urlID: e.params.ElicitationID, requestID: e.requestID,
		conversationID: e.ConversationID, turnID: e.TurnID,
		record: api.ACPInteractionRecord{ID: e.ID, Kind: "elicitation", Title: e.params.Message, ToolCallID: e.params.ToolCallID},
	}
	bytes := completion.bytes()
	if bytes > acpElicitationCompletionBytes {
		a.elicitationRecordLocked(e, "expired", "")
		return
	}
	for _, retained := range a.elicitationCompletions {
		bytes += retained.bytes()
	}
	// Keep the most recent correlations when either independent budget fills.
	for len(a.elicitationCompletions) >= maxACPElicitationCompletions || bytes > acpElicitationCompletionBytes {
		bytes -= a.elicitationCompletions[0].bytes()
		a.endElicitationCompletionLocked(0, "expired")
	}
	a.elicitationCompletions = append(a.elicitationCompletions, completion)
}

func (a *acpController) endElicitationCompletionLocked(index int, state string) {
	completion := a.elicitationCompletions[index]
	completion.record.State = state
	a.interactionRecordLocked(completion.conversationID, completion.turnID, completion.record)
	a.elicitationCompletions = slices.Delete(a.elicitationCompletions, index, index+1)
}
