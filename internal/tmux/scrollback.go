package tmux

import (
	"bytes"
	"fmt"
	"strings"

	"github.com/aiomni/dune/pkg/api"
)

// Scrollback never attaches a client or changes the pane, its size or copy mode.
func (r *Session) Scrollback(limit int) (api.TerminalScrollback, error) {
	var snapshot api.TerminalScrollback
	if limit == 0 {
		limit = api.DefaultTerminalScrollbackLines
	}
	if limit < 1 || limit > api.MaxTerminalScrollbackLines {
		return snapshot, &api.Error{Code: "INVALID_ARGUMENT", Detail: "scrollback limit must be 1..10000 (0 uses 5000)"}
	}
	out := &scrollbackOutput{}
	// Both commands run in one tmux command queue. Evaluate the starting row
	// inside tmux so pane output/resize cannot race a separate metadata query.
	start := fmt.Sprintf("#{e|-:#{pane_height},%d}", limit)
	err := r.Server.runOutput(out,
		"display-message", "-p", "-t", r.pane(), "#{pane_height} #{pane_width} #{history_size} #{history_limit}", ";",
		"capture-pane", "-p", "-t", r.pane(), "-S", start, "-E", "-")
	if err != nil {
		return snapshot, err
	}
	var historyLimit int
	if !out.haveHeader {
		return snapshot, fmt.Errorf("tmux scrollback metadata missing")
	}
	if _, err := fmt.Sscanf(string(out.header), "%d %d %d %d", &snapshot.Rows, &snapshot.Cols, &snapshot.HistoryLines, &historyLimit); err != nil {
		return snapshot, fmt.Errorf("tmux scrollback metadata: %w", err)
	}
	content := out.content
	if out.partialRow {
		// Drop the oldest partial row rather than returning broken UTF-8 or
		// a row fragment. A single oversized row may leave an empty snapshot.
		if end := bytes.IndexByte(content, '\n'); end >= 0 {
			content = content[end+1:]
		} else {
			content = nil
		}
	}
	snapshot.Content = string(content)
	snapshot.CapturedLines = strings.Count(snapshot.Content, "\n")
	// tmux frees the oldest 10% in a batch. It exposes no lifetime eviction
	// counter, so flag the eviction region conservatively, including before
	// the first eviction. This is not a complete process-output log.
	evictionRegion := max(1, historyLimit-max(1, historyLimit/10))
	snapshot.Truncated = out.truncated || snapshot.HistoryLines+snapshot.Rows > limit || snapshot.HistoryLines >= evictionRegion
	return snapshot, nil
}

// Keep a bounded suffix while draining stdout, including captures whose raw
// bytes exceed Server.run's normal limit. The first line is trusted metadata.
type scrollbackOutput struct {
	header     []byte
	haveHeader bool
	content    []byte
	partialRow bool
	truncated  bool
}

func (b *scrollbackOutput) Write(p []byte) (int, error) {
	n := len(p)
	if !b.haveHeader {
		end := bytes.IndexByte(p, '\n')
		if end < 0 {
			end = len(p)
		}
		if len(b.header)+end > 128 {
			return 0, fmt.Errorf("tmux scrollback metadata exceeds limit")
		}
		b.header = append(b.header, p[:end]...)
		p = p[end:]
		if len(p) == 0 {
			return n, nil
		}
		b.haveHeader = true
		p = p[1:]
	}
	b.content = append(b.content, p...)
	if excess := len(b.content) - api.MaxTerminalScrollbackBytes; excess > 0 {
		b.partialRow = b.content[excess-1] != '\n'
		copy(b.content, b.content[excess:])
		b.content = b.content[:api.MaxTerminalScrollbackBytes]
		b.truncated = true
	}
	return n, nil
}
