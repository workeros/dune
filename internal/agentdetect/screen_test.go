package agentdetect

import "testing"

func TestScreenControls(t *testing.T) {
	for _, tt := range []struct{ name, agent, screen, want string }{
		{"codex work", "codex", "• Working (12s • esc to interrupt)\n\n›\n\n? for shortcuts", "working"},
		{"codex work without prompt", "codex", "Working (1m 12s • esc to interrupt)", "working"},
		{"codex queued input", "codex", "• Working (12s • esc to interrupt)\n• Queued follow-up inputs\n›⠁Draft\n›\n? for shortcuts", "working"},
		{"codex new busy footer", "codex", "›\n? for shortcuts · esc to interrupt", "unknown"},
		{"codex permission", "codex", "› run a command\nWould you like to run it?\n1. Yes\n2. No\nPress enter to confirm or esc to cancel", "blocked"},
		{"codex question", "codex", "› plan\nChoose one\n1. A\n2. B\nenter to submit answer", "blocked"},
		{"codex trust", "codex", "> You are in /tmp\nDo you trust the contents of this directory?\n› 1. Yes\n2. No", "blocked"},
		{"codex update", "codex", "Update available!\n1. Update now\n2. Skip\nPress enter to continue", "blocked"},
		{"codex idle", "codex", "• Answer\n\n› Summarize this project\n\n90% context left", "idle"},
		{"codex stale permission", "codex", "Press enter to confirm or esc to cancel\n• Done\n›\n? for shortcuts", "idle"},
		{"codex stale working", "codex", "• Working (12s • esc to interrupt)\n• Done\n›\n? for shortcuts", "idle"},
		{"codex stale prompt", "codex", "› request\n• Answer in progress\n? for shortcuts", "unknown"},
		{"codex transcript", "codex", "›\n? for shortcuts\n↑/↓ to scroll q to quit", "unknown"},
		{"codex draft", "codex", "› press enter to confirm or esc to cancel\n? for shortcuts", "idle"},
		{"claude idle", "claude", "Done\n──────────\n❯\n──────────\n? for shortcuts", "idle"},
		{"claude older prompt", "claude", "──────────\n> Draft\n──────────\n? for shortcuts", "idle"},
		{"claude work", "claude", "✻ Thinking… (12s · ↓ 240 tokens)\n──────────\n❯\n──────────\n? for shortcuts", "working"},
		{"claude work without timer", "claude", "✻ Thinking…\n──────────\n❯\n──────────", "working"},
		{"claude permission", "claude", "──────────\nBash command\nDo you want to proceed?\n❯ 1. Yes\n2. No\nEsc to cancel", "blocked"},
		{"claude form", "claude", "──────────\nYour answer\nEnter to select · Arrow keys to navigate · Esc to cancel", "blocked"},
		{"claude old permission", "claude", "Do you want to proceed?\n❯ 1. Yes\nEsc to cancel\nDone\n──────────\n❯\n──────────\n? for shortcuts", "idle"},
		{"claude draft", "claude", "──────────\n❯ explain this message:\nDo you want to proceed?\n1. Yes\nEsc to cancel\n──────────\n? for shortcuts", "idle"},
		{"claude old timer", "claude", "✻ Thinking…\nDone\n──────────\n❯\n──────────", "idle"},
		{"claude transcript", "claude", "──────────\n❯\n──────────\nShowing detailed transcript · ctrl+o to toggle", "unknown"},
		{"claude picker", "claude", "──────────\n❯ Model\n──────────\nEnter to set as default · Esc to cancel", "unknown"},
		{"claude incomplete box", "claude", "──────────\n❯", "unknown"},
		{"unknown CLI", "gemini", "›\n? for shortcuts", "unknown"},
		{"known CLI unknown screen", "codex", "the process is still running", "unknown"},
		{"empty screen", "claude", "\n ", "unknown"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			if got := Screen(tt.agent, tt.screen); got != tt.want {
				t.Fatalf("got %s, want %s; screen: %q", got, tt.want, tt.screen)
			}
		})
	}
}
