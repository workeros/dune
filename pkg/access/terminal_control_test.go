package access

import (
	"testing"

	"github.com/aiomni/dune/pkg/api"
	pb "github.com/aiomni/dune/proto/dune/dtp/v1"
)

func TestTerminalControlRequiresWritablePTYSubscription(t *testing.T) {
	for _, tc := range []struct {
		adapter, kind, action string
		observe, allowed      bool
	}{
		{"pty", "control", "take", false, true},
		{"pty", "control", "release", false, true},
		{"pty", "control", "acquire", false, true},
		{"pty", "history", "older", false, true},
		{"pty", "control", "take", true, false},
		{"acp", "control", "take", false, false},
		{"pty", "control", "unknown", false, false},
		{"pty", "history", "take", false, false},
	} {
		request, err := continuation(Request{Operation: "runtime.attach", Resource: Resource{Observe: tc.observe}}, RuntimeIdentity{ID: "runtime", Adapter: tc.adapter}, &pb.Message{Kind: tc.kind, Payload: api.Payload(api.TerminalControl{Action: tc.action})})
		if (err == nil) != tc.allowed {
			t.Fatalf("%+v: %v", tc, err)
		}
		if tc.allowed && (request.Suboperation != tc.kind || request.Mode != tc.action) {
			t.Fatalf("lost policy action: %+v", request)
		}
	}
}
