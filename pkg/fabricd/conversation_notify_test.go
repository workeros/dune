package fabricd

import (
	"context"
	"encoding/json"
	"fmt"
	"testing"
	"time"

	"github.com/aiomni/dune/pkg/api"
	pb "github.com/aiomni/dune/proto/dune/dtp/v1"
)

func TestConversationNotificationUnionAndOverflow(t *testing.T) {
	a := api.ACPConversationChanged{ConversationID: "c1", PreviousRevision: 1, Revision: 2, ChangedEntryIDs: []string{"e-1"}}
	b := api.ACPConversationChanged{ConversationID: "c1", PreviousRevision: 2, Revision: 5, ChangedEntryIDs: []string{"e-2"}}
	merged := mergeConversationChanges(a, b)
	if merged.PreviousRevision != 1 || merged.Revision != 5 || merged.InvalidatesAll || len(merged.ChangedEntryIDs) != 2 {
		t.Fatalf("lost earlier invalidation: %+v", merged)
	}
	for i := 3; i <= 201; i++ {
		merged = mergeConversationChanges(merged, api.ACPConversationChanged{ConversationID: "c1", PreviousRevision: merged.Revision, Revision: merged.Revision + 1, ChangedEntryIDs: []string{fmt.Sprintf("e-%d", i)}})
	}
	if !merged.InvalidatesAll || len(merged.ChangedEntryIDs) != 0 || len(api.Payload(merged)) > api.MaxACPConversationNotificationBytes {
		t.Fatal("overflow silently lost IDs or exceeded budget")
	}
	gap := mergeConversationChanges(a, api.ACPConversationChanged{ConversationID: "c1", PreviousRevision: 4, Revision: 5})
	if !gap.InvalidatesAll {
		t.Fatal("uncovered interval advertised as complete IDs")
	}
	changed := mergeConversationChanges(merged, api.ACPConversationChanged{ConversationID: "c2", PreviousRevision: 4, Revision: 5})
	if changed.ConversationID != "c2" || changed.PreviousRevision != 0 || !changed.InvalidatesAll {
		t.Fatal("new generation inherited old interval")
	}
}

func TestConversationNotificationsFollowCommitsAndGlobalEviction(t *testing.T) {
	store := newConversationStore()
	delivered := make(chan api.ACPConversationChanged, 10)
	first := store.register("one", "inc", func(change api.ACPConversationChanged) { delivered <- change })
	second := store.register("two", "inc", nil)
	first.begin(api.ACPAction{Action: "new", Cwd: "/tmp"})
	first.update(map[string]json.RawMessage{"sessionUpdate": api.Payload("vendor_event"), "text": api.Payload("retained")}, "")
	initial := first.describe()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go store.run(ctx)
	select {
	case change := <-delivered:
		if change.Revision != initial.Revision || change.PreviousRevision != 0 {
			t.Fatal("notification preceded final commit", change)
		}
	case <-time.After(time.Second):
		t.Fatal("no notification")
	}
	first.exited()
	second.begin(api.ACPAction{Action: "new", Cwd: "/tmp"})
	store.mu.Lock()
	store.maxBytes = store.bytes
	store.mu.Unlock()
	second.update(map[string]json.RawMessage{"sessionUpdate": api.Payload("vendor_event"), "text": api.Payload("new data")}, "")
	deadline := time.After(time.Second)
	for {
		select {
		case change := <-delivered:
			current := first.describe()
			if change.Revision == current.Revision && current.PrefixEvicted {
				if !change.InvalidatesAll {
					t.Fatal("global eviction did not invalidate current values")
				}
				return
			}
		case <-deadline:
			t.Fatal("global eviction silently changed another Runtime")
		}
	}
}

func TestConversationSubscriberIsolationAndBoundedPendingInterval(t *testing.T) {
	r := &runtime{subs: map[*subscription]bool{}}
	r.acp = newACPController(r)
	model, err := r.subscribe(false, true)
	if err != nil {
		t.Fatal(err)
	}
	diagnostic, err := r.subscribe(false, false)
	if err != nil {
		t.Fatal(err)
	}
	done := make(chan struct{})
	go func() {
		defer close(done)
		for i := uint64(1); i <= 1000; i++ {
			r.emit(&pb.Message{Kind: "acp_update", Payload: api.Payload(map[string]string{"text": "raw"})})
			change := api.ACPConversationChanged{ConversationID: "c1", PreviousRevision: i - 1, Revision: i, ChangedEntryIDs: []string{fmt.Sprintf("e-%d", i)}}
			r.emit(&pb.Message{Kind: "acp_conversation_changed", Payload: api.Payload(change)})
		}
	}()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("slow subscriber blocked writer")
	}
	select {
	case <-diagnostic.failed:
	default:
		t.Fatal("stalled diagnostics were not bounded")
	}
	select {
	case <-model.failed:
		t.Fatal("raw diagnostics overflowed model subscriber")
	default:
	}
	if len(model.q) != 0 {
		t.Fatal("model-only stream contains diagnostic messages")
	}
	var change api.ACPConversationChanged
	if err := json.Unmarshal(model.takeConversationChange().Payload, &change); err != nil {
		t.Fatal(err)
	}
	if change.PreviousRevision != 0 || change.Revision != 1000 || !change.InvalidatesAll || len(change.ChangedEntryIDs) != 0 {
		t.Fatalf("pending interval lost revisions: %+v", change)
	}
	if usage := r.conversations.statistics(); usage.NotificationMerges == 0 || usage.SlowConsumerClosures != 1 {
		t.Fatal("diagnostic counters missing", usage)
	}
}

func TestConversationStateChangesInvalidateTheWholeModel(t *testing.T) {
	slot := conversationFixture(t)
	for _, change := range []struct {
		name  string
		apply func()
	}{
		{"open", func() {
			slot.begin(api.ACPAction{Action: "load", SessionID: "session-test"})
			slot.store.mu.Lock()
			slot.pending = nil
			slot.store.mu.Unlock()
			slot.opened("session-test", "/work", "succeeded", nil)
		}},
		{"plan", func() {
			conversationUpdate(t, slot, "", `{"sessionUpdate":"plan","entries":[{"content":"next step","status":"pending","priority":"medium"}]}`)
		}},
		{"turn start", func() { slot.startTurn("operation", "question") }},
		{"turn terminal", func() { slot.finishTurn(api.AgentOperation{Ref: "operation", State: "completed"}) }},
		{"exit", func() { slot.exited() }},
	} {
		t.Run(change.name, func(t *testing.T) {
			slot.store.mu.Lock()
			slot.pending = nil
			slot.store.mu.Unlock()
			change.apply()
			slot.store.mu.Lock()
			notice := slot.pending
			slot.store.mu.Unlock()
			if notice == nil || !notice.InvalidatesAll || len(notice.ChangedEntryIDs) != 0 || notice.Revision != slot.describe().Revision {
				t.Fatal("state-only invalidation was indistinguishable from unchanged entries", notice)
			}
		})
	}
}
