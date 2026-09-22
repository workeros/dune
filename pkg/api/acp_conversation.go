package api

import (
	"encoding/json"
	"fmt"
)

const (
	DefaultACPConversationLimit         = 50
	MaxACPConversationLimit             = 200
	MaxACPConversationBytes             = 8 * 1024 * 1024
	MaxACPConversationEntries           = 5000
	MaxACPConversationsBytes            = 128 * 1024 * 1024
	MaxACPEntryBytes                    = 256 * 1024
	MaxACPConversationStateBytes        = 64 * 1024
	MaxACPConversationResponseBytes     = 512 * 1024
	MaxACPConversationNotificationBytes = 16 * 1024
)

// ACPState describes the live controller. Conversation describes retained data,
// independently of whether the Agent can still accept an operation.
type ACPState struct {
	OperationRef       string                `json:"operation_ref,omitempty"`
	Pending            int                   `json:"pending"`
	Revision           uint64                `json:"revision"`
	Ready              bool                  `json:"ready"`
	Busy               string                `json:"busy"`
	SessionID          string                `json:"session_id"`
	Cwd                string                `json:"cwd"`
	CanList            bool                  `json:"can_list"`
	CanLoad            bool                  `json:"can_load"`
	PromptCapabilities ACPPromptCapabilities `json:"prompt_capabilities"`
	MCPTransport       string                `json:"mcp_transport,omitempty"`
	Agent              json.RawMessage       `json:"agent,omitempty"`
	Permissions        []ACPPermission       `json:"permissions"`
	Elicitations       []ACPElicitation      `json:"elicitations"`
	List               json.RawMessage       `json:"list,omitempty"`
	Error              string                `json:"error,omitempty"`
	StopReason         string                `json:"stop_reason,omitempty"`
	Conversation       *ACPConversation      `json:"conversation"`
	Resources          *ACPResourceUsage     `json:"resources,omitempty"`
}

type ACPPromptCapabilities struct {
	Image           bool `json:"image"`
	Audio           bool `json:"audio"`
	EmbeddedContext bool `json:"embeddedContext"`
}

type ACPPermission struct {
	ID             string          `json:"id"`
	ConversationID string          `json:"conversation_id"`
	TurnID         string          `json:"turn_id,omitempty"`
	Params         json.RawMessage `json:"params"`
}

// Request-scoped questions can arrive during initialize, before a conversation
// exists. ID belongs to this host/connection, not to an Agent-provided URL ID.
type ACPElicitation struct {
	ID             string          `json:"id"`
	ConversationID string          `json:"conversation_id,omitempty"`
	TurnID         string          `json:"turn_id,omitempty"`
	Params         json.RawMessage `json:"params"`
	FormError      string          `json:"form_error,omitempty"`
}

// ACPInteractionRecord is a read-only transcript fact. Executable requests
// exist only in the live controller state and are consumed under its lock.
type ACPInteractionRecord struct {
	ID         string `json:"id"`
	Kind       string `json:"kind"`
	State      string `json:"state"`
	Title      string `json:"title"`
	ToolCallID string `json:"tool_call_id,omitempty"`
	Response   string `json:"response,omitempty"`
}

type ACPFailure struct {
	Code   string `json:"code"`
	Detail string `json:"detail"`
}

// ACPConversation revision orders commits only within ID. NativeSession and
// controller revisions are separate counters. Reads never open a session.
type ACPConversation struct {
	ID                    string                     `json:"conversation_id"`
	Revision              uint64                     `json:"revision,string"`
	Phase                 string                     `json:"phase"`
	Origin                string                     `json:"origin"`
	OpenOutcome           string                     `json:"open_outcome"`
	OpenError             *ACPFailure                `json:"open_error"`
	RequestedSessionID    string                     `json:"requested_session_id,omitempty"`
	RequestedCwd          string                     `json:"requested_cwd"`
	SessionID             string                     `json:"session_id,omitempty"`
	Cwd                   string                     `json:"cwd,omitempty"`
	HeadOrder             uint64                     `json:"head_order,string"`
	RetainedFromOrder     uint64                     `json:"retained_from_order,string"`
	RetainedEntryCount    int                        `json:"retained_entry_count"`
	PrefixEvicted         bool                       `json:"prefix_evicted"`
	ContentOmitted        bool                       `json:"content_omitted"`
	ContextIncomplete     bool                       `json:"context_incomplete"`
	NativeHistoryCoverage string                     `json:"native_history_coverage"`
	CurrentTurn           *ACPTurn                   `json:"current_turn,omitempty"`
	ExitCode              *int                       `json:"exit_code,omitempty"`
	State                 map[string]json.RawMessage `json:"state,omitempty"`
}

// ACPEntry is one current protocol object, not an ACP chunk or an executable
// permission. Exactly one payload matches Type: message, tool, activity, turn.
type ACPEntry struct {
	ID                string        `json:"entry_id"`
	Order             uint64        `json:"order,string"`
	Revision          uint64        `json:"entry_revision,string"`
	Type              string        `json:"type"`
	TurnID            string        `json:"turn_id,omitempty"`
	Message           *ACPMessage   `json:"message,omitempty"`
	Tool              *ACPTool      `json:"tool,omitempty"`
	Activity          *ACPActivity  `json:"activity,omitempty"`
	Turn              *ACPTurn      `json:"turn,omitempty"`
	ContentOmitted    bool          `json:"content_omitted"`
	ContextIncomplete bool          `json:"context_incomplete"`
	Omissions         []ACPOmission `json:"omissions,omitempty"`
}

// ACPOmission describes retained partial data without pretending a truncated
// value was the original protocol field. Unknown lost byte counts are omitted.
type ACPOmission struct {
	Path   string `json:"path"`
	Reason string `json:"reason"`
}

type ACPMessage struct {
	Role      string            `json:"role"`
	Channel   string            `json:"channel"`
	MessageID string            `json:"message_id,omitempty"`
	Source    string            `json:"source"`
	Status    string            `json:"status"`
	Content   []json.RawMessage `json:"content"`
	// Tail follows an omitted region after Content. It is not contiguous text.
	Tail []json.RawMessage `json:"tail,omitempty"`
}

// Fields preserve ACP's presence/null distinction and structured content. ID is
// the native tool identifier, scoped by the entry's conversation and known turn.
type ACPTool struct {
	ID           string                     `json:"tool_call_id"`
	Status       string                     `json:"status"`
	StatusReason string                     `json:"status_reason,omitempty"`
	Fields       map[string]json.RawMessage `json:"fields"`
}

type ACPActivity struct {
	UpdateType string          `json:"update_type"`
	Data       json.RawMessage `json:"data"`
}

type ACPTurn struct {
	ID           string      `json:"turn_id"`
	OperationRef string      `json:"operation_ref"`
	State        string      `json:"state"`
	StopReason   string      `json:"stop_reason,omitempty"`
	Error        *ACPFailure `json:"error,omitempty"`
}

type ACPConversationRead struct {
	ConversationID string `json:"conversation_id,omitempty"`
	Cursor         string `json:"cursor,omitempty"`
	// Nil selects the default. Explicit zero is invalid.
	Limit *int `json:"limit,omitempty"`
}

func (r *ACPConversationRead) UnmarshalJSON(data []byte) error {
	type request ACPConversationRead
	var decoded request
	if err := json.Unmarshal(data, &decoded); err != nil {
		return err
	}
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(data, &fields); err != nil {
		return err
	}
	if limit, ok := fields["limit"]; ok && string(limit) == "null" {
		return fmt.Errorf("limit cannot be null")
	}
	*r = ACPConversationRead(decoded)
	return nil
}

// ACPConversationUsage is aggregate, bounded diagnostic data. EncodedBytes is
// retained JSON accounting, not an estimate of process RSS.
type ACPConversationUsage struct {
	NotificationMerges   uint64            `json:"notification_merges"`
	SlowConsumerClosures uint64            `json:"slow_consumer_closures"`
	MergeFailures        map[string]uint64 `json:"merge_failures,omitempty"`
	Models               int               `json:"models"`
	EncodedBytes         int               `json:"encoded_bytes"`
	Entries              int               `json:"entries"`
	Evictions            uint64            `json:"evictions"`
	OmittedUpdates       uint64            `json:"omitted_updates"`
	ReadCount            uint64            `json:"read_count"`
	ReadBytes            uint64            `json:"read_bytes"`
	ReadNanoseconds      uint64            `json:"read_nanoseconds"`
	InFlightLoads        int               `json:"in_flight_loads"`
}

type ACPConversationPage struct {
	Conversation ACPConversation `json:"conversation"`
	Entries      []ACPEntry      `json:"entries"`
	ThroughOrder uint64          `json:"through_order,string"`
	NextCursor   string          `json:"next_cursor,omitempty"`
	HasMore      bool            `json:"has_more"`
	RangeEvicted bool            `json:"range_evicted"`
}

type ACPConversationGet struct {
	ConversationID string   `json:"conversation_id"`
	EntryIDs       []string `json:"entry_ids"`
}

type ACPMissingEntry struct {
	ID     string `json:"entry_id"`
	Reason string `json:"reason"`
}

type ACPConversationEntries struct {
	Conversation        ACPConversation   `json:"conversation"`
	Entries             []ACPEntry        `json:"entries"`
	Missing             []ACPMissingEntry `json:"missing"`
	UnprocessedEntryIDs []string          `json:"unprocessed_entry_ids"`
}

type ACPConversationChanged struct {
	ConversationID   string   `json:"conversation_id"`
	PreviousRevision uint64   `json:"previous_revision,string"`
	Revision         uint64   `json:"revision,string"`
	ChangedEntryIDs  []string `json:"changed_entry_ids,omitempty"`
	InvalidatesAll   bool     `json:"invalidates_all"`
}
