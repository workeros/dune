package fabricd

import (
	"bytes"
	"encoding/base64"
	"fmt"
	"testing"

	"github.com/aiomni/dune/pkg/api"
)

func retainedTerminal(t *testing.T, a *acpController, id string) api.ACPEntry {
	t.Helper()
	result, err := a.conversation.get(api.ACPConversationGet{ConversationID: a.snapshot().Conversation.ID, TerminalID: id})
	if err != nil || len(result.Entries) != 1 || result.Entries[0].Terminal == nil {
		t.Fatalf("terminal lookup: %+v %v", result, err)
	}
	return result.Entries[0]
}

func terminalChunk(a *acpController, data []byte) {
	emitV2(a, fmt.Sprintf(`{"sessionUpdate":"terminal_output_chunk","terminalId":"term","data":%q}`, base64.StdEncoding.EncodeToString(data)))
}

func TestACPV2TerminalByteOrderingSnapshotsAndExit(t *testing.T) {
	a, _ := v2QueueFixture(t)
	terminalChunk(a, []byte{0xc3})
	first := retainedTerminal(t, a, "term")
	terminalChunk(a, []byte{0xa9, 0x1b, '['})
	terminalChunk(a, []byte("31mred"))
	entry := retainedTerminal(t, a, "term")
	if entry.ID != first.ID || entry.Terminal.Generation != first.Terminal.Generation || !bytes.Equal(entry.Terminal.Output, []byte("é\x1b[31mred")) {
		t.Fatal("chunk decoding lost split UTF-8 or ANSI", entry.Terminal)
	}
	emitV2(a, `{"sessionUpdate":"terminal_update","terminalId":"term","command":"go test","cwd":"/tmp","exitStatus":{"exitCode":0}}`)
	if got := retainedTerminal(t, a, "term"); !bytes.Equal(got.Terminal.Output, entry.Terminal.Output) {
		t.Fatal("metadata patch replaced output")
	}
	emitV2(a, `{"sessionUpdate":"terminal_update","terminalId":"term","command":null,"output":{"data":"bmV3"}}`)
	entry = retainedTerminal(t, a, "term")
	if string(entry.Terminal.Output) != "new" || entry.Terminal.Generation == first.Terminal.Generation || string(entry.Terminal.Fields["command"]) != "null" || string(entry.Terminal.Fields["cwd"]) != `"/tmp"` {
		t.Fatal("snapshot/patch semantics lost", entry.Terminal)
	}
	theSnapshot := entry.Terminal.Generation
	terminalChunk(a, []byte(" tail"))
	if got := retainedTerminal(t, a, "term"); string(got.Terminal.Output) != "new tail" || got.Terminal.Generation != theSnapshot {
		t.Fatal(got.Terminal)
	}
	emitV2(a, `{"sessionUpdate":"terminal_update","terminalId":"term","output":null,"exitStatus":null}`)
	entry = retainedTerminal(t, a, "term")
	if entry.Terminal.OutputKnown || len(entry.Terminal.Output) != 0 || entry.Terminal.Generation == theSnapshot {
		t.Fatal("null did not reset output")
	}
	emitV2(a, `{"sessionUpdate":"tool_call_update","toolCallId":"tool","status":"completed","content":[{"type":"terminal","terminalId":"term"}]}`)
	if got := retainedTerminal(t, a, "term"); string(got.Terminal.Fields["exitStatus"]) != "null" {
		t.Fatal("tool completion invented terminal exit")
	}
}

func TestACPV2TerminalRetentionGapsAndScopedLookup(t *testing.T) {
	a, _ := v2QueueFixture(t)
	terminalChunk(a, bytes.Repeat([]byte("x"), maxTerminalOutputBytes+19))
	entry := retainedTerminal(t, a, "term")
	if !entry.ContentOmitted || entry.Terminal.OmittedBytes != 19 || len(entry.Terminal.Output) != maxTerminalOutputBytes {
		t.Fatal("terminal retention is unbounded", entry.Terminal.OmittedBytes)
	}
	generation := entry.Terminal.Generation
	terminalChunk(a, []byte("after"))
	entry = retainedTerminal(t, a, "term")
	if entry.Terminal.Generation == generation || entry.Terminal.OmittedBytes != 24 || !bytes.HasSuffix(entry.Terminal.Output, []byte("after")) {
		t.Fatal("retention gap lacks a new parser generation")
	}
	emitV2(a, `{"sessionUpdate":"terminal_output_chunk","terminalId":"term","data":"invalid!"}`)
	broken := retainedTerminal(t, a, "term")
	if broken.Terminal.OutputKnown || len(broken.Terminal.Output) != 0 {
		t.Fatal("invalid chunk was spliced into prior bytes")
	}
	terminalChunk(a, []byte("later"))
	if got := retainedTerminal(t, a, "term"); string(got.Terminal.Output) != "later" || !got.ContentOmitted {
		t.Fatal("gap was hidden")
	}
	result, err := a.conversation.get(api.ACPConversationGet{ConversationID: a.snapshot().Conversation.ID, TerminalID: "missing"})
	if err != nil || len(result.Entries) != 0 {
		t.Fatal(result, err)
	}
	if _, err := a.conversation.get(api.ACPConversationGet{ConversationID: "obsolete", TerminalID: "term"}); err == nil {
		t.Fatal("lookup crossed conversation generation")
	}
	if _, err := a.conversation.get(api.ACPConversationGet{ConversationID: a.snapshot().Conversation.ID, TerminalID: "term", EntryIDs: []string{entry.ID}}); err == nil {
		t.Fatal("ambiguous lookup accepted")
	}
}
