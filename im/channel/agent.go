package channel

import "context"

// AgentCapabilities are checked before a turn. CardKit streaming requires a
// managed ACP backend with ordered assistant-body deltas and a reliable final.
type AgentCapabilities struct {
	Adapter         string
	AssistantDeltas bool
	ReliableFinal   bool
}

type AgentSession struct {
	Runtime      RuntimeHandle
	ACPSessionID string
	ContextLost  bool // a replacement Runtime could not load the prior ACP session
}

type AgentEventKind string

const (
	AgentDelta AgentEventKind = "delta"
	AgentFinal AgentEventKind = "final"
	AgentError AgentEventKind = "error"
)

// Delta contains only assistant answer text, never thinking or tool content.
// Prompt's returned final text is authoritative for the completed turn.
type AgentEvent struct {
	Kind AgentEventKind
	Text string
}

type AgentBackend interface {
	Capabilities(context.Context, ConversationSession) (AgentCapabilities, error)
	Start(context.Context, ConversationSession) (AgentSession, error)
	Attach(context.Context, ConversationSession, AgentSession) (AgentSession, error)
	Prompt(context.Context, ConversationSession, AgentSession, string, func(AgentEvent) error) (string, error)
	Stop(context.Context, ConversationSession, AgentSession) error
}
