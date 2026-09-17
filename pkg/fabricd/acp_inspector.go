package fabricd

import (
	"encoding/json"

	"github.com/aiomni/dune/pkg/api"
)

// redactMCPConfiguration removes MCP connection configuration from diagnostic
// copies and structured error details. The Agent receives the original RPC bytes. Credentials may
// appear in headers, env, arguments or URL userinfo/query, so the entire server
// configuration is replaced instead of maintaining a fragile secret-key list.
func redactMCPConfiguration(frame []byte) json.RawMessage {
	var value any
	if json.Unmarshal(frame, &value) != nil {
		return json.RawMessage(`{"duneNotice":"invalid ACP diagnostic frame omitted"}`)
	}
	if !redactMCPServers(value) {
		return frame
	}
	return api.Payload(value)
}

func redactMCPServers(value any) bool {
	changed := false
	switch value := value.(type) {
	case map[string]any:
		for key, child := range value {
			if key == "mcpServers" {
				value[key] = "[Dune redacted MCP connection configuration]"
				changed = true
			} else if redactMCPServers(child) {
				changed = true
			}
		}
	case []any:
		for _, child := range value {
			if redactMCPServers(child) {
				changed = true
			}
		}
	}
	return changed
}
