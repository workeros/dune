package fabricd

import (
	"encoding/json"
	"fmt"

	"github.com/aiomni/dune/pkg/api"
)

// v2 is opt-in while the specification is a draft. The pinned schema and
// supported surfaces are documented in docs/acp-v2.md.
func (a *acpController) initializationParams() map[string]any {
	info := map[string]string{"name": "dune", "version": "0.1.0"}
	capabilities := map[string]any{}
	if a.elicitationEnabled {
		capabilities["elicitation"] = map[string]any{"form": map[string]any{}, "url": map[string]any{}}
	}
	if a.v2Draft {
		return map[string]any{"protocolVersion": 2, "info": info, "capabilities": capabilities}
	}
	capabilities["session"] = map[string]any{"configOptions": map[string]any{"boolean": map[string]any{}}}
	return map[string]any{"protocolVersion": 1, "clientInfo": info, "clientCapabilities": capabilities}
}

type acpNegotiation struct {
	version                         int
	info                            json.RawMessage
	load, resume, list, http, stdio bool
	prompt                          api.ACPPromptCapabilities
}

func capabilityObject(value json.RawMessage) bool {
	var object map[string]json.RawMessage
	return json.Unmarshal(value, &object) == nil && object != nil
}

func negotiateACP(result json.RawMessage, allowV2 bool) (acpNegotiation, error) {
	var envelope struct {
		Version           int             `json:"protocolVersion"`
		Info              json.RawMessage `json:"info"`
		AgentInfo         json.RawMessage `json:"agentInfo"`
		Capabilities      json.RawMessage `json:"capabilities"`
		AgentCapabilities json.RawMessage `json:"agentCapabilities"`
	}
	var negotiated acpNegotiation
	if json.Unmarshal(result, &envelope) != nil || (envelope.Version != 1 && !(allowV2 && envelope.Version == 2)) {
		return negotiated, fmt.Errorf("Agent selected an unsupported ACP protocol version")
	}
	negotiated.version = envelope.Version
	if envelope.Version == 1 {
		var capabilities struct {
			Load   bool                      `json:"loadSession"`
			Prompt api.ACPPromptCapabilities `json:"promptCapabilities"`
			MCP    struct {
				HTTP bool `json:"http"`
			} `json:"mcpCapabilities"`
			Sessions struct {
				List json.RawMessage `json:"list"`
			} `json:"sessionCapabilities"`
		}
		if len(envelope.AgentCapabilities) > 0 && json.Unmarshal(envelope.AgentCapabilities, &capabilities) != nil {
			return negotiated, fmt.Errorf("invalid ACP v1 capabilities")
		}
		negotiated.info, negotiated.load, negotiated.list = envelope.AgentInfo, capabilities.Load, capabilityObject(capabilities.Sessions.List)
		negotiated.prompt, negotiated.http, negotiated.stdio = capabilities.Prompt, capabilities.MCP.HTTP, true
		return negotiated, nil
	}
	var info struct {
		Name    *string `json:"name"`
		Version *string `json:"version"`
	}
	if len(envelope.Info) > acpSummaryFieldBytes || json.Unmarshal(envelope.Info, &info) != nil || info.Name == nil || info.Version == nil {
		return negotiated, fmt.Errorf("ACP v2 requires implementation name and version")
	}
	var capabilities struct {
		Session json.RawMessage `json:"session"`
	}
	if !capabilityObject(envelope.Info) || json.Unmarshal(envelope.Capabilities, &capabilities) != nil || !capabilityObject(capabilities.Session) {
		return negotiated, fmt.Errorf("ACP v2 requires implementation info and the session capability")
	}
	var session struct {
		Prompt struct{ Image, Audio, EmbeddedContext json.RawMessage } `json:"prompt"`
		MCP    struct{ HTTP, Stdio json.RawMessage }                   `json:"mcp"`
	}
	if json.Unmarshal(capabilities.Session, &session) != nil {
		return negotiated, fmt.Errorf("invalid ACP v2 session capabilities")
	}
	negotiated.info, negotiated.resume, negotiated.list = envelope.Info, true, true
	negotiated.http, negotiated.stdio = capabilityObject(session.MCP.HTTP), capabilityObject(session.MCP.Stdio)
	negotiated.prompt = api.ACPPromptCapabilities{Image: capabilityObject(session.Prompt.Image), Audio: capabilityObject(session.Prompt.Audio), EmbeddedContext: capabilityObject(session.Prompt.EmbeddedContext)}
	return negotiated, nil
}

func isSessionOpen(action string) bool {
	return action == "new" || action == "load" || action == "resume"
}

func (operation *acpQueuedAction) isForeground() bool {
	return operation != nil && (operation.request.Action == "prompt" || operation.request.Action == "foreground")
}
