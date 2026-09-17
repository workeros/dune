package agentservice

import (
	"encoding/base64"
	"strings"
	"testing"

	"github.com/aiomni/dune/pkg/api"
	"github.com/aiomni/dune/pkg/runner"
)

func TestAgentReferencesRetainExactTargetAndRejectAmbiguousInput(t *testing.T) {
	target := targetFor(runner.Binding{RunnerID: "r", FabricID: "f", MachineID: "m", Revision: 2}, api.Runtime{ID: "agent", Incarnation: "inc", Generation: 3, Adapter: "acp"})
	encoded := agentRef(target, &api.NativeSession{ID: "native", Cwd: "/src"})
	decoded, err := parseAgentRef(encoded)
	if err != nil || decoded.Target != target || decoded.NativeID != "native" || decoded.NativeCwd != "/src" {
		t.Fatal("reference did not retain target", decoded, err)
	}
	raw, _ := base64.RawURLEncoding.DecodeString(strings.TrimPrefix(encoded, "agent_"))
	for _, value := range []string{"", "agent_{}", encoded + "=", "agent_" + base64.RawURLEncoding.EncodeToString(append(raw, []byte(`{}`)...)), "agent_" + base64.RawURLEncoding.EncodeToString([]byte(`{"target":{},"owner":"other"}`)), strings.Repeat("x", 20000)} {
		if _, err := parseAgentRef(value); err == nil {
			t.Fatal("malformed selector accepted")
		}
	}
}
