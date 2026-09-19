package tmux

import (
	"fmt"
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/aiomni/dune/pkg/api"
)

func TestScrollbackRetainedExitAndReadOnly(t *testing.T) {
	s := server(t)
	r := session(t, s, "i", `seq 1 80; printf '\033[31m中文🙂\033[0m\033]8;;https://example.com\007LINK\033]8;;\007\n'; printf '%s\n' '`+strings.Repeat("长", 100)+`'; exit 7`, []string{"PATH=/usr/bin:/bin", "LANG=en_US.UTF-8"})
	await(t, func() bool { p, err := r.Inspect(); return err == nil && p.Dead })
	for _, mode := range []string{"close", "older"} {
		if err := r.History(mode); err != nil {
			t.Fatal(err)
		}
		state := func() string {
			t.Helper()
			out, err := s.run("display-message", "-p", "-t", r.pane(), "#{pane_pid} #{pane_dead} #{pane_dead_status} #{pane_width} #{pane_height} #{history_size} #{pane_in_mode} #{scroll_position} #{cursor_x} #{cursor_y} #{session_attached}")
			if err != nil {
				t.Fatal(err)
			}
			return out
		}
		before := state()
		want, err := s.run("capture-pane", "-p", "-t", r.pane(), "-S", "-", "-E", "-")
		if err != nil {
			t.Fatal(err)
		}
		got, err := r.Scrollback(0)
		if err != nil || got.Content != want || got.Truncated || got.Cols != 80 || got.Rows != 24 || got.HistoryLines == 0 || got.CapturedLines != got.HistoryLines+got.Rows {
			t.Fatalf("snapshot: %+v, %v", got, err)
		}
		if strings.ContainsAny(got.Content, "\x1b\x07") || !utf8.ValidString(got.Content) || !strings.Contains(got.Content, "中文🙂LINK") || strings.Count(got.Content, "长") != 100 {
			t.Fatalf("text was corrupted: %q", got.Content)
		}
		for _, limit := range []int{1, 10, 24, 30} {
			limited, err := r.Scrollback(limit)
			if err != nil || limited.CapturedLines != limit || !limited.Truncated || !strings.HasSuffix(got.Content, limited.Content) {
				t.Fatalf("limit %d: %+v %v", limit, limited, err)
			}
		}
		if after := state(); before != after {
			t.Fatalf("read changed pane: %q -> %q", before, after)
		}
	}
	for _, limit := range []int{-1, api.MaxTerminalScrollbackLines + 1} {
		if _, err := r.Scrollback(limit); err == nil {
			t.Fatalf("accepted limit %d", limit)
		}
	}
	if err := r.Destroy(); err != nil {
		t.Fatal(err)
	}
	if _, err := r.Scrollback(0); err == nil {
		t.Fatal("read destroyed pane")
	}
}

func TestScrollbackLimitsAndEviction(t *testing.T) {
	s := server(t)
	r := session(t, s, "j", `seq 1 10200; exit`, []string{"PATH=/usr/bin:/bin"})
	await(t, func() bool { p, err := r.Inspect(); return err == nil && p.Dead })
	for _, limit := range []int{0, api.MaxTerminalScrollbackLines} {
		got, err := r.Scrollback(limit)
		wantLines := limit
		if wantLines == 0 {
			wantLines = api.DefaultTerminalScrollbackLines
		}
		if err != nil || !got.Truncated || got.CapturedLines != wantLines || !strings.Contains(got.Content, "10200\n") {
			t.Fatalf("limit %d: lines=%d truncated=%v err=%v", limit, got.CapturedLines, got.Truncated, err)
		}
	}
	meta := r.Runtime
	meta.ID = strings.Repeat("k", 32)
	evicted, err := s.Create(meta, []string{"/bin/sh", "-c", "seq 1 350; exit"}, []string{"PATH=/usr/bin:/bin"}, CreateOptions{HistoryLines: 100})
	if err != nil {
		t.Fatal(err)
	}
	await(t, func() bool { p, err := evicted.Inspect(); return err == nil && p.Dead })
	got, err := evicted.Scrollback(0)
	if err != nil || !got.Truncated || got.HistoryLines >= 100 || got.CapturedLines != got.HistoryLines+got.Rows || strings.HasPrefix(got.Content, "1\n") {
		t.Fatalf("batch eviction not indicated: %+v %v", got, err)
	}
}

func TestScrollbackByteLimit(t *testing.T) {
	s := server(t)
	// At 400 columns these rows exceed both the public byte bound and run's
	// ordinary 2 MiB bound. The capture must drain and retain the newest rows.
	row := strings.Repeat("界", 199)
	r := session(t, s, "l", fmt.Sprintf(`read -r ready; i=0; while [ "$i" -lt 6000 ]; do printf '%%s\n' '%s'; i=$((i+1)); done; printf 'NEWEST🙂\n'; exit`, row), []string{"PATH=/usr/bin:/bin", "LANG=en_US.UTF-8"})
	if _, err := s.run("resize-window", "-t", r.target(), "-x", "400", "-y", "24", ";", "send-keys", "-t", r.pane(), "Enter"); err != nil {
		t.Fatal(err)
	}
	await(t, func() bool { p, err := r.Inspect(); return err == nil && p.Dead })
	got, err := r.Scrollback(api.MaxTerminalScrollbackLines)
	if err != nil || !got.Truncated || len(got.Content) > api.MaxTerminalScrollbackBytes || !utf8.ValidString(got.Content) || !strings.Contains(got.Content, "NEWEST🙂\n") || got.CapturedLines != strings.Count(got.Content, "\n") {
		t.Fatalf("byte limit: bytes=%d lines=%d truncated=%v err=%v", len(got.Content), got.CapturedLines, got.Truncated, err)
	}
	if !strings.HasPrefix(got.Content, row+"\n") || len(got.Content) < api.MaxTerminalScrollbackBytes-len(row)-1 {
		t.Fatal("did not retain the newest complete rows")
	}
}

func TestScrollbackAlternateScreen(t *testing.T) {
	s := server(t)
	r := session(t, s, "m", `seq 1 80; printf 'MAIN中文\n'; read -r a; printf '\033[?1049h\033[HALTERNATE🙂'; read -r a; printf '\033[?1049l'; sleep 120`, []string{"PATH=/usr/bin:/bin", "LANG=en_US.UTF-8"})
	await(t, func() bool { c, err := r.Capture(); return err == nil && strings.Contains(c.Content, "MAIN中文") })
	for _, active := range []bool{true, false} {
		if _, err := s.run("send-keys", "-t", r.pane(), "Enter"); err != nil {
			t.Fatal(err)
		}
		await(t, func() bool {
			c, err := r.Capture()
			return err == nil && strings.Contains(c.Content, "ALTERNATE🙂") == active && strings.Contains(c.Content, "MAIN中文") != active
		})
		got, err := r.Scrollback(0)
		if err != nil || got.HistoryLines == 0 || !strings.Contains(got.Content, "1\n") || strings.Contains(got.Content, "ALTERNATE🙂") != active || strings.Contains(got.Content, "MAIN中文") == active {
			t.Fatalf("alternate=%v: %+v %v", active, got, err)
		}
		mode, err := s.run("display-message", "-p", "-t", r.pane(), "#{alternate_on} #{pane_in_mode} #{session_attached}")
		want := "0 0 0\n"
		if active {
			want = "1 0 0\n"
		}
		if err != nil || mode != want {
			t.Fatal("snapshot changed modes or attached a viewer", mode, err)
		}
	}
}
