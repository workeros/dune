package fabricd

import (
	"bytes"
	"encoding/json"
	"io"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/aiomni/dune/internal/process"
	"github.com/aiomni/dune/internal/wire"
	"github.com/aiomni/dune/pkg/api"
	pb "github.com/aiomni/dune/proto/dune/dtp/v1"
	"google.golang.org/protobuf/proto"
)

type bufferWriteCloser struct{ bytes.Buffer }

func (b *bufferWriteCloser) Close() error { return nil }

func TestACPStreamPublishesInputAndOutputRPCs(t *testing.T) {
	input := &bufferWriteCloser{}
	r := &runtime{cwd: "/tmp", subs: map[*subscription]bool{}, p: &process.Process{Input: input}}
	a := newACPController(r)
	r.acp = a
	s, err := r.subscribe(false)
	if err != nil {
		t.Fatal(err)
	}

	if err := a.send(map[string]any{"jsonrpc": "2.0", "id": "request-1", "method": "initialize", "params": map[string]any{}}); err != nil {
		t.Fatal(err)
	}
	inputEvent := <-s.q
	if inputEvent.Kind != "acp_stream" || !bytes.Contains(inputEvent.Payload, []byte(`"direction":"input"`)) || !bytes.Contains(inputEvent.Payload, []byte(`"method":"initialize"`)) {
		t.Fatalf("unexpected input stream event: kind=%q payload=%s", inputEvent.Kind, inputEvent.Payload)
	}

	update := []byte(`{"jsonrpc":"2.0","method":"session/update","params":{"sessionId":"session-1","update":{"sessionUpdate":"agent_message_chunk","content":{"type":"text","text":"hello"}}}}`)
	a.receive(update)
	updateEvent := <-s.q
	if updateEvent.Kind != "acp_update" || len(updateEvent.Data) != 0 || !bytes.Contains(updateEvent.Payload, []byte(`"sessionUpdate":"agent_message_chunk"`)) {
		t.Fatalf("unexpected update stream event: kind=%q payload=%s data=%s", updateEvent.Kind, updateEvent.Payload, updateEvent.Data)
	}

	response := []byte(`{"jsonrpc":"2.0","id":"unknown","result":{"stopReason":"end_turn"}}`)
	a.receive(response)
	outputEvent := <-s.q
	if outputEvent.Kind != "acp_stream" || !bytes.Contains(outputEvent.Payload, []byte(`"direction":"output"`)) || !bytes.Contains(outputEvent.Payload, []byte(`"stopReason":"end_turn"`)) {
		t.Fatalf("unexpected output stream event: kind=%q payload=%s", outputEvent.Kind, outputEvent.Payload)
	}
}

func TestACPReplayBackpressuresInsteadOfDroppingBurst(t *testing.T) {
	r := &runtime{subs: map[*subscription]bool{}}
	a := newACPController(r)
	r.acp = a
	s, err := r.subscribe(false)
	if err != nil {
		t.Fatal(err)
	}
	a.replaying.Store(true)

	payload := bytes.Repeat([]byte("x"), 256*1024)
	done := make(chan struct{})
	go func() {
		defer close(done)
		for i := 0; i < 160; i++ {
			r.emit(&pb.Message{Kind: "acp_update", Payload: payload})
		}
	}()

	// Sixteen MiB cannot hold this whole burst. The producer must wait for the
	// consumer to release byte budget, while the subscription stays attached.
	select {
	case <-done:
		t.Fatal("replay producer did not apply byte backpressure")
	case <-time.After(10 * time.Millisecond):
	}
	for i := 0; i < 160; i++ {
		m := <-s.q
		s.release(m)
	}
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("replay remained blocked after queue drained")
	}
	select {
	case <-s.failed:
		t.Fatal("replay burst detached a live subscriber")
	default:
	}
}

func TestACPLiveBurstKeepsSmallQueueLimit(t *testing.T) {
	r := &runtime{subs: map[*subscription]bool{}}
	a := newACPController(r)
	r.acp = a
	s, err := r.subscribe(false)
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i <= acpLiveQueueMessages; i++ {
		r.emit(&pb.Message{Kind: "acp_update", Payload: []byte(`{"update":{}}`)})
	}
	select {
	case <-s.failed:
	default:
		t.Fatal("live burst exceeded the small queue without detaching the stalled subscriber")
	}
	r.mu.Lock()
	_, attached := r.subs[s]
	r.mu.Unlock()
	if attached {
		t.Fatal("failed live subscriber remained attached")
	}
}

func TestACPScannerAcceptsAndCompactsLargeHistoryUpdate(t *testing.T) {
	r := &runtime{adapter: "acp", subs: map[*subscription]bool{}}
	a := newACPController(r)
	r.acp = a
	s, err := r.subscribe(false)
	if err != nil {
		t.Fatal(err)
	}
	update, err := json.Marshal(map[string]any{
		"jsonrpc": "2.0",
		"method":  "session/update",
		"params": map[string]any{
			"sessionId": "large-history",
			"update": map[string]any{
				"sessionUpdate": "tool_call",
				"rawOutput":     strings.Repeat("x", 900*1024),
			},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	lineLimit, _ := acpMemoryLimits(machineMemoryBytes())
	if len(update) <= acpBrowserUpdateBytes || len(update) >= lineLimit {
		t.Fatalf("test update size %d does not exercise the raised scanner limit", len(update))
	}
	var wg sync.WaitGroup
	wg.Add(1)
	r.read(bytes.NewReader(append(update, '\n')), "data", &wg)
	wg.Wait()
	m := <-s.q
	if m.Kind != "acp_update" || len(m.Payload) >= acpBrowserUpdateBytes || !bytes.Contains(m.Payload, []byte(`"duneOmittedBytes"`)) {
		t.Fatalf("large history update was not compacted: kind=%q bytes=%d payload=%s", m.Kind, len(m.Payload), m.Payload)
	}
}

func TestACPLargeMessageTextIsChunkedWithoutLoss(t *testing.T) {
	r := &runtime{subs: map[*subscription]bool{}}
	a := newACPController(r)
	r.acp = a
	s, err := r.subscribe(false)
	if err != nil {
		t.Fatal(err)
	}
	want := strings.Repeat("世界", 200*1024)
	a.emitUpdate(api.Payload(map[string]any{"sessionId": "large", "update": map[string]any{"sessionUpdate": "agent_message_chunk", "messageId": "one", "content": map[string]any{"type": "text", "text": want}, "rawOutput": strings.Repeat("x", 5*1024*1024)}}))
	var got strings.Builder
	for got.Len() < len(want) {
		m := <-s.q
		if proto.Size(m) > wire.MaxMessage {
			t.Fatalf("chunk exceeded transport frame: %d", proto.Size(m))
		}
		if bytes.Contains(m.Payload, []byte(`"rawOutput"`)) {
			t.Fatal("large unrelated field was copied into message chunk")
		}
		var params struct {
			Update struct {
				Content struct {
					Text string `json:"text"`
				} `json:"content"`
			} `json:"update"`
		}
		if err := json.Unmarshal(m.Payload, &params); err != nil {
			t.Fatal(err)
		}
		got.WriteString(params.Update.Content.Text)
	}
	if got.String() != want {
		t.Fatal("chunked ACP text changed")
	}
}

func TestACPOversizedLineIsOmittedAndFollowingRPCContinues(t *testing.T) {
	r := &runtime{adapter: "acp", subs: map[*subscription]bool{}}
	a := newACPController(r)
	r.acp = a
	s, err := r.subscribe(false)
	if err != nil {
		t.Fatal(err)
	}
	lineLimit, _ := acpMemoryLimits(machineMemoryBytes())
	tooLarge := `{"jsonrpc":"2.0","method":"session/update","params":{"update":{"sessionUpdate":"tool_call","content":"` + strings.Repeat("x", lineLimit) + `"}}}`
	following := `{"jsonrpc":"2.0","method":"session/update","params":{"update":{"sessionUpdate":"agent_message_chunk","content":{"type":"text","text":"after"}}}}`
	var wg sync.WaitGroup
	wg.Add(1)
	r.read(strings.NewReader(tooLarge+"\n"+following+"\n"), "data", &wg)
	wg.Wait()
	first, second := <-s.q, <-s.q
	if first.Kind != "acp_notice" || second.Kind != "acp_update" || !bytes.Contains(second.Payload, []byte(`"after"`)) {
		t.Fatalf("oversized line did not recover: first=%q second=%q payload=%s", first.Kind, second.Kind, second.Payload)
	}
	r.mu.Lock()
	exited := r.exit != nil
	r.mu.Unlock()
	if exited {
		t.Fatal("oversized ACP notification stopped the Runtime")
	}
}

func TestACPMemoryLimitsScaleWithMachineSize(t *testing.T) {
	for _, test := range []struct {
		memory, line, replay uint64
	}{
		{2 << 30, 2 << 20, 8 << 20},
		{8 << 30, 4 << 20, 16 << 20},
		{32 << 30, 8 << 20, 32 << 20},
	} {
		line, replay := acpMemoryLimits(test.memory)
		if uint64(line) != test.line || uint64(replay) != test.replay {
			t.Fatalf("memory %d: got line=%d replay=%d", test.memory, line, replay)
		}
	}
}

// The simulated Agent chooses independent list/load capabilities and replays
// updates. Dune retains metadata but never adds replay text to ACP state.
func TestACPCapabilityCombinationsAndLoad(t *testing.T) {
	for _, capabilities := range []string{`{}`, `{"loadSession":true}`, `{"sessionCapabilities":{"list":{}}}`, `{"loadSession":true,"sessionCapabilities":{"list":{}}}`} {
		t.Run(capabilities, func(t *testing.T) {
			rd, wr := io.Pipe()
			defer rd.Close()
			defer wr.Close()
			r := &runtime{cwd: "/tmp", subs: map[*subscription]bool{}, p: &process.Process{Input: wr}}
			a := newACPController(r)
			r.acp = a
			go func() {
				dec := json.NewDecoder(rd)
				for {
					var m struct {
						ID     string         `json:"id"`
						Method string         `json:"method"`
						Params map[string]any `json:"params"`
					}
					if dec.Decode(&m) != nil {
						return
					}
					var result any
					switch m.Method {
					case "initialize":
						var caps any
						_ = json.Unmarshal([]byte(capabilities), &caps)
						result = map[string]any{"protocolVersion": 1, "agentCapabilities": caps}
					case "session/list":
						result = map[string]any{"sessions": []any{map[string]string{"sessionId": "old", "cwd": "/tmp"}}, "nextCursor": "page-two"}
					case "session/load":
						a.receive([]byte(`{"jsonrpc":"2.0","method":"session/update","params":{"sessionId":"old","update":{"sessionUpdate":"agent_message_chunk","content":{"type":"text","text":"secret-transcript-marker"}}}}`))
						if m.Params["sessionId"] == "fail" {
							b, _ := json.Marshal(map[string]any{"jsonrpc": "2.0", "id": m.ID, "error": map[string]any{"code": -32000, "message": "load failed", "data": map[string]string{"details": "thread already has an active writer"}}})
							a.receive(b)
							continue
						}
						result = map[string]any{}
					}
					b, _ := json.Marshal(map[string]any{"jsonrpc": "2.0", "id": m.ID, "result": result})
					a.receive(b)
				}
			}()
			a.initialize()
			s := a.snapshot()
			if !s.Ready {
				t.Fatal(s.Error)
			}
			wait := func() {
				t.Helper()
				for i := 0; i < 100; i++ {
					if a.snapshot().Busy == "" {
						return
					}
					time.Sleep(time.Millisecond)
				}
				t.Fatal("ACP busy")
			}
			for _, action := range []string{"list", "load"} {
				supported := s.CanList
				if action == "load" {
					supported = s.CanLoad
				}
				_, err := a.action(acpAction{Action: action, SessionID: "old"})
				if supported && err != nil || !supported && err == nil {
					t.Fatalf("%s capability mismatch: %v", action, err)
				}
				wait()
			}
			b, _ := json.Marshal(a.snapshot())
			for _, x := range []string{"secret-transcript-marker", "sessionUpdate"} {
				if strings.Contains(string(b), x) {
					t.Fatal("transcript retained in state")
				}
			}
			if s.CanList {
				var list struct {
					Next string `json:"nextCursor"`
				}
				_ = json.Unmarshal(a.snapshot().List, &list)
				if list.Next != "page-two" {
					t.Fatal("pagination lost")
				}
			}
			if s.CanLoad {
				_, err := a.action(acpAction{Action: "load", SessionID: "fail"})
				if err != nil {
					t.Fatal(err)
				}
				wait()
				if got := a.snapshot(); got.SessionID != "" || got.Error != "ACP -32000: load failed: thread already has an active writer" {
					t.Fatalf("failed load lost error detail: %+v", got)
				}
			}
			a.closed()
		})
	}
}
