package fabricd

import (
	"context"
	"encoding/json"
	pb "github.com/aiomni/dune/proto/dune/dtp/v1"
	"sort"

	"github.com/aiomni/dune/pkg/api"
)

// Each slot keeps at most one pending interval. The publisher takes bounded
// immutable values under the store lock and calls transport code outside it.
func (s *conversationStore) run(ctx context.Context) {
	for {
		select {
		case <-ctx.Done():
			return
		case <-s.wake:
		}
		type delivery struct {
			publish func(api.ACPConversationChanged)
			change  api.ACPConversationChanged
		}
		s.mu.Lock()
		deliveries := make([]delivery, 0, len(s.slots))
		for slot := range s.slots {
			if slot.pending != nil && slot.publish != nil {
				deliveries = append(deliveries, delivery{slot.publish, *slot.pending})
				slot.pending = nil
			}
		}
		s.mu.Unlock()
		for _, item := range deliveries {
			item.publish(item.change)
		}
	}
}

func (s *conversationSlot) notifyLocked(previous uint64) {
	m := s.model
	change := api.ACPConversationChanged{ConversationID: m.description.ID, PreviousRevision: previous, Revision: m.description.Revision, InvalidatesAll: m.invalidatesAll || previous == 0}
	change.SessionMetadata = s.metadata
	if !change.InvalidatesAll {
		for id := range m.changed {
			change.ChangedEntryIDs = append(change.ChangedEntryIDs, id)
		}
		sort.Strings(change.ChangedEntryIDs)
	}
	m.changed, m.invalidatesAll = nil, false
	if s.pending != nil {
		change = mergeConversationChanges(*s.pending, change)
		s.store.usage.NotificationMerges++
	}
	s.pending = &change
	select {
	case s.store.wake <- struct{}{}:
	default:
	}
}

func mergeConversationChanges(previous, next api.ACPConversationChanged) api.ACPConversationChanged {
	if next.SessionMetadata.Revision < previous.SessionMetadata.Revision {
		return previous
	}
	if previous.ConversationID != next.ConversationID {
		next.PreviousRevision, next.InvalidatesAll, next.ChangedEntryIDs = 0, true, nil
		return next
	}
	if next.Revision <= previous.Revision {
		return previous
	}
	next.InvalidatesAll = next.InvalidatesAll || previous.InvalidatesAll || next.PreviousRevision > previous.Revision
	next.PreviousRevision = previous.PreviousRevision
	if !next.InvalidatesAll {
		ids := map[string]bool{}
		for _, id := range previous.ChangedEntryIDs {
			ids[id] = true
		}
		for _, id := range next.ChangedEntryIDs {
			ids[id] = true
		}
		next.ChangedEntryIDs = nil
		for id := range ids {
			next.ChangedEntryIDs = append(next.ChangedEntryIDs, id)
		}
		sort.Strings(next.ChangedEntryIDs)
		next.InvalidatesAll = len(ids) > api.MaxACPConversationLimit || len(api.Payload(next)) > api.MaxACPConversationNotificationBytes
	}
	if next.InvalidatesAll {
		next.ChangedEntryIDs = nil
	}
	return next
}

func (s *subscription) enqueueConversationChange(message *pb.Message) bool {
	var next api.ACPConversationChanged
	if json.Unmarshal(message.Payload, &next) != nil {
		return false
	}
	s.queueMu.Lock()
	merged := s.pendingChange != nil
	if merged {
		next = mergeConversationChanges(*s.pendingChange, next)
	}
	s.pendingChange = &next
	s.queueMu.Unlock()
	select {
	case s.changed <- struct{}{}:
	default:
	}
	return merged
}

func (s *subscription) takeConversationChange() *pb.Message {
	s.queueMu.Lock()
	next := s.pendingChange
	s.pendingChange = nil
	s.queueMu.Unlock()
	if next == nil {
		return nil
	}
	return &pb.Message{Kind: "acp_conversation_changed", Payload: api.Payload(next)}
}
