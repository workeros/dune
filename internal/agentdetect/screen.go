// Package agentdetect recognizes a small set of native CLI screen controls.
// These are presentation hints, never proof that a particular task completed.
package agentdetect

import (
	"regexp"
	"strings"
	"unicode"
)

// SupportsScreen deliberately differs from foreground recognition: identifying
// an executable does not imply that we understand its current UI.
func SupportsScreen(agent string) bool { return agent == "claude" || agent == "codex" }

// Screen reads the live, unstyled tmux screen, not scrollback or a viewer's copy
// mode. Restrict controls to the current input area and recent footer so old
// approval questions and quoted output do not become current blockers.
func Screen(agent, content string) string {
	if !SupportsScreen(agent) || len(content) > 256*1024 {
		return "unknown"
	}
	lines := strings.Split(content, "\n")
	if len(lines) > 40 {
		lines = lines[len(lines)-40:]
	}
	for i := range lines {
		lines[i] = strings.TrimSpace(lines[i])
	}
	lines = nonempty(lines)
	if len(lines) == 0 {
		return "unknown"
	}
	if agent == "codex" {
		return codex(lines)
	}
	return claude(lines)
}

var codexWorking = regexp.MustCompile(`^(?:[•◦] )?[^›■✗✓].*\((?:[0-9]+[hm] )*[0-9]+s • esc to interrupt\)(?: · .*)?$`)

func codex(lines []string) string {
	prompt := -1
	for i, line := range lines {
		if (line == "›" || strings.HasPrefix(line, "› ")) && !choice.MatchString(line) {
			prompt = i
		} else if strings.ContainsRune("•■✗✓", firstRune(line)) {
			prompt = -1 // A response below a prompt makes that prompt historical.
		}
	}
	controls := lines
	if prompt >= 0 {
		controls = lines[prompt+1:]
	}
	footer := strings.ToLower(strings.Join(tail(controls, 4), "\n"))
	if strings.Contains(footer, "q to quit") || strings.Contains(footer, "esc to edit prev") || strings.Contains(footer, "esc/← to edit prev") {
		return "unknown"
	}
	for _, hint := range []string{"press enter to confirm or esc to cancel", "enter to submit answer", "enter to submit all"} {
		if strings.Contains(footer, hint) {
			return "blocked"
		}
	}
	if prompt < 0 {
		text := strings.ToLower(strings.Join(lines, "\n"))
		if strings.Contains(text, "do you trust the contents of this directory?") && numberedChoice(lines) {
			return "blocked"
		}
		if strings.Contains(text, "update available!") && strings.Contains(footer, "press enter to continue") {
			return "blocked"
		}
	}
	before := lines
	if prompt >= 0 {
		before = lines[:prompt]
	}
	// Queued drafts can sit below the live timer. A later response marker
	// ends that timer's relevance; an arbitrary old timer is not live evidence.
	for i := len(before) - 1; i >= 0 && i >= len(before)-8; i-- {
		if codexWorking.MatchString(before[i]) {
			return "working"
		}
		if strings.HasPrefix(before[i], "• Queued") || strings.HasPrefix(before[i], "• Messages") {
			continue
		}
		if strings.ContainsRune("•■✗✓", firstRune(before[i])) {
			break
		}
	}
	if strings.Contains(footer, "esc to interrupt") {
		return "unknown"
	}
	if prompt >= 0 && (strings.Contains(footer, "? for shortcuts") || strings.Contains(footer, "context left")) {
		return "idle"
	}
	return "unknown"
}

var claudeWorking = regexp.MustCompile(`^[*·✢✳✶✻✽] \S.*…(?: \([0-9]+[smh](?:[ ·].*)?\))?$`)
var choice = regexp.MustCompile(`^[❯›>]?\s*[1-9]\.\s+\S`)

func claude(lines []string) string {
	footer := strings.ToLower(strings.Join(tail(lines, 4), "\n"))
	if strings.Contains(footer, "showing detailed transcript") || strings.Contains(footer, "enter to set as default") {
		return "unknown"
	}
	// Claude's input box has two horizontal borders. Looking inside that box
	// excludes output and draft text when interpreting the control footer.
	bottom, top := -1, -1
	for i := len(lines) - 1; i >= 0; i-- {
		if horizontal(lines[i]) {
			if bottom < 0 {
				bottom = i
			} else {
				top = i
				break
			}
		}
	}
	controls := lines
	if bottom >= 0 {
		controls = lines[bottom+1:]
	}
	footer = strings.ToLower(strings.Join(tail(controls, 4), "\n"))
	if strings.Contains(footer, "esc to cancel") && (strings.Contains(footer, "enter to confirm") || strings.Contains(footer, "enter to select") || numberedChoice(controls)) {
		return "blocked"
	}
	before := lines
	if top >= 0 {
		before = lines[:top]
	}
	if len(before) > 0 {
		line := before[len(before)-1]
		if claudeWorking.MatchString(line) || (strings.Contains(line, "esc to interrupt") && strings.ContainsRune("⏸⏵", firstRune(line))) {
			return "working"
		}
	}
	if top >= 0 && bottom > top+1 && (strings.HasPrefix(lines[top+1], "❯") || lines[top+1] == ">" || strings.HasPrefix(lines[top+1], "> ")) {
		if strings.Contains(footer, "esc to cancel") || strings.Contains(footer, "enter to select") || strings.Contains(footer, "esc to interrupt") {
			return "unknown"
		}
		return "idle"
	}
	return "unknown"
}

func numberedChoice(lines []string) bool {
	for _, line := range lines {
		if choice.MatchString(line) {
			return true
		}
	}
	return false
}

func firstRune(s string) rune {
	for _, r := range s {
		return r
	}
	return 0
}

func horizontal(line string) bool {
	return len([]rune(line)) >= 8 && strings.TrimFunc(line, func(r rune) bool { return r == '─' || r == '━' || unicode.IsSpace(r) }) == ""
}

func nonempty(lines []string) []string {
	result := lines[:0]
	for _, line := range lines {
		if line != "" {
			result = append(result, line)
		}
	}
	return result
}

func tail(lines []string, count int) []string {
	if len(lines) > count {
		return lines[len(lines)-count:]
	}
	return lines
}
