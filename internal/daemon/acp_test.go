package daemon

import (
	"encoding/json"
	"io"
	"strings"
	"testing"
	"time"

	"github.com/aiomni/dune/internal/process"
)

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
							b, _ := json.Marshal(map[string]any{"jsonrpc": "2.0", "id": m.ID, "error": map[string]any{"code": -32000, "message": "load failed"}})
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
				if a.snapshot().SessionID != "" || a.snapshot().Error == "" {
					t.Fatal("failed load appeared successful")
				}
			}
			a.closed()
		})
	}
}
