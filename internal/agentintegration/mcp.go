package agentintegration

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"time"

	"github.com/aiomni/dune/internal/mcpbridge"
	"github.com/aiomni/dune/pkg/api"
	"golang.org/x/sys/unix"
)

type mcpGate struct {
	Deadline time.Time `json:"deadline"`
}

type mcpConfiguration struct {
	Binding
	Config api.AgentMCP `json:"config"`
}

// prepareMCP injects only a bridge location. The native CLI never receives the
// credential in argv or environment. Its bridge waits for the host to record
// the Runtime and configure it, including when MCP starts before profile.start
// has returned. Existing native settings and trust policies remain in effect.
func prepareMCP(dir, agent string, argv []string) ([]string, error) {
	root, err := privateRoot(dir)
	if err != nil {
		return nil, err
	}
	defer root.Close()
	if err := writeJSON(root, "mcp-gate.json", mcpGate{Deadline: time.Now().Add(90 * time.Second)}); err != nil {
		return nil, err
	}
	helper := filepath.Join(dir, "helper")
	if agent == "claude" {
		settings, _ := json.Marshal(map[string]any{"mcpServers": map[string]any{"dune-agents": map[string]any{
			"command": helper, "args": []string{mcpbridge.Command}, "env": map[string]string{SessionDirEnv: dir},
		}}})
		return append(argv, "--mcp-config", string(settings)), nil
	}
	value := "mcp_servers.dune-agents={command=" + strconv.Quote(helper) + ",args=[" + strconv.Quote(mcpbridge.Command) + "],env={" + SessionDirEnv + "=" + strconv.Quote(dir) + "}}"
	return append(argv, "-c", value), nil
}

// ConfigureMCP publishes one private credential for this exact tmux Runtime.
// The hook writer lock also serializes configuration with Runtime destruction.
// Nothing is routed or queued here; subsequent calls cannot replace a token.
func ConfigureMCP(ctx context.Context, dir, id, incarnation string, config api.AgentMCP) error {
	if err := config.Validate(); err != nil {
		return &api.Error{Code: "INVALID_ARGUMENT", Detail: "invalid Agent MCP configuration"}
	}
	root, err := privateRoot(dir)
	if err != nil {
		return &api.Error{Code: "UNSUPPORTED", Detail: "Runtime has no native MCP integration"}
	}
	defer root.Close()
	lock, err := root.OpenFile("session.lock", os.O_RDWR, 0600)
	if err != nil {
		return err
	}
	defer lock.Close()
	if err := acquire(ctx, lock); err != nil {
		return err
	}
	defer unix.Flock(int(lock.Fd()), unix.LOCK_UN)
	var binding Binding
	if err := readJSON(root, "binding.json", &binding); err != nil || !validBinding(binding) || binding.RuntimeID != id || binding.Incarnation != incarnation {
		return &api.Error{Code: "STALE_RUNTIME", Detail: "native MCP context belongs to another Runtime"}
	}
	var gate mcpGate
	if err := readJSON(root, "mcp-gate.json", &gate); err != nil {
		return &api.Error{Code: "UNSUPPORTED", Detail: "Runtime was not launched with Agent MCP"}
	}
	if _, err := root.Stat("mcp.json"); !os.IsNotExist(err) {
		return &api.Error{Code: "CONFLICT", Detail: "native MCP configuration is already fixed or unavailable"}
	}
	if !time.Now().Before(gate.Deadline) {
		return &api.Error{Code: "CONFLICT", Detail: "native MCP configuration deadline has passed"}
	}
	return writeJSON(root, "mcp.json", mcpConfiguration{Binding: binding, Config: config})
}

// AwaitMCP is used only by the retained stdio bridge. The private configuration
// survives fabricd restarts alongside tmux, and is removed with the Runtime.
// A missing configuration expires; the bridge never starts an unauthenticated
// connection or retries a tool call.
func AwaitMCP(ctx context.Context, dir string) (api.AgentMCP, error) {
	root, err := privateRoot(dir)
	if err != nil {
		return api.AgentMCP{}, err
	}
	defer root.Close()
	var binding Binding
	var gate mcpGate
	if err := readJSON(root, "binding.json", &binding); err != nil || !validBinding(binding) {
		return api.AgentMCP{}, fmt.Errorf("native MCP binding is unavailable")
	}
	if err := readJSON(root, "mcp-gate.json", &gate); err != nil || gate.Deadline.IsZero() {
		return api.AgentMCP{}, fmt.Errorf("native MCP launch gate is unavailable")
	}
	for {
		var value mcpConfiguration
		err := readJSON(root, "mcp.json", &value)
		if err == nil {
			if value.Binding != binding || value.Config.Validate() != nil {
				return api.AgentMCP{}, fmt.Errorf("native MCP configuration does not match its Runtime")
			}
			return value.Config, nil
		}
		if !os.IsNotExist(err) || !time.Now().Before(gate.Deadline) {
			return api.AgentMCP{}, fmt.Errorf("native MCP configuration was not confirmed")
		}
		select {
		case <-ctx.Done():
			return api.AgentMCP{}, ctx.Err()
		case <-time.After(25 * time.Millisecond):
		}
	}
}
