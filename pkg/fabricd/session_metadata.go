package fabricd

import (
	"bytes"
	"encoding/json"
	"strings"
	"unicode"
	"unicode/utf8"

	"github.com/aiomni/dune/pkg/api"
)

// Called under the conversation store lock. This is the single title merger
// for both ACP versions; transports only carry the resulting immutable value.
func (s *conversationSlot) mergeSessionInfo(update map[string]json.RawMessage) {
	raw, present := update["title"]
	if !present {
		return
	}
	title, valid := sessionTitle(raw)
	if !valid {
		s.store.mergeFailureLocked("invalid_session_title")
		return
	}
	if equalTitle(s.metadata.Title, title) {
		return
	}
	s.metadata.Revision++
	s.metadata.Title = title
	s.model.description.SessionMetadata = s.metadata
	s.model.invalidatesAll = true
}

func sessionTitle(raw json.RawMessage) (*string, bool) {
	if bytes.Equal(bytes.TrimSpace(raw), []byte("null")) {
		return nil, true
	}
	var title string
	if !utf8.Valid(raw) || json.Unmarshal(raw, &title) != nil || len(title) > api.MaxSessionTitleBytes {
		return nil, false
	}
	title = strings.TrimSpace(title)
	if title == "" {
		return nil, true
	}
	if strings.ContainsFunc(title, func(r rune) bool {
		return unicode.IsControl(r) || r == '\u2028' || r == '\u2029'
	}) {
		return nil, false
	}
	return &title, true
}

func equalTitle(a, b *string) bool {
	return a == nil && b == nil || a != nil && b != nil && *a == *b
}
