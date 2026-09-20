package fabricd

import (
	"encoding/json"
	"strconv"
	"sync"

	"github.com/aiomni/dune/internal/wire"
	"github.com/aiomni/dune/pkg/api"
)

// The store owns all model data and capacity accounting. Controller locks may
// enter this lock; the store never calls back into a controller. Published entry
// values are immutable, so a bounded read can encode them after releasing it.
type conversationStore struct {
	mu            sync.Mutex
	slots         map[*conversationSlot]struct{}
	bytes         int
	clock         uint64
	maxBytes      int
	maxModelBytes int
	maxEntries    int
	usage         api.ACPConversationUsage
	wake          chan struct{}
}

type conversationSlot struct {
	store       *conversationStore
	runtimeID   string
	incarnation string
	model       *conversationModel
	pending     *api.ACPConversationChanged
	publish     func(api.ACPConversationChanged)
}

type conversationModel struct {
	description    api.ACPConversation
	entries        []conversationEntry
	index          map[string]uint64
	bytes          int
	updated        uint64
	entryBytes     int
	omittedUpdates uint64
	changed        map[string]bool
	invalidatesAll bool
}

type conversationEntry struct {
	value api.ACPEntry
	key   string
	bytes int
}

func newConversationStore() *conversationStore {
	return &conversationStore{slots: map[*conversationSlot]struct{}{}, wake: make(chan struct{}, 1), maxBytes: api.MaxACPConversationsBytes,
		maxModelBytes: api.MaxACPConversationBytes, maxEntries: api.MaxACPConversationEntries}
}

func (s *conversationStore) register(runtimeID, incarnation string, publish func(api.ACPConversationChanged)) *conversationSlot {
	s.mu.Lock()
	defer s.mu.Unlock()
	slot := &conversationSlot{store: s, runtimeID: runtimeID, incarnation: incarnation, publish: publish}
	s.slots[slot] = struct{}{}
	return slot
}

func (s *conversationSlot) remove() {
	s.store.mu.Lock()
	defer s.store.mu.Unlock()
	if s.model != nil {
		s.store.bytes -= s.model.bytes
		s.model = nil
	}
	s.pending = nil
	delete(s.store.slots, s)
}

func (s *conversationSlot) begin(action api.ACPAction) {
	s.store.mu.Lock()
	defer s.store.mu.Unlock()
	if s.model != nil {
		s.store.bytes -= s.model.bytes
	}
	phase, coverage := "creating", "not_applicable"
	if action.Action == "load" {
		phase, coverage = "loading", "unknown"
	}
	s.model = &conversationModel{description: api.ACPConversation{ID: wire.ID(), Origin: action.Action,
		Phase: phase, OpenOutcome: "pending", RequestedSessionID: action.SessionID, RequestedCwd: action.Cwd,
		RetainedFromOrder: 1, NativeHistoryCoverage: coverage}, index: map[string]uint64{}}
	s.commitLocked()
}

func (s *conversationSlot) describe() *api.ACPConversation {
	s.store.mu.Lock()
	defer s.store.mu.Unlock()
	if s.model == nil {
		return nil
	}
	description := s.model.description
	return &description
}

func (s *conversationSlot) opened(sessionID, cwd, outcome string, failure *api.ACPFailure) {
	s.mutate(func(m *conversationModel) {
		if m.description.OpenOutcome != "pending" {
			return
		}
		m.description.OpenOutcome, m.description.OpenError = outcome, boundedConversationFailure(failure)
		m.description.Phase = outcome
		if outcome == "succeeded" {
			m.description.Phase = "ready"
			m.description.SessionID, m.description.Cwd = sessionID, cwd
		}
	})
}

func (s *conversationSlot) exited() { s.exitedWithCode(nil) }

func (s *conversationSlot) exitedWithCode(code *int) {
	s.mutate(func(m *conversationModel) {
		if m.description.OpenOutcome == "pending" {
			m.description.OpenOutcome = "unknown"
			m.description.OpenError = &api.ACPFailure{Code: "RESULT_UNKNOWN", Detail: "Agent exited before a matching open result"}
		}
		m.description.Phase = "exited"
		if code != nil {
			value := *code
			m.description.ExitCode = &value
		}
	})
}

func (s *conversationSlot) mutate(change func(*conversationModel)) {
	s.store.mu.Lock()
	defer s.store.mu.Unlock()
	if s.model == nil {
		return
	}
	previousOmissions := s.model.omittedUpdates
	change(s.model)
	s.store.usage.OmittedUpdates += s.model.omittedUpdates - previousOmissions
	s.commitLocked()
}

func (s *conversationSlot) commitLocked() {
	m := s.model
	previous := m.description.Revision
	m.description.Revision++
	s.store.clock++
	m.updated = s.store.clock
	s.store.bytes -= m.bytes
	m.measure()
	for len(m.entries) > 0 && (m.bytes > s.store.maxModelBytes || len(m.entries) > s.store.maxEntries) {
		m.evictFirst()
		s.store.usage.Evictions++
		m.measure()
	}
	s.store.bytes += m.bytes
	for s.store.bytes > s.store.maxBytes {
		var victim *conversationModel
		var victimSlot *conversationSlot
		for slot := range s.store.slots {
			candidate := slot.model
			if candidate == nil || len(candidate.entries) == 0 {
				continue
			}
			if victim == nil || (candidate.description.Phase == "exited" && victim.description.Phase != "exited") ||
				((candidate.description.Phase == "exited") == (victim.description.Phase == "exited") && candidate.updated < victim.updated) {
				victim, victimSlot = candidate, slot
			}
		}
		if victim == nil {
			break
		} // descriptions are separately bounded by Runtime admission.
		s.store.bytes -= victim.bytes
		victim.evictFirst()
		s.store.usage.Evictions++
		if victim != m {
			prior := victim.description.Revision
			victim.description.Revision++
			victimSlot.notifyLocked(prior)
		}
		victim.measure()
		s.store.bytes += victim.bytes
	}
	s.notifyLocked(previous)
}

func (m *conversationModel) measure() {
	m.description.RetainedEntryCount = len(m.entries)
	m.description.RetainedFromOrder = m.description.HeadOrder + 1
	if len(m.entries) > 0 {
		m.description.RetainedFromOrder = m.entries[0].value.Order
	}
	m.bytes = len(api.Payload(m.description)) + m.entryBytes
}

func (m *conversationModel) evictFirst() {
	entry := m.entries[0]
	m.entryBytes -= entry.bytes
	if entry.key != "" && m.index[entry.key] == entry.value.Order {
		delete(m.index, entry.key)
	}
	m.entries[0] = conversationEntry{}
	m.entries = m.entries[1:]
	m.description.PrefixEvicted = true
	m.invalidatesAll = true
}

func (m *conversationModel) put(entry api.ACPEntry, key string) {
	entry.Revision = m.description.Revision + 1
	if entry.Order == 0 {
		m.description.HeadOrder++
		entry.Order = m.description.HeadOrder
		entry.ID = "e-" + strconv.FormatUint(entry.Order, 10)
	}
	if !m.invalidatesAll {
		if m.changed == nil {
			m.changed = map[string]bool{}
		}
		m.changed[entry.ID] = true
		if len(m.changed) > api.MaxACPConversationLimit {
			m.changed = nil
			m.invalidatesAll = true
		}
	}
	data := api.Payload(entry)
	tooManyFields := entry.Tool != nil && len(entry.Tool.Fields) > 128
	tooManyBlocks := entry.Message != nil && (len(entry.Message.Content) > 128 || len(entry.Message.Tail) > 128)
	if len(data) > api.MaxACPEntryBytes || tooManyFields || tooManyBlocks {
		entry = boundedConversationEntry(entry)
		data = api.Payload(entry)
	}
	if entry.ContentOmitted {
		m.description.ContentOmitted = true
		m.omittedUpdates++
	}
	if entry.ContextIncomplete {
		m.description.ContextIncomplete = true
	}
	retained := conversationEntry{value: entry, key: key, bytes: len(data)}
	if len(m.entries) > 0 && entry.Order <= m.entries[len(m.entries)-1].value.Order {
		m.entryBytes -= m.entries[entry.Order-m.entries[0].value.Order].bytes
		m.entries[entry.Order-m.entries[0].value.Order] = retained
	} else {
		m.entries = append(m.entries, retained)
	}
	m.entryBytes += retained.bytes
	if key != "" {
		m.index[key] = entry.Order
	}
}

func (m *conversationModel) find(key string) *api.ACPEntry {
	order, ok := m.index[key]
	if !ok || len(m.entries) == 0 || order < m.entries[0].value.Order {
		return nil
	}
	// Every mutation starts from a private copy, never the graph returned by a read.
	var entry api.ACPEntry
	_ = json.Unmarshal(api.Payload(m.entries[order-m.entries[0].value.Order].value), &entry)
	return &entry
}
