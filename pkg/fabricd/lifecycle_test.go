package fabricd

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/aiomni/dune/internal/lifecycle"
)

func TestConversationGapLogOmitsProtocolBodies(t *testing.T) {
	dir := t.TempDir()
	if err := os.Chmod(dir, 0700); err != nil {
		t.Fatal(err)
	}
	log, err := lifecycle.Open(dir, "events.jsonl")
	if err != nil {
		t.Fatal(err)
	}
	defer log.Close()
	slot := conversationFixture(t)
	slot.store.mu.Lock()
	slot.store.maxEntries, slot.events = 1, log
	slot.store.mu.Unlock()
	for range 4 {
		conversationUpdate(t, slot, "", `{"sessionUpdate":"vendor_event","value":"private-tool-body"}`)
	}
	log.Close()
	body, err := os.ReadFile(filepath.Join(dir, "events.jsonl"))
	if err != nil || strings.Count(string(body), `"kind":"conversation_gap"`) != 1 || strings.Contains(string(body), "private-tool-body") {
		t.Fatal(string(body), err)
	}
}
