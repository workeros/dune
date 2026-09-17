package fabricd

import (
	"os"
	"strings"
	"testing"
)

func setPTYScreen(t *testing.T, r *runtime, record, screen string) {
	t.Helper()
	if err := os.WriteFile(record+".screen", []byte(screen), 0600); err != nil {
		t.Fatal(err)
	}
	awaitPTY(t, func() bool {
		content, err := r.tmux.LiveScreen()
		return err == nil && strings.TrimSpace(content) == screen
	})
}

func TestPTYScreenActivityWithoutViewer(t *testing.T) {
	r, _, record := ptyInputFixture(t)
	pane, err := r.tmux.Inspect()
	if err != nil {
		t.Fatal(err)
	}
	setPTYScreen(t, r, record, "• Working (12s • esc to interrupt)\n›\n? for shortcuts")
	r.observePTY(pane, false)
	working := r.info().Activity
	if working.State != "working" || working.Source != "screen" || working.Agent != "codex" || len(r.subs) != 0 {
		t.Fatal(working)
	}
	r.observePTY(pane, false)
	if got := r.info().Activity; got.Sequence != working.Sequence {
		t.Fatal("unchanged polling emitted activity", got)
	}
	setPTYScreen(t, r, record, "›\n? for shortcuts")
	r.observePTY(pane, true)
	idle := r.info().Activity
	if idle.State != "idle" || idle.Sequence <= working.Sequence || idle.Epoch != working.Epoch {
		t.Fatal(idle)
	}
	if err := r.tmux.History("older"); err != nil {
		t.Fatal(err)
	}
	r.observePTY(pane, true)
	if r.info().Activity.Sequence != idle.Sequence {
		t.Fatal("history browsing changed activity")
	}
	setPTYScreen(t, r, record, "unrecognized screen")
	r.observePTY(pane, true)
	if r.info().Activity.State != "unknown" {
		t.Fatal("unknown screen retained idle")
	}
	pane.Command = "sh"
	r.observePTY(pane, false)
	if got := r.info().Activity; got.State != "unknown" || got.Agent != "" || got.Source != "foreground" {
		t.Fatal("shell retained Agent activity", got)
	}
}
