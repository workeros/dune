package channel

import "context"

// BindingIssues exposes durable work requiring inspection. Running/pending
// entries may still be active; callers must verify their lease and remote
// outcome before deciding that an operation can be resumed. No raw inbound
// message text or credentials are returned.
type BindingIssues struct {
	Events        []EventIssue        `json:"events"`
	Conversations []ConversationIssue `json:"conversations"`
	Deliveries    []Delivery          `json:"deliveries"`
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

// IssueStore is read-only. It does not implicitly retry, abandon or confirm
// any Agent turn or external delivery.
type IssueStore interface {
	ListIssues(context.Context, string, int) (BindingIssues, error)
}
