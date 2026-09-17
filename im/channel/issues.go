package channel

import "context"

// BindingIssues exposes durable work requiring inspection. Running/pending
// entries may still be active; callers must verify their lease and remote
// outcome before deciding that an operation can be resumed. No raw inbound
// or outbound message text, reply addresses, provider state, or
// credentials are returned.
type BindingIssues struct {
	Events        []EventIssue        `json:"events"`
	Conversations []ConversationIssue `json:"conversations"`
	Deliveries    []DeliveryIssue     `json:"deliveries"`
}

type EventIssue struct {
	EventID  string `json:"event_id"`
	ChatID   string `json:"chat_id"`
	State    string `json:"state"`
	Attempts int    `json:"attempts"`
	Failure  string `json:"failure,omitempty"`
}

type ConversationIssue struct {
	Key            SessionKey `json:"key"`
	State          string     `json:"state"`
	CurrentEventID string     `json:"current_event_id,omitempty"`
	Failure        string     `json:"failure,omitempty"`
}

// DeliveryIssue is a safe diagnostic projection, not the full durable
// Delivery. ProviderState may contain the Agent's answer and must stay in the
// store until an explicitly authorized reconciliation reads it.
type DeliveryIssue struct {
	ID                 string     `json:"id"`
	Session            SessionKey `json:"session"`
	Mode               string     `json:"mode"`
	Phase              string     `json:"phase"`
	Operation          string     `json:"operation,omitempty"`
	AgentTurnCompleted bool       `json:"agent_turn_completed"`
}

// IssueStore is read-only. It does not implicitly retry, abandon or confirm
// any Agent turn or external delivery.
type IssueStore interface {
	ListIssues(context.Context, string, int) (BindingIssues, error)
}
