package api

import "testing"

func TestAgentMCPRejectsUnsafeEndpointAndCredential(t *testing.T) {
	for _, endpoint := range []string{"", "file:///tmp/mcp", "https://user:secret@host/mcp", "https://host/mcp?token=secret", "https://host/mcp?", "https://host/mcp#token"} {
		if (AgentMCP{URL: endpoint, Token: "credential"}).Validate() == nil {
			t.Errorf("accepted unsafe endpoint %q", endpoint)
		}
	}
	for _, token := range []string{"", "token\r\nHeader: bad", "nul\x00token", "with space"} {
		if (AgentMCP{URL: "https://host/mcp", Token: token}).Validate() == nil {
			t.Error("accepted unsafe credential")
		}
	}
	profile := Profile{Version: 1, Kind: "agent", Adapter: "acp", WorkingDirectory: "/tmp", Start: Command{Argv: []string{"agent"}}, RequireAgentMCP: true}
	if profile.Validate() == nil {
		t.Fatal("MCP gate permitted for raw ACP")
	}
	profile.ManagedACP = true
	if err := profile.Validate(); err != nil {
		t.Fatal(err)
	}
}
