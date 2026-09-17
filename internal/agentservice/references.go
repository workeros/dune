package agentservice

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"strings"

	"github.com/aiomni/dune/pkg/api"
	"github.com/aiomni/dune/pkg/runner"
	"github.com/aiomni/dune/pkg/workbench"
)

// References are selectors, not signed credentials. Every use must authorize
// the authenticated Owner and recheck the complete Runner/Runtime binding.
type agentReference struct {
	Target    workbench.AgentTarget `json:"target"`
	NativeID  string                `json:"native_id,omitempty"`
	NativeCwd string                `json:"native_cwd,omitempty"`
}

func agentRef(target workbench.AgentTarget, native *api.NativeSession) string {
	ref := agentReference{Target: target}
	if native != nil {
		ref.NativeID, ref.NativeCwd = native.ID, native.Cwd
	}
	encoded, _ := json.Marshal(ref)
	return "agent_" + base64.RawURLEncoding.EncodeToString(encoded)
}

func parseAgentRef(value string) (agentReference, error) {
	var ref agentReference
	if len(value) > 16*1024 || !strings.HasPrefix(value, "agent_") {
		return ref, invalid("invalid Agent reference")
	}
	decoded, err := base64.RawURLEncoding.DecodeString(strings.TrimPrefix(value, "agent_"))
	if err != nil {
		return ref, invalid("invalid Agent reference")
	}
	decoder := json.NewDecoder(bytes.NewReader(decoded))
	decoder.DisallowUnknownFields()
	if decoder.Decode(&ref) != nil || ref.Target.Validate() != nil || len(ref.NativeID) > 4096 || len(ref.NativeCwd) > 4096 || (ref.NativeID == "") != (ref.NativeCwd == "") {
		return agentReference{}, invalid("invalid Agent reference")
	}
	// Reject trailing JSON too; a selector has exactly one canonical object.
	if canonical := agentRef(ref.Target, referenceNative(ref)); canonical != value {
		return agentReference{}, invalid("invalid Agent reference")
	}
	return ref, nil
}

func referenceNative(ref agentReference) *api.NativeSession {
	if ref.NativeID == "" {
		return nil
	}
	return &api.NativeSession{ID: ref.NativeID, Cwd: ref.NativeCwd}
}

func runtimeFor(target workbench.AgentTarget) api.Runtime {
	return api.Runtime{ID: target.Runtime.ID, Incarnation: target.Runtime.Incarnation, Generation: target.Runtime.Generation, Adapter: target.Runtime.Adapter}
}

func targetFor(binding runner.Binding, runtime api.Runtime) workbench.AgentTarget {
	return workbench.AgentTarget{Binding: binding, Runtime: workbench.RuntimeRef{ID: runtime.ID, Incarnation: runtime.Incarnation, Generation: runtime.Generation, Adapter: runtime.Adapter}}
}
