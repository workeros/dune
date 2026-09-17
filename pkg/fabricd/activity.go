package fabricd

import (
	"encoding/json"
	"path/filepath"
	"strings"
	"unicode"

	"github.com/aiomni/dune/internal/wire"
	"github.com/aiomni/dune/pkg/api"
)

func (r *runtime) activityLocked() api.AgentActivity {
	if r.activity.Epoch == "" {
		r.activity = api.AgentActivity{State: "unknown", Source: "none", Epoch: wire.ID(), Sequence: 1}
	}
	return r.activity
}

func (r *runtime) endActivityLocked() {
	r.activityLocked()
	r.activity.State = "unknown"
	r.activity.Sequence++
}

func (r *runtime) updateActivity(state, source, agent, foreground string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	previous := r.activityLocked()
	if r.exit != nil {
		state = "unknown"
	}
	if previous.State == state && previous.Source == source && previous.Agent == agent && previous.Foreground == foreground {
		return
	}
	r.activity = api.AgentActivity{State: state, Source: source, Agent: agent, Foreground: foreground, Epoch: previous.Epoch, Sequence: previous.Sequence + 1}
}

func (a *acpController) publishActivityLocked() {
	state := "unknown"
	switch {
	case len(a.permissions) > 0:
		state = "blocked"
	case a.state.Busy != "":
		state = "working"
	case a.state.Ready && a.state.Error == "":
		state = "idle"
	}
	var agent struct {
		Name string `json:"name"`
	}
	_ = json.Unmarshal(a.state.Agent, &agent)
	if len(agent.Name) > 120 || strings.ContainsFunc(agent.Name, unicode.IsControl) {
		agent.Name = ""
	}
	a.r.updateActivity(state, "acp", agent.Name, "")
}

// Foreground recognition identifies supported CLIs, not whether a task finished.
// Working/idle/blocked for a PTY requires the Agent's native integration.
func (r *runtime) observeForeground(command string) {
	if len(command) > 256 || strings.ContainsFunc(command, unicode.IsControl) {
		command = ""
	}
	agent := ""
	switch filepath.Base(command) {
	case "claude", "codex", "opencode", "gemini":
		agent = filepath.Base(command)
	}
	r.updateActivity("unknown", "foreground", agent, command)
}
