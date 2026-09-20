package fabricd

import (
	"time"

	"github.com/aiomni/dune/pkg/api"
)

func (s *conversationStore) statistics() *api.ACPConversationUsage {
	s.mu.Lock()
	defer s.mu.Unlock()
	usage := s.usage
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
