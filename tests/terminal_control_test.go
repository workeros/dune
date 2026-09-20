package tests

import (
	"encoding/json"
	"testing"

	"github.com/aiomni/dune/pkg/api"
	"github.com/aiomni/dune/pkg/sdk"
	pb "github.com/aiomni/dune/proto/dune/dtp/v1"
)

func terminalControlEvent(t *testing.T, stream *sdk.Stream, writable, available bool) uint64 {
	t.Helper()
	for {
		message, err := stream.Recv()
		must(t, err)
		if message.Kind != "control" {
			continue
		}
		var state api.TerminalControlState
		must(t, json.Unmarshal(message.Payload, &state))
		if state.Writable == writable && state.Available == available {
			return message.ControlEpoch
		}
	}
}

func TestTerminalViewersTakeControlWithoutDisconnecting(t *testing.T) {
	h := start(t)
	runtime, first, err := testStartProfile(h.client, h.ctx, profile(h.dir, "pty", "/bin/sh"))
	must(t, err)
	defer first.Close()
	defer testStopRuntime(h.client, h.ctx, runtime)
	originalEpoch := terminalControlEvent(t, first, true, false)
	second, err := h.client.Attach(h.ctx, runtime, false)
	must(t, err)
	defer second.Close()
	terminalControlEvent(t, second, false, false)
	must(t, second.Control("take"))
	terminalControlEvent(t, second, true, false)
	terminalControlEvent(t, first, false, false)
	// Even deliberately sending bytes with the previous owner's valid-looking
	// token cannot bypass the server's ownership check.
	must(t, first.Send(&pb.Message{Kind: "input", Data: []byte("touch forbidden\n"), ControlEpoch: originalEpoch}))
	receive(t, first, "input_rejected", "")
	must(t, first.Send(&pb.Message{Kind: "resize", Payload: api.Payload(api.Resize{Rows: 9, Cols: 17}), ControlEpoch: originalEpoch}))
	receive(t, first, "input_rejected", "")
	must(t, first.Send(&pb.Message{Kind: "signal", Data: []byte("TERM"), ControlEpoch: originalEpoch}))
	receive(t, first, "input_rejected", "")
	must(t, second.Resize(30, 100))
	receive(t, second, "written", "")
	must(t, first.Resize(40, 120))
	receive(t, first, "written", "")
	_, err = second.Input([]byte("printf 'CONTROL_%s\\n' OK; stty size; test ! -e forbidden && printf 'FENCE_%s\\n' OK\n"))
	must(t, err)
	receive(t, second, "data", "CONTROL_OK")
	receive(t, first, "data", "CONTROL_OK")
	// Inspect the pane itself: an observer's viewport must not resize the TUI.
	var capture struct{ Rows, Cols uint16 }
	must(t, h.client.CallID(h.ctx, "runtime.capture", "control-capture", struct{}{}, &capture, &runtime))
	if capture.Rows != 30 || capture.Cols != 100 {
		t.Fatalf("observer resized pane: %+v", capture)
	}
	if result := h.exec("/bin/sh", "-c", "test ! -e forbidden"); result.ExitCode != 0 {
		t.Fatal("old owner wrote input")
	}
	// A fresh connection never preempts; it remains usable for output.
	third, err := h.client.Attach(h.ctx, runtime, false)
	must(t, err)
	defer third.Close()
	terminalControlEvent(t, third, false, false)
	must(t, second.Control("release"))
	terminalControlEvent(t, third, false, true)
	must(t, third.Control("acquire"))
	terminalControlEvent(t, third, true, false)
	second.Close()
	_, err = third.Input([]byte("printf 'LATE_CLEANUP_%s\\n' OK\n"))
	must(t, err)
	receive(t, third, "data", "LATE_CLEANUP_OK")
	third.Close()
	terminalControlEvent(t, first, false, true)
}
