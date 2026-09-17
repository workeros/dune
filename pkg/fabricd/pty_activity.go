package fabricd

import (
	"path/filepath"
	"time"

	"github.com/aiomni/dune/internal/agentdetect"
	"github.com/aiomni/dune/internal/tmux"
)

// One probe per Runtime is shared by summary polling and prompt admission.
// Sampling never opens a terminal viewer or changes its size/history position.
func (r *runtime) observePTY(pane tmux.Pane, force bool) {
	r.ptyActivityMu.Lock()
	defer r.ptyActivityMu.Unlock()
	agent := filepath.Base(pane.Command)
	if pane.Dead || !agentdetect.SupportsScreen(agent) {
		r.observeForeground(pane.Command)
		r.ptyProbeAfter = time.Time{}
		return
	}
	if !force && time.Now().Before(r.ptyProbeAfter) && r.info().Activity.Foreground == pane.Command {
		return
	}
	content, err := r.tmux.LiveScreen()
	state := "unknown"
	if err == nil {
		state = agentdetect.Screen(agent, content)
	}
	r.updateActivity(state, "screen", agent, pane.Command)
	r.ptyProbeAfter = time.Now().Add(time.Second)
}
