package agents

import (
	"testing"

	"github.com/aiomni/dune/pkg/api"
	"github.com/aiomni/dune/pkg/runner"
	"github.com/aiomni/dune/pkg/workbench"
)

func directoryCacheAgent(revision uint64, title string) Agent {
	binding := runner.Binding{RunnerID: "runner", FabricID: "fabric", MachineID: "machine", Revision: 1}
	conversation := "conversation"
	runtime := api.Runtime{ID: "runtime", Incarnation: "original", Generation: 1, Adapter: "acp", ConversationID: conversation,
		SessionMetadata: &api.SessionMetadata{Revision: revision, ConversationID: &conversation, Title: &title}}
	return Agent{Target: workbench.AgentTarget{Binding: binding, Runtime: workbench.RuntimeRef{ID: runtime.ID, Incarnation: runtime.Incarnation, Generation: runtime.Generation, Adapter: runtime.Adapter}},
		Runtime: runtime, Runner: runner.Runner{ID: binding.RunnerID, Binding: &binding}}
}

func TestDirectoryCacheFencesBufferedPagesAndEventsAfterInvalidation(t *testing.T) {
	for _, reason := range []string{"removed", "replaced", "revoked"} {
		t.Run(reason, func(t *testing.T) {
			var cache DirectoryCache
			batch := cache.Begin("original-subscription")
			old := directoryCacheAgent(1<<53, "old")
			if !cache.MergePage(batch, DirectoryPage{Items: []Agent{old}}) {
				t.Fatal("initial discovery rejected")
			}
			cache.Apply(DirectoryEvent{SubscriptionID: "original-subscription", Kind: DirectoryInvalidated, Code: reason})
			late := directoryCacheAgent(1<<53+100, "late but higher")
			page := DirectoryPage{Items: []Agent{late}}
			if cache.MergePage(batch, page) || cache.Apply(DirectoryEvent{SubscriptionID: "original-subscription", Kind: DirectoryMember, Agent: &late}) || len(cache.Snapshot()) != 0 {
				t.Fatal("invalidated member revived")
			}
			current := cache.Begin("new-subscription")
			if cache.MergePage(batch, page) || cache.Apply(DirectoryEvent{SubscriptionID: "original-subscription", Kind: DirectoryMember, Agent: &late}) {
				t.Fatal("reconnecting relabeled old data")
			}
			if cache.Apply(DirectoryEvent{SubscriptionID: "new-subscription", Kind: DirectoryMetadata, Agent: &late}) {
				t.Fatal("title alone established membership")
			}
			confirmed := directoryCacheAgent(1, "confirmed replacement")
			confirmed.Runtime.Incarnation, confirmed.Target.Runtime.Incarnation = "replacement", "replacement"
			if !cache.MergePage(current, DirectoryPage{Items: []Agent{confirmed}}) || len(cache.Snapshot()) != 1 || cache.Snapshot()[0].Runtime.Incarnation != "replacement" {
				t.Fatal("newly confirmed member was not accepted")
			}
		})
	}
}

func TestDirectoryCacheMergesSingleRevisionAfterMembership(t *testing.T) {
	var cache DirectoryCache
	batch := cache.Begin("subscription")
	newer := directoryCacheAgent(1<<53+2, "new")
	newer.Ref = "current-native-reference"
	if !cache.Apply(DirectoryEvent{SubscriptionID: "subscription", Kind: DirectoryMember, Agent: &newer}) {
		t.Fatal("member event rejected")
	}
	older := directoryCacheAgent(1<<53+1, "old")
	older.Ref = "old-native-reference"
	cache.MergePage(batch, DirectoryPage{Items: []Agent{older}})
	absent := older
	absent.Runtime.SessionMetadata = nil
	cache.MergePage(batch, DirectoryPage{Items: []Agent{absent}, Complete: false})
	if value := cache.Snapshot()[0].Runtime.SessionMetadata; value.Revision != newer.Runtime.SessionMetadata.Revision || *value.Title != "new" {
		t.Fatal("late or unavailable discovery overwrote a newer title", value)
	}
	if cache.Snapshot()[0].Ref != newer.Ref {
		t.Fatal("old discovery replaced the current native action reference")
	}
	cleared := directoryCacheAgent(1<<53+3, "")
	cleared.Runtime.SessionMetadata.Title = nil
	cache.Apply(DirectoryEvent{SubscriptionID: "subscription", Kind: DirectoryMetadata, Agent: &cleared})
	if cache.Snapshot()[0].Runtime.SessionMetadata.Title != nil {
		t.Fatal("explicit clear was lost")
	}
	// Neither caller input nor a returned snapshot may mutate retained data.
	cleared.Runtime.SessionMetadata.Revision = 1
	copy := cache.Snapshot()
	copy[0].Runtime.SessionMetadata.Revision = 2
	if cache.Snapshot()[0].Runtime.SessionMetadata.Revision != 1<<53+3 {
		t.Fatal("mutable metadata escaped the SDK cache")
	}
}
