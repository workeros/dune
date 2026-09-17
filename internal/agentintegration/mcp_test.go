package agentintegration

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/aiomni/dune/internal/mcpbridge"
	"github.com/aiomni/dune/pkg/api"
)

func mcpFixture(t *testing.T, agent string) (string, Binding, []string, []string) {
	t.Helper()
	dir := t.TempDir()
	if err := os.Chmod(dir, 0700); err != nil {
		t.Fatal(err)
	}
	binding := Binding{RuntimeID: "runtime", Incarnation: "incarnation", Agent: agent}
	argv, env, err := Launch(dir, binding, []string{agent}, []string{"CUSTOM=preserved", mcpbridge.TokenEnv + "=parent-secret", mcpbridge.URLEnv + "=https://parent.test/mcp"}, true)
	if err != nil {
		t.Fatal(err)
	}
	return dir, binding, argv, env
}

func TestNativeMCPBridgeWaitsForSingleBoundConfiguration(t *testing.T) {
	for _, agent := range []string{"claude", "codex"} {
		t.Run(agent, func(t *testing.T) {
			dir, binding, argv, env := mcpFixture(t, agent)
			joined := strings.Join(append(argv, env...), "\n")
			if strings.Contains(joined, "parent-secret") || strings.Contains(joined, "https://parent.test") || !strings.Contains(joined, mcpbridge.Command) || !strings.Contains(joined, "CUSTOM=preserved") {
				t.Fatal("native MCP launch leaked parent credentials or lost bridge configuration")
			}
			if agent == "claude" {
				var settings struct {
					Servers map[string]struct {
						Command string
						Args    []string
						Env     map[string]string
					} `json:"mcpServers"`
				}
				if len(argv) != 5 || argv[3] != "--mcp-config" || json.Unmarshal([]byte(argv[4]), &settings) != nil {
					t.Fatal("invalid Claude MCP argument")
				}
				server := settings.Servers["dune-agents"]
				if server.Command != filepath.Join(dir, "helper") || len(server.Args) != 1 || server.Args[0] != mcpbridge.Command || server.Env[SessionDirEnv] != dir {
					t.Fatal("invalid Claude bridge")
				}
			} else if len(argv) != 5 || argv[3] != "-c" || !strings.HasPrefix(argv[4], "mcp_servers.dune-agents={") {
				t.Fatal("invalid Codex MCP argument")
			}
			ctx, cancel := context.WithTimeout(t.Context(), 3*time.Second)
			defer cancel()
			type outcome struct {
				value api.AgentMCP
				err   error
			}
			ready := make(chan outcome, 1)
			go func() { value, err := AwaitMCP(ctx, dir); ready <- outcome{value, err} }()
			select {
			case <-ready:
				t.Fatal("bridge did not wait for Runtime configuration")
			case <-time.After(30 * time.Millisecond):
			}
			if err := ConfigureMCP(ctx, dir, "wrong", binding.Incarnation, api.AgentMCP{URL: "https://host.test/mcp", Token: "wrong"}); err == nil {
				t.Fatal("configured another Runtime")
			}
			var wg sync.WaitGroup
			errors := make(chan error, 2)
			for _, token := range []string{"one", "two"} {
				wg.Go(func() {
					errors <- ConfigureMCP(ctx, dir, binding.RuntimeID, binding.Incarnation, api.AgentMCP{URL: "https://host.test/mcp", Token: token})
				})
			}
			wg.Wait()
			close(errors)
			successes := 0
			for err := range errors {
				if err == nil {
					successes++
				}
			}
			if successes != 1 {
				t.Fatal("configuration was not exclusive", successes)
			}
			result := <-ready
			if result.err != nil || result.value.Token == "" {
				t.Fatal("bridge did not receive credential", result.err)
			}
			info, err := os.Stat(filepath.Join(dir, "mcp.json"))
			if err != nil || info.Mode().Perm() != 0600 {
				t.Fatal("credential file is not private", err)
			}
			// Later bridge processes may reconnect after the initial gate deadline.
			root, err := privateRoot(dir)
			if err != nil {
				t.Fatal(err)
			}
			err = writeJSON(root, "mcp-gate.json", mcpGate{Deadline: time.Now().Add(-time.Second)})
			root.Close()
			if err != nil {
				t.Fatal(err)
			}
			again, err := AwaitMCP(ctx, dir)
			if err != nil || again != result.value {
				t.Fatal("lost configured credential", err)
			}
			if err := Remove(dir); err != nil {
				t.Fatal(err)
			}
			if _, err := AwaitMCP(ctx, dir); err == nil {
				t.Fatal("removed Runtime retained MCP")
			}
		})
	}
}

func TestNativeMCPMissingExpiredAndCorruptConfigurationFails(t *testing.T) {
	for _, mode := range []string{"cancelled", "expired", "corrupt", "binding", "not-injected"} {
		t.Run(mode, func(t *testing.T) {
			dir, binding, _, _ := mcpFixture(t, "codex")
			root, err := privateRoot(dir)
			if err != nil {
				t.Fatal(err)
			}
			defer root.Close()
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			switch mode {
			case "cancelled":
				cancel()
			case "expired":
				err = writeJSON(root, "mcp-gate.json", mcpGate{Deadline: time.Now().Add(-time.Second)})
			case "corrupt":
				err = os.WriteFile(filepath.Join(dir, "mcp.json"), []byte("broken"), 0600)
			case "binding":
				binding.Incarnation = "different"
				err = writeJSON(root, "mcp.json", mcpConfiguration{Binding: binding, Config: api.AgentMCP{URL: "https://host.test/mcp", Token: "secret"}})
			case "not-injected":
				err = root.Remove("mcp-gate.json")
			}
			if err != nil {
				t.Fatal(err)
			}
			if _, err := AwaitMCP(ctx, dir); err == nil {
				t.Fatal("invalid configuration accepted")
			}
			if mode != "cancelled" {
				if err := ConfigureMCP(t.Context(), dir, "runtime", "incarnation", api.AgentMCP{URL: "https://host.test/mcp", Token: "replacement"}); err == nil {
					t.Fatal("invalid context overwritten")
				}
			}
		})
	}
}

func TestNativeMCPConfigurationSerializesWithRetirement(t *testing.T) {
	for range 15 {
		dir, binding, _, _ := mcpFixture(t, "claude")
		var wg sync.WaitGroup
		wg.Go(func() {
			_ = ConfigureMCP(t.Context(), dir, binding.RuntimeID, binding.Incarnation, api.AgentMCP{URL: "https://host.test/mcp", Token: "temporary"})
		})
		var removed error
		wg.Go(func() { removed = Remove(dir) })
		wg.Wait()
		if removed != nil {
			t.Fatal(removed)
		}
		if _, err := os.Stat(dir); !os.IsNotExist(err) {
			t.Fatal("configuration recreated retired Runtime", err)
		}
	}
}
