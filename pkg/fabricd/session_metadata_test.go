package fabricd

import (
	"encoding/json"
	"fmt"
	"strings"
	"sync"
	"testing"

	"github.com/aiomni/dune/pkg/api"
)

func TestSessionMetadataMergesBothProtocols(t *testing.T) {
	for _, version := range []int{1, 2} {
		t.Run(fmt.Sprint(version), func(t *testing.T) {
			slot := conversationFixture(t)
			update := slot.update
			if version == 2 {
				update = slot.updateV2
			}
			apply := func(raw string) {
				t.Helper()
				fields := map[string]json.RawMessage{"sessionUpdate": api.Payload("session_info_update")}
				if raw != "" {
					fields["title"] = json.RawMessage(raw)
				}
				update(fields, "")
			}
			for _, step := range []struct {
				name, raw, title string
				revision         uint64
			}{
				{"set", `"  修复登录失败  "`, "修复登录失败", 2},
				{"absent", "", "修复登录失败", 2},
				{"duplicate", `"修复登录失败"`, "修复登录失败", 2},
				{"type", `123`, "修复登录失败", 2},
				{"control", `"invalid\u0000title"`, "修复登录失败", 2},
				{"multiline", `"two\nlines"`, "修复登录失败", 2},
				{"long", string(api.Payload(strings.Repeat("题", 342))), "修复登录失败", 2},
				{"long whitespace", string(api.Payload(strings.Repeat(" ", 1025))), "修复登录失败", 2},
				{"null", `null`, "", 3},
				{"already clear", `"\t \n"`, "", 3},
				{"replace", `"B"`, "B", 4},
				{"empty", `""`, "", 5},
			} {
				t.Run(step.name, func(t *testing.T) {
					before := slot.describe()
					beforeJSON := string(api.Payload(before))
					apply(step.raw)
					model, metadata := slot.snapshot()
					if metadata.Revision != step.revision || metadata.ConversationID == nil || *metadata.ConversationID != model.ID || !equalTitle(metadata.Title, model.SessionMetadata.Title) {
						t.Fatalf("inconsistent snapshot: %s", api.Payload(model))
					}
					if step.title == "" && metadata.Title != nil || step.title != "" && (metadata.Title == nil || *metadata.Title != step.title) {
						t.Fatalf("unexpected metadata: %s", api.Payload(metadata))
					}
					if string(api.Payload(before)) != beforeJSON {
						t.Fatal("previously published snapshot mutated")
					}
				})
			}
			if got := slot.store.statistics().MergeFailures["invalid_session_title"]; got != 5 {
				t.Fatalf("invalid updates not diagnosed: %d", got)
			}
		})
	}
}

func TestSessionMetadataOutlivesContentAndConversationRevisions(t *testing.T) {
	slot := newConversationStore().register("runtime", "incarnation", nil)
	model, metadata := slot.snapshot()
	if model != nil || string(api.Payload(metadata)) != `{"revision":"0","conversation_id":null,"title":null}` {
		t.Fatal("initial snapshot is not explicit", model, metadata)
	}
	slot.metadata.Revision = 1 << 53
	for _, action := range []string{"new", "load", "resume"} {
		for _, replay := range []bool{false, true} {
			for _, outcome := range []string{"succeeded", "failed", "unknown"} {
				for _, withTitle := range []bool{false, true} {
					prior := slot.metadata
					slot.begin(api.ACPAction{Action: action, SessionID: "same-native-id", Replay: replay})
					model, metadata = slot.snapshot()
					if metadata.Revision <= prior.Revision || metadata.Title != nil || metadata.ConversationID == prior.ConversationID {
						t.Fatal("new generation inherited metadata", metadata)
					}
					if withTitle {
						conversationUpdate(t, slot, "", `{"sessionUpdate":"session_info_update","title":"A"}`)
					}
					slot.opened("same-native-id", "/work", outcome, nil)
					slot.store.maxEntries = 1
					for i := 0; i < 3; i++ {
						conversationUpdate(t, slot, "", `{"sessionUpdate":"vendor_event","data":"body"}`)
					}
					conversationUpdate(t, slot, "", `{"sessionUpdate":"session_info_update","updatedAt":"later"}`)
					slot.exited()
					current := slot.describe()
					expectedRevision := metadata.Revision
					if withTitle {
						expectedRevision++
					}
					if (withTitle && (current.SessionMetadata.Title == nil || *current.SessionMetadata.Title != "A")) || (!withTitle && current.SessionMetadata.Title != nil) || current.SessionMetadata.Revision != expectedRevision || !current.PrefixEvicted {
						t.Fatal("metadata lost with content/open outcome", current)
					}
					if string(api.Payload(slot.pending.SessionMetadata)) != string(api.Payload(current.SessionMetadata)) {
						t.Fatal("merged notification lost final metadata", slot.pending)
					}
				}
			}
		}
	}
}

func TestSessionMetadataAtomicConcurrentReads(t *testing.T) {
	slot := conversationFixture(t)
	var group sync.WaitGroup
	for range 4 {
		group.Go(func() {
			seen := map[uint64]string{}
			for range 250 {
				model, metadata := slot.snapshot()
				encoded := string(api.Payload(metadata))
				if prior, ok := seen[metadata.Revision]; ok && prior != encoded {
					t.Error("same revision changed contents")
				}
				seen[metadata.Revision] = encoded
				if *metadata.ConversationID != model.ID || encoded != string(api.Payload(model.SessionMetadata)) {
					t.Error("mixed model and metadata generations")
				}
			}
		})
	}
	for range 250 {
		slot.begin(api.ACPAction{Action: "load", SessionID: "same-native-id"})
		conversationUpdate(t, slot, "", `{"sessionUpdate":"session_info_update","title":"A"}`)
	}
	group.Wait()
}
