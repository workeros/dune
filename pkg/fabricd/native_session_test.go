package fabricd

import (
	"testing"

	"github.com/aiomni/dune/pkg/api"
)

func TestNativeSessionConfirmationSurvivesLaterSwitchAndFailedLoad(t *testing.T) {
	a, requests := queueFixture(t)
	a.mu.Lock()
	a.state.Agent = api.Payload(map[string]string{"name": "fixture", "version": "1.2.3"})
	a.state.CanList = false
	a.mu.Unlock()
	first := submitAction(t, a, api.ACPAction{Action: "new", Cwd: "/first"})
	firstRPC := takeRPC(t, requests)
	if a.r.info().NativeSession != nil {
		t.Fatal("unconfirmed request was published as a native session")
	}
	replyRPC(a, firstRPC, map[string]string{"sessionId": "first-native"})
	confirmed := waitOperation(t, a, first).NativeSession
	if confirmed == nil || *confirmed != (api.NativeSession{ID: "first-native", Cwd: "/first", Sequence: 1, Source: "acp-response", AgentVersion: "1.2.3", ResumeSupported: true}) {
		t.Fatalf("missing native confirmation or load/list conflated: %+v", confirmed)
	}
	second := submitAction(t, a, api.ACPAction{Action: "load", SessionID: "second-native", Cwd: "/second"})
	secondRPC := takeRPC(t, requests)
	if current := a.r.info().NativeSession; *current != *confirmed {
		t.Fatal("pending load replaced the previous confirmation")
	}
	replyRPC(a, secondRPC, map[string]any{})
	loaded := waitOperation(t, a, second).NativeSession
	if loaded == nil || loaded.Sequence != 2 || loaded.ID != "second-native" || loaded.Cwd != "/second" {
		t.Fatalf("load confirmation: %+v", loaded)
	}
	if current := a.r.info().NativeSession; *current != *loaded {
		t.Fatal("Runtime discovery did not retain the latest confirmation")
	}
	if old := readOperation(t, a, first).NativeSession; old == nil || *old != *confirmed {
		t.Fatal("old operation was redirected to a later native session")
	}
	failed := submitAction(t, a, api.ACPAction{Action: "load", SessionID: "missing", Cwd: "/missing"})
	rpc := takeRPC(t, requests)
	a.receive(api.Payload(map[string]any{"jsonrpc": "2.0", "id": rpc.ID, "error": map[string]any{"code": -32602, "message": "not found"}}))
	if result := waitOperation(t, a, failed); result.State != "failed" || result.NativeSession != nil {
		t.Fatal("failed load produced a native confirmation")
	}
	if current := a.r.info().NativeSession; *current != *loaded {
		t.Fatal("failed load erased the last recoverable native session")
	}
}

func TestNativeSessionRejectsInvalidIDAndBoundsAgentVersion(t *testing.T) {
	a, requests := queueFixture(t)
	a.mu.Lock()
	a.state.Agent = api.Payload(map[string]string{"version": "bad\nversion"})
	a.state.CanLoad = false
	a.mu.Unlock()
	first := submitAction(t, a, api.ACPAction{Action: "new"})
	replyRPC(a, takeRPC(t, requests), map[string]string{"sessionId": "valid"})
	native := waitOperation(t, a, first).NativeSession
	if native == nil || native.AgentVersion != "" || native.ResumeSupported {
		t.Fatalf("unsafe version or invented load support: %+v", native)
	}
	bad := submitAction(t, a, api.ACPAction{Action: "new"})
	replyRPC(a, takeRPC(t, requests), map[string]string{"sessionId": "bad\nid"})
	if result := waitOperation(t, a, bad); result.State != "unknown" || result.NativeSession != nil {
		t.Fatal("invalid native ID was accepted")
	}
}
