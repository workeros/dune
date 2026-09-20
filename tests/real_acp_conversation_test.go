package tests

import (
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/aiomni/dune/internal/wire"
	"github.com/aiomni/dune/pkg/api"
)

// The command must be a configured load-capable ACP Agent (or an absolute
// wrapper that supplies its isolated account environment). Ordinary tests never
// start a vendor Agent or copy credentials from a daily-use account.
func TestRealACPConversationLoad(t *testing.T) {
	if os.Getenv("DUNE_REAL_AGENT") != "1" {
		t.Skip("set DUNE_REAL_AGENT=1 with DUNE_REAL_ACP_COMMAND and DUNE_REAL_ACP_WORKDIR")
	}
	var argv []string
	if err := json.Unmarshal([]byte(os.Getenv("DUNE_REAL_ACP_COMMAND")), &argv); err != nil || len(argv) == 0 {
		t.Fatal("DUNE_REAL_ACP_COMMAND must be a JSON argv array for the configured ACP Agent")
	}
	parent := os.Getenv("DUNE_REAL_ACP_WORKDIR")
	if !filepath.IsAbs(parent) {
		t.Fatal("DUNE_REAL_ACP_WORKDIR must be an absolute dedicated acceptance directory")
	}
	command, err := exec.LookPath(argv[0])
	must(t, err)
	argv[0] = command
	work, err := os.MkdirTemp(parent, "dune-history-")
	must(t, err)
	t.Cleanup(func() { _ = os.RemoveAll(work) })
	h := start(t)
	p := profile(work, "acp", argv...)
	p.ManagedACP = true
	runtime, stream, err := h.client.Start(h.ctx, p)
	must(t, err)
	stream.Close()
	defer h.client.Stop(h.ctx, runtime)
	initial := waitManagedACPReady(t, h, runtime)
	if !initial.CanLoad {
		t.Fatal("configured Agent does not advertise session/load")
	}
	var info struct {
		Name    string `json:"name"`
		Version string `json:"version"`
	}
	must(t, json.Unmarshal(initial.Agent, &info))
	if info.Version == "" {
		t.Fatal("Agent initialize response must identify its version for acceptance")
	}
	submit := func(action api.ACPAction) api.AgentOperation {
		t.Helper()
		operation, err := h.client.ACPSubmit(h.ctx, runtime, action)
		must(t, err)
		for !operation.Terminal() {
			operation, err = h.client.WaitAgentOperation(h.ctx, runtime, api.AgentOperationWait{Ref: operation.Ref, TimeoutMS: 30000})
			must(t, err)
		}
		if operation.State != "completed" {
			t.Fatalf("real %s failed: %+v", action.Action, operation)
		}
		return operation
	}
	created := submit(api.ACPAction{Action: "new", Cwd: work})
	if created.NativeSession == nil {
		t.Fatal("new did not confirm native session")
	}
	marker := "DUNE_NATIVE_HISTORY_" + wire.ID()
	submit(api.ACPAction{Action: "prompt", ExpectedConversationID: created.ConversationID, Text: "Reply with exactly this marker and no other text: " + marker + ". Do not call tools or modify files."})
	before, err := h.client.ReadACPConversation(h.ctx, runtime, api.ACPConversationRead{ConversationID: created.ConversationID})
	must(t, err)
	assertNativeHistoryMarker(t, before, marker)
	loaded := submit(api.ACPAction{Action: "load", SessionID: created.NativeSession.ID, Cwd: work})
	if loaded.ConversationID == created.ConversationID {
		t.Fatal("load did not create a fresh conversation")
	}
	after, err := h.client.ReadACPConversation(h.ctx, runtime, api.ACPConversationRead{ConversationID: loaded.ConversationID})
	must(t, err)
	assertNativeHistoryMarker(t, after, marker)
	if after.Conversation.OpenOutcome != "succeeded" || after.Conversation.NativeHistoryCoverage != "unknown" {
		t.Fatal("load outcome or coverage is incorrect")
	}
	t.Logf("native load verified: Agent=%s version=%s cwd=%s entries_before=%d entries_after=%d new_generation=true user_and_agent_marker=true", info.Name, info.Version, work, len(before.Entries), len(after.Entries))
}

func assertNativeHistoryMarker(t *testing.T, page api.ACPConversationPage, marker string) {
	t.Helper()
	roles := map[string]bool{}
	for _, entry := range page.Entries {
		if entry.Message != nil && strings.Contains(string(api.Payload(entry.Message.Content)), marker) {
			roles[entry.Message.Role] = true
		}
	}
	if !roles["user"] || !roles["agent"] {
		t.Fatal("native transcript did not retain both submitted marker and Agent response")
	}
}
