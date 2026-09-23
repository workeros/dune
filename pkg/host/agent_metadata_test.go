package host

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/aiomni/dune/pkg/agents"
	"github.com/aiomni/dune/pkg/api"
	"github.com/aiomni/dune/pkg/runner"
)

func TestAgentDirectoryDiscoversSessionMetadataWithoutACPReads(t *testing.T) {
	f := openExecutorFixture(t)
	journal := filepath.Join(f.workspace, "rpc.log")
	_, connection := directoryACPWithEnvironment(t, f, map[string]string{
		"DUNE_HOST_FAKE_ACP_TITLE": "  修复登录失败  ", "DUNE_HOST_FAKE_ACP_METHODS": journal,
	})
	directory := f.app.AgentDirectory()
	var found agents.Agent
	for deadline := time.Now().Add(5 * time.Second); ; time.Sleep(10 * time.Millisecond) {
		page, err := directory.List(t.Context(), f.agentScope(), runner.Query{})
		if err != nil || len(page.Items) != 1 {
			t.Fatal("discovery failed", page, err)
		}
		found = page.Items[0]
		if metadata := found.Runtime.SessionMetadata; metadata != nil && metadata.Title != nil {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("title absent from directory", found)
		}
	}
	before, err := os.ReadFile(journal)
	if err != nil {
		t.Fatal(err)
	}
	current, err := directory.Get(t.Context(), f.agentScope(), found.Ref)
	if err != nil {
		t.Fatal(err)
	}
	if current.Runtime.SessionMetadata == nil || *current.Runtime.SessionMetadata.Title != "修复登录失败" || string(api.Payload(current.Runtime.SessionMetadata)) != string(api.Payload(found.Runtime.SessionMetadata)) {
		t.Fatal("List/Get disagree on metadata", found, current)
	}
	// Test-only inspection confirms that the trailing partial update replaced
	// the raw event, while discovery retained the independently merged title.
	state, err := connection.ACPState(t.Context(), current.Runtime)
	if err != nil || state.Conversation == nil || string(state.Conversation.State["session_info_update"]) != `{"sessionUpdate":"session_info_update","updatedAt":"2026-09-23T00:00:00Z"}` {
		t.Fatal("fixture did not deliver the trailing partial update", state, err)
	}
	after, err := os.ReadFile(journal)
	if err != nil || string(before) != string(after) {
		t.Fatal("discovery/read dispatched native RPCs", string(after), err)
	}
}
