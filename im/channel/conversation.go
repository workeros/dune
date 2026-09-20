package channel

import (
	"context"
	"time"
)

// RuntimeHandle identifies a concrete Dune Runtime. A later Runtime with the
// same ID but a different incarnation or generation must not be attached as
// if it were the original conversation process.
type RuntimeHandle struct {
	ID          string `json:"id"`
	Incarnation string `json:"incarnation"`
	Generation  uint64 `json:"generation"`
	Adapter     string `json:"adapter"`
}

// ConversationSession freezes the Agent target when the conversation is first
// created. Updating a BotBinding never silently moves an existing session.
type ConversationSession struct {
	Key               SessionKey    `json:"key"`
	ProviderThreadRef string        `json:"provider_thread_ref,omitempty"`
	Target            AgentTarget   `json:"target"`
	Runtime           RuntimeHandle `json:"runtime"`
	ConversationID    string        `json:"conversation_id,omitempty"`
	ACPSessionID      string        `json:"acp_session_id,omitempty"`
	Address           ReplyAddress  `json:"address"`
	Revision          int64         `json:"revision"`
}

type ConversationState string

const (
	ConversationReady   ConversationState = "ready"
	ConversationRunning ConversationState = "running"
	ConversationUnknown ConversationState = "unknown"
)

// ConversationLease is a fencing token for one session. The token is opaque
// and scoped to Key; callers must not infer ownership from a process mutex.
type ConversationLease struct {
	Key   SessionKey
	Token string
}

// ConversationStore persists the session and serializes turns across workers.
// BeginTurn must commit before WorkQueue.BeginSubmission and Agent prompt.
// A running or unknown session is never automatically taken over on expiry.
type ConversationStore interface {
	Ensure(context.Context, SessionKey, AgentTarget, ReplyAddress, string) (ConversationSession, error)
	FindByThreadRef(context.Context, string, string, string) (SessionKey, bool, error)
	Get(context.Context, SessionKey) (ConversationSession, ConversationState, bool, error)
	Acquire(context.Context, SessionKey, time.Duration) (ConversationLease, bool, error)
	Renew(context.Context, ConversationLease, time.Duration) error
	Save(context.Context, ConversationLease, ConversationSession) (ConversationSession, error)
	BeginTurn(context.Context, ConversationLease, string) error
	FinishTurn(context.Context, ConversationLease) error
	UnknownTurn(context.Context, ConversationLease, string) error
	ReleaseLease(context.Context, ConversationLease) error
}
