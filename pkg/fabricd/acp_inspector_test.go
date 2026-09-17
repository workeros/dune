package fabricd

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"

	"github.com/aiomni/dune/internal/process"
)

func TestACPInspectorRedactsMCPConfigWithoutChangingNativeRPC(t *testing.T) {
	input := &bufferWriteCloser{}
	r := &runtime{cwd: "/tmp", subs: map[*subscription]bool{}, p: &process.Process{Input: input}}
	a := newACPController(r)
	r.acp = a
	s, err := r.subscribe(false)
	if err != nil {
		t.Fatal(err)
	}
	for _, method := range []string{"session/new", "session/load"} {
		request := map[string]any{"jsonrpc": "2.0", "id": "test", "method": method, "params": map[string]any{
			"cwd": "/repo", "sessionId": "original", "mcpServers": []any{
				map[string]any{"name": "tenant", "type": "http", "url": "https://user:url-secret@example.test/mcp?key=query-secret", "headers": []any{map[string]string{"name": "Authorization", "value": "Bearer header-secret"}}},
				map[string]any{"name": "stdio", "command": "/bin/bridge", "args": []string{"argument-secret"}, "env": []any{map[string]string{"name": "TOKEN", "value": "environment-secret"}}},
			},
		}}
		input.Reset()
		if err := a.send(request); err != nil {
			t.Fatal(err)
		}
		event := <-s.q
		for _, secret := range []string{"url-secret", "query-secret", "header-secret", "argument-secret", "environment-secret"} {
			if !bytes.Contains(input.Bytes(), []byte(secret)) || bytes.Contains(event.Payload, []byte(secret)) {
				t.Fatal("MCP secret changed in native RPC or leaked to inspector")
			}
		}
		var native map[string]any
		if json.Unmarshal(input.Bytes(), &native) != nil || !bytes.Contains(event.Payload, []byte("original")) || !bytes.Contains(event.Payload, []byte("redacted")) {
			t.Fatal("native RPC or non-secret diagnostics changed")
		}
		failure := formatACPError(-32000, "bad configuration", json.RawMessage(`{"mcpServers":[{"url":"https://echo-secret@example.test"}]}`))
		if strings.Contains(failure.Error(), "echo-secret") || !strings.Contains(failure.Error(), "redacted") {
			t.Fatal("operation error leaked MCP configuration")
		}
		// Agents can echo invalid MCP configuration in structured RPC errors.
		response := []byte(`{"jsonrpc":"2.0","id":"unknown","error":{"code":-32000,"message":"bad configuration","data":{"mcpServers":[{"env":[{"name":"TOKEN","value":"echo-secret"}]}]}}}`)
		a.receive(response)
		output := <-s.q
		if bytes.Contains(output.Payload, []byte("echo-secret")) || !bytes.Contains(output.Payload, []byte("bad configuration")) {
			t.Fatal("structured Agent error leaked echoed configuration")
		}
	}
}
