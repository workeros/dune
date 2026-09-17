package fabricd

import (
	"bytes"
	"encoding/json"
	"os"
	"sync"
	"testing"

	"github.com/aiomni/dune/internal/mcpbridge"
	"github.com/aiomni/dune/pkg/api"
)

func TestACPMCPConfigurationGatesAndPersistsAcrossNativeSessions(t *testing.T) {
	for _, httpCapable := range []bool{true, false} {
		name := "stdio"
		if httpCapable {
			name = "http"
		}
		t.Run(name, func(t *testing.T) {
			a, requests := queueFixture(t)
			a.state.SessionID = ""
			a.requireMCP = true
			a.mcpHTTP = httpCapable
			if _, err := a.action(api.ACPAction{Action: "new"}); err == nil {
				t.Fatal("native session admitted before MCP configuration")
			}
			config := api.AgentMCP{URL: "https://host.test/dune/api/v1/agent-mcp", Token: "private-credential"}
			status, err := a.configureMCP(config)
			if err != nil || status.Transport != name {
				t.Fatalf("transport %q: %v", status.Transport, err)
			}
			if bytes.Contains(api.Payload(a.snapshot()), []byte(config.Token)) {
				t.Fatal("credential in public ACP state")
			}
			for _, action := range []api.ACPAction{{Action: "new"}, {Action: "load", SessionID: "old-session"}} {
				op := submitAction(t, a, action)
				rpc := takeRPC(t, requests)
				var params struct {
					Servers []struct {
						Name    string                         `json:"name"`
						Type    string                         `json:"type"`
						URL     string                         `json:"url"`
						Command string                         `json:"command"`
						Args    []string                       `json:"args"`
						Env     []struct{ Name, Value string } `json:"env"`
						Headers []struct{ Name, Value string } `json:"headers"`
					} `json:"mcpServers"`
				}
				if json.Unmarshal(rpc.Params, &params) != nil || len(params.Servers) != 1 {
					t.Fatal("native RPC has no single injected MCP")
				}
				server := params.Servers[0]
				if server.Name != "dune-agents" {
					t.Fatal("wrong MCP server name")
				}
				if httpCapable {
					if server.Type != "http" || server.URL != config.URL || len(server.Headers) != 1 || server.Headers[0].Name != "Authorization" || server.Headers[0].Value != "Bearer "+config.Token {
						t.Fatal("invalid native HTTP MCP configuration")
					}
				} else {
					executable, _ := os.Executable()
					if server.Type != "" || server.Command != executable || len(server.Args) != 1 || server.Args[0] != mcpbridge.Command || len(server.Env) != 2 || server.Env[0].Name != mcpbridge.URLEnv || server.Env[0].Value != config.URL || server.Env[1].Name != mcpbridge.TokenEnv || server.Env[1].Value != config.Token {
						t.Fatal("invalid native stdio MCP configuration")
					}
				}
				if action.Action == "new" {
					replyRPC(a, rpc, map[string]string{"sessionId": "initial"})
				} else {
					replyRPC(a, rpc, map[string]any{})
				}
				if waitOperation(t, a, op).State != "completed" {
					t.Fatal("native session failed")
				}
				if _, err := a.configureMCP(config); err == nil {
					t.Fatal("credential replaced after native session began")
				}
			}
		})
	}
}

func TestACPMCPConcurrentConfigurationHasOneWinner(t *testing.T) {
	a, _ := queueFixture(t)
	a.state.SessionID = ""
	var wg sync.WaitGroup
	results := make(chan error, 2)
	for _, token := range []string{"first", "second"} {
		wg.Go(func() {
			_, err := a.configureMCP(api.AgentMCP{URL: "https://host.test/mcp", Token: token})
			results <- err
		})
	}
	wg.Wait()
	close(results)
	successes := 0
	for err := range results {
		if err == nil {
			successes++
		}
	}
	if successes != 1 {
		t.Fatalf("configuration winners: %d", successes)
	}
}

func TestACPMCPReadsAdvertisedTransport(t *testing.T) {
	a, requests := queueFixture(t)
	a.state.Ready = false
	a.state.SessionID = ""
	done := make(chan struct{})
	go func() { a.initialize(); close(done) }()
	replyRPC(a, takeRPC(t, requests), map[string]any{"protocolVersion": 1, "agentCapabilities": map[string]any{"mcpCapabilities": map[string]bool{"http": true}}})
	<-done
	status, err := a.configureMCP(api.AgentMCP{URL: "https://host.test/mcp", Token: "secret"})
	if err != nil || status.Transport != "http" {
		t.Fatalf("advertised HTTP transport: %+v, %v", status, err)
	}
}
