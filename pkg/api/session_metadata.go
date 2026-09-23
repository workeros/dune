package api

// MaxSessionTitleBytes bounds the decoded UTF-8 title before trimming. Invalid
// or oversized updates leave the previous title unchanged.
const MaxSessionTitleBytes = 1024

// SessionMetadata is a complete observation, never an ACP partial update.
// Revision orders metadata across conversations within one exact Runtime.
// Neither a title nor its revision establishes directory membership or proves
// that a native session open succeeded.
type SessionMetadata struct {
	Revision       uint64  `json:"revision,string"`
	ConversationID *string `json:"conversation_id"`
	Title          *string `json:"title"`
}
