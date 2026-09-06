package tests

import (
	"encoding/json"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/aiomni/dune/internal/wire"
)

func TestManagedACPOfflinePermissions(t *testing.T) {
	h := start(t)
	mock := filepath.Join(h.dir, "mock-acp")
	output, err := exec.Command("go", "build", "-o", mock, "../samples/mock-acp").CombinedOutput()
	if err != nil {
		t.Fatalf("mock build: %s %v", output, err)
	}
	p := profile(h.dir, "acp", mock)
	p.ManagedACP = true
	rt, stream, err := h.client.Start(h.ctx, p)
	must(t, err)
	stream.Close()
	type state struct {
		Ready       bool   `json:"ready"`
		Busy        string `json:"busy"`
		SessionID   string `json:"session_id"`
		Permissions []struct {
			ID string `json:"id"`
		} `json:"permissions"`
	}
	read := func() state {
		var s state
		must(t, h.client.CallID(h.ctx, "acp.state", wire.ID(), map[string]any{}, &s, &rt))
		return s
	}
	await := func(check func(state) bool) state {
		t.Helper()
		for deadline := time.Now().Add(8 * time.Second); time.Now().Before(deadline); {
			s := read()
			if check(s) {
				return s
			}
			time.Sleep(20 * time.Millisecond)
		}
		t.Fatal("ACP state did not settle")
		return state{}
	}
	action := func(a map[string]any) error {
		var out json.RawMessage
		return h.client.CallID(h.ctx, "acp.action", wire.ID(), a, &out, &rt)
	}
	await(func(s state) bool { return s.Ready })
	must(t, action(map[string]any{"action": "new"}))
	await(func(s state) bool { return s.SessionID != "" && s.Busy == "" })
	for _, a := range []string{"list", "load"} {
		if err := action(map[string]any{"action": a, "session_id": "mock-session"}); err == nil || !strings.Contains(err.Error(), "UNSUPPORTED") {
			t.Fatalf("unadvertised %s: %v", a, err)
		}
	}
	must(t, action(map[string]any{"action": "prompt", "text": "offline permission"}))
	pending := await(func(s state) bool { return len(s.Permissions) == 1 })
	if err := action(map[string]any{"action": "new"}); err == nil {
		t.Fatal("concurrent new accepted")
	}
	if _, err := h.client.Attach(h.ctx, rt, false); err == nil {
		t.Fatal("raw ACP input was not rejected")
	}
	h.client.Close()
	time.Sleep(100 * time.Millisecond)
	h.reconnect()
	still := read()
	if still.Busy != "prompt" || len(still.Permissions) != 1 || still.Permissions[0].ID != pending.Permissions[0].ID {
		t.Fatal("offline permission changed")
	}
	permission := map[string]any{"action": "permission", "permission_id": pending.Permissions[0].ID, "option_id": "allow"}
	must(t, action(permission))
	if err := action(permission); err == nil {
		t.Fatal("permission answered twice")
	}
	await(func(s state) bool { return s.Busy == "" && len(s.Permissions) == 0 })
	must(t, action(map[string]any{"action": "prompt", "text": "permission cancellation"}))
	stale := await(func(s state) bool { return len(s.Permissions) == 1 })
	must(t, action(map[string]any{"action": "cancel"}))
	await(func(s state) bool { return s.Busy == "" && len(s.Permissions) == 0 })
	if err := action(map[string]any{"action": "permission", "permission_id": stale.Permissions[0].ID, "option_id": "allow"}); err == nil {
		t.Fatal("cancelled permission accepted")
	}
	err = h.client.Stop(h.ctx, rt)
	must(t, err)
}
