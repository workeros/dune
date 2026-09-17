package agentservice

import (
	"testing"

	"github.com/aiomni/dune/pkg/agents"
)

func TestNativeSessionValidationRejectsAmbiguousRequests(t *testing.T) {
	for _, request := range []agents.OpenSessionRequest{
		{Action: "prompt"}, {Action: "load"}, {Action: "new", SessionID: "unexpected"},
		{Action: "load", SessionID: "session\n"}, {Action: "new", Cwd: "relative"},
		{Action: "new", Cwd: "/repo\x00"}, {Action: "new", WaitMS: 30001},
	} {
		if err := validateOpenSession(request); err == nil {
			t.Fatal("accepted invalid lifecycle request", request)
		}
	}
}
