// Package channel defines the provider-independent boundary for Dune IM bots.
package channel

import (
	"context"
	"encoding/json"
)

// BotBinding is owned and authorized by the embedding application. Config must
// not contain credentials; CredentialRef identifies a provider-specific bundle.
type BotBinding struct {
	ID            string          `json:"id"`
	TenantID      string          `json:"tenant_id"`
	Name          string          `json:"name"`
	Provider      string          `json:"provider"`
	ConfigVersion int             `json:"config_version"`
	Config        json.RawMessage `json:"config"`
	CredentialRef string          `json:"credential_ref"`
	Target        AgentTarget     `json:"target"`
	Enabled       bool            `json:"enabled"`
	Revision      int64           `json:"revision"`
}

type AgentTarget struct {
	RunnerID         string `json:"runner_id"`
	AgentConfigID    string `json:"agent_config_id"`
	WorkingDirectory string `json:"working_directory"`
}

// InboundMessage is a verified, normalized event. Provider-specific fields in
// Address remain opaque to the common router and are validated by the provider.
type InboundMessage struct {
	BindingID       string       `json:"binding_id"`
	BindingRevision int64        `json:"binding_revision,omitempty"`
	EventID         string       `json:"event_id"`
	MessageID       string       `json:"message_id"`
	SenderID        string       `json:"sender_id"`
	ChatID          string       `json:"chat_id"`
	ChatKind        ChatKind     `json:"chat_kind"`
	MentionedIDs    []string     `json:"mentioned_ids,omitempty"`
	Text            string       `json:"text"`
	Address         ReplyAddress `json:"address"`
}

type ChatKind string

const (
	ChatDirect ChatKind = "direct"
	ChatGroup  ChatKind = "group"
)

type ReplyAddress struct {
	Provider string          `json:"provider"`
	Version  int             `json:"version"`
	Data     json.RawMessage `json:"data"`
}

type OutboundMessage struct {
	Text               string     `json:"text"`
	DeliveryID         string     `json:"delivery_id,omitempty"` // stable logical turn ID for idempotent delivery tracking
	Session            SessionKey `json:"session"`
	AgentTurnCompleted bool       `json:"agent_turn_completed,omitempty"` // authoritative Agent final, not merely a closed reply
}

type Provider interface {
	Kind() string
	Validate(config json.RawMessage, credentials []byte) error
	Open(ctx context.Context, binding BotBinding, credentials []byte, sink EventSink) (Channel, error)
}

type Channel interface {
	Start(ctx context.Context) error
	Stop(ctx context.Context) error
	Send(ctx context.Context, address ReplyAddress, message OutboundMessage) (json.RawMessage, error)
}

type StreamingChannel interface {
	OpenStream(ctx context.Context, address ReplyAddress, initial OutboundMessage) (ReplyStream, error)
}

type ReplyStream interface {
	Update(ctx context.Context, message OutboundMessage) error
	Complete(ctx context.Context, message OutboundMessage) error
}

// EventSink is the sole inbound boundary. Providers own their transports and
// invoke Accept only after platform-specific verification and normalization.
type EventSink interface {
	Accept(ctx context.Context, message InboundMessage) error
}
