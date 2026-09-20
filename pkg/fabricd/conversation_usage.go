package fabricd

import (
	"time"

	"github.com/aiomni/dune/pkg/api"
)

func (s *conversationStore) statistics() *api.ACPConversationUsage {
	s.mu.Lock()
	defer s.mu.Unlock()
	usage := s.usage
	usage.MergeFailures = make(map[string]uint64, len(s.usage.MergeFailures))
	for reason, count := range s.usage.MergeFailures {
		usage.MergeFailures[reason] = count
	}
	usage.EncodedBytes = s.bytes
	for slot := range s.slots {
		if slot.model == nil {
			continue
		}
		usage.Models++
		usage.Entries += len(slot.model.entries)
		if slot.model.description.Origin == "load" && slot.model.description.OpenOutcome == "pending" {
			usage.InFlightLoads++
		}
	}
	return &usage
}

func (s *conversationStore) recordRead(bytes int, elapsed time.Duration) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.usage.ReadCount++
	s.usage.ReadBytes += uint64(bytes)
	s.usage.ReadNanoseconds += uint64(max(0, elapsed))
}

// Call only with fixed reason names, never protocol IDs or message text.
func (s *conversationStore) mergeFailure(reason string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.usage.MergeFailures == nil {
		s.usage.MergeFailures = map[string]uint64{}
	}
	s.usage.MergeFailures[reason]++
}
