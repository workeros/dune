package fabricd

import (
	"encoding/json"
	"testing"

	"github.com/aiomni/dune/pkg/api"
)

func TestACPElicitationRequiresHostOptIn(t *testing.T) {
	for _, draft := range []bool{false, true} {
		for _, enabled := range []bool{false, true} {
			a, requests := queueFixture(t)
			a.v2Draft, a.elicitationEnabled = draft, enabled
			params := a.initializationParams()
			key := "clientCapabilities"
			if draft {
				key = "capabilities"
			}
			capabilities := params[key].(map[string]any)
			_, advertised := capabilities["elicitation"]
			if advertised != enabled {
				t.Fatalf("v2=%v enabled=%v: unexpected capabilities %v", draft, enabled, capabilities)
			}
			// A request during initialize must either be answerable by the opted-in
			// host or fail explicitly, never block a host with no interaction UI.
			a.mu.Lock()
			a.state.Ready = false
			a.pending["initialize"] = make(chan acpReply, 1)
			a.methods["initialize"] = "initialize"
			a.mu.Unlock()
			elicit(a, "question", map[string]any{"requestId": "initialize", "mode": "form", "message": "Choose a workspace", "requestedSchema": map[string]any{}})
			if !enabled {
				reply := takeRPC(t, requests)
				var failure struct {
					Code int `json:"code"`
				}
				if json.Unmarshal(reply.Error, &failure) != nil || failure.Code != -32601 || len(a.snapshot().Elicitations) != 0 || a.snapshot().Resources.Elicitations.Used != 0 {
					t.Fatalf("unadvertised elicitation was not rejected: %+v", reply)
				}
				continue
			}
			pending := a.snapshot().Elicitations
			if len(pending) != 1 {
				t.Fatal("enabled host did not receive its initialization question", pending)
			}
			if _, err := a.action(api.ACPAction{Action: "elicitation", ElicitationID: pending[0].ID, ElicitationResponse: &api.ACPElicitationResponse{Action: "decline"}}); err != nil {
				t.Fatal("initialization question could not be answered", err)
			}
			if reply := takeRPC(t, requests); string(reply.Result) != `{"action":"decline"}` {
				t.Fatal(reply)
			}
		}
	}
}
