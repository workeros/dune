package fabricd

import (
	"encoding/base64"
	"encoding/json"
	"strconv"
	"strings"

	"github.com/aiomni/dune/pkg/api"
)

type conversationCursor struct {
	Runtime      string `json:"runtime"`
	Incarnation  string `json:"incarnation"`
	Conversation string `json:"conversation"`
	Through      uint64 `json:"through"`
	Before       uint64 `json:"before"`
}

func encodeConversationCursor(cursor conversationCursor) string {
	return base64.RawURLEncoding.EncodeToString(api.Payload(cursor))
}

func conversationArgument(detail string) error {
	return &api.Error{Code: "INVALID_ARGUMENT", Detail: detail}
}

func (s *conversationSlot) selectLocked(id string) (*conversationModel, error) {
	if s.model == nil {
		return nil, &api.Error{Code: "CONVERSATION_UNAVAILABLE", Detail: "Runtime has no retained conversation"}
	}
	if id != s.model.description.ID {
		return nil, &api.Error{Code: "CONVERSATION_CHANGED", Detail: "Runtime conversation has changed"}
	}
	return s.model, nil
}

func (s *conversationSlot) read(request api.ACPConversationRead) (api.ACPConversationPage, error) {
	page := api.ACPConversationPage{Entries: []api.ACPEntry{}}
	limit := api.DefaultACPConversationLimit
	if request.Limit != nil {
		limit = *request.Limit
	}
	if limit < 1 || limit > api.MaxACPConversationLimit {
		return page, conversationArgument("limit must be 1..200 or omitted")
	}
	if (request.ConversationID == "") == (request.Cursor == "") {
		return page, conversationArgument("choose conversation_id or cursor")
	}
	cursor := conversationCursor{Runtime: s.runtimeID, Incarnation: s.incarnation, Conversation: request.ConversationID}
	if request.Cursor != "" {
		if len(request.Cursor) > 4096 {
			return page, &api.Error{Code: "INVALID_CURSOR", Detail: "cursor exceeds size limit"}
		}
		data, err := base64.RawURLEncoding.DecodeString(request.Cursor)
		if len(request.Cursor) > 4096 || err != nil || json.Unmarshal(data, &cursor) != nil || encodeConversationCursor(cursor) != request.Cursor || cursor.Runtime != s.runtimeID || cursor.Incarnation != s.incarnation {
			return page, &api.Error{Code: "INVALID_CURSOR", Detail: "cursor is invalid or belongs to another Runtime"}
		}
	}
	s.store.mu.Lock()
	defer s.store.mu.Unlock()
	m, err := s.selectLocked(cursor.Conversation)
	if err != nil {
		return page, err
	}
	if request.Cursor == "" {
		cursor.Through = m.description.HeadOrder
		cursor.Before = cursor.Through + 1
	} else if cursor.Through > m.description.HeadOrder || cursor.Before == 0 || cursor.Before > cursor.Through+1 {
		return page, &api.Error{Code: "INVALID_CURSOR", Detail: "cursor exceeds allocated order range"}
	}
	page.Conversation, page.ThroughOrder = m.description, cursor.Through
	remainingBytes := api.MaxACPConversationResponseBytes - len(api.Payload(page)) - 4096
	order := cursor.Before - 1
	for order >= m.description.RetainedFromOrder && order > 0 && len(page.Entries) < limit {
		entry := m.entries[order-m.description.RetainedFromOrder]
		if entry.bytes+1 > remainingBytes {
			break
		}
		page.Entries = append(page.Entries, entry.value)
		remainingBytes -= entry.bytes + 1
		order--
	}
	for left, right := 0, len(page.Entries)-1; left < right; left, right = left+1, right-1 {
		page.Entries[left], page.Entries[right] = page.Entries[right], page.Entries[left]
	}
	page.HasMore = order >= m.description.RetainedFromOrder && order > 0
	page.RangeEvicted = order > 0 && order < m.description.RetainedFromOrder && len(page.Entries) < limit
	if page.HasMore {
		cursor.Before = order + 1
		page.NextCursor = encodeConversationCursor(cursor)
	}
	return page, nil
}

func conversationEntryOrder(id string) (uint64, error) {
	if !strings.HasPrefix(id, "e-") || len(id) > 22 {
		return 0, conversationArgument("invalid entry_id")
	}
	order, err := strconv.ParseUint(strings.TrimPrefix(id, "e-"), 10, 64)
	if err != nil || order == 0 || "e-"+strconv.FormatUint(order, 10) != id {
		return 0, conversationArgument("invalid entry_id")
	}
	return order, nil
}

func (s *conversationSlot) get(request api.ACPConversationGet) (api.ACPConversationEntries, error) {
	result := api.ACPConversationEntries{Entries: []api.ACPEntry{}, Missing: []api.ACPMissingEntry{}, UnprocessedEntryIDs: []string{}}
	if request.ConversationID == "" || len(request.EntryIDs) < 1 || len(request.EntryIDs) > api.MaxACPConversationLimit {
		return result, conversationArgument("conversation_id and 1..200 entry_ids are required")
	}
	orders := make([]uint64, 0, len(request.EntryIDs))
	ids := make([]string, 0, len(request.EntryIDs))
	seen := map[string]bool{}
	for _, id := range request.EntryIDs {
		order, err := conversationEntryOrder(id)
		if err != nil {
			return result, err
		}
		if !seen[id] {
			seen[id] = true
			ids = append(ids, id)
			orders = append(orders, order)
		}
	}
	s.store.mu.Lock()
	defer s.store.mu.Unlock()
	m, err := s.selectLocked(request.ConversationID)
	if err != nil {
		return result, err
	}
	result.Conversation = m.description
	remainingBytes := api.MaxACPConversationResponseBytes - len(api.Payload(result)) - 16384
	for i, order := range orders {
		switch {
		case order > m.description.HeadOrder:
			result.Missing = append(result.Missing, api.ACPMissingEntry{ID: ids[i], Reason: "not_found"})
		case order < m.description.RetainedFromOrder:
			result.Missing = append(result.Missing, api.ACPMissingEntry{ID: ids[i], Reason: "evicted"})
		default:
			entry := m.entries[order-m.description.RetainedFromOrder]
			if entry.bytes+1 > remainingBytes {
				result.UnprocessedEntryIDs = append(result.UnprocessedEntryIDs, ids[i])
				continue
			}
			result.Entries = append(result.Entries, entry.value)
			remainingBytes -= entry.bytes + 1
		}
	}
	return result, nil
}
