package agentintegration

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"syscall"

	"github.com/aiomni/dune/internal/mcpbridge"
)

const HelperEnv = "DUNE_AGENT_HELPER"

// Agent supports unambiguous interactive launches and exact native resumes.
// Other arguments may encode a one-shot task or change native config roots.
func Agent(argv []string) string {
	if len(argv) == 0 {
		return ""
	}
	switch agent := filepath.Base(argv[0]); agent {
	case "claude", "codex":
		if len(argv) == 1 {
			return agent
		}
		if len(argv) == 3 && validSessionID(argv[2]) && (agent == "claude" && argv[1] == "--resume" || agent == "codex" && argv[1] == "resume") {
			return agent
		}
		return ""
	default:
		return ""
	}
}

// Launch adds Dune's SessionStart hook and optional MCP bridge. It does not edit
// user/project settings or bypass the native CLI's trust policy.
func Launch(dir string, binding Binding, argv, env []string, requireMCP bool) ([]string, []string, error) {
	if Agent(argv) != binding.Agent {
		return nil, nil, fmt.Errorf("native integration requires a supported interactive launch")
	}
	if err := Prepare(dir, binding); err != nil {
		return nil, nil, err
	}
	helper := filepath.Join(dir, "helper")
	if err := retainExecutable(helper); err != nil {
		return nil, nil, err
	}
	// The definition is identical across launches; only the environment changes.
	// Native hook trust can therefore refer to this exact stable definition.
	command := `"$` + HelperEnv + `" ` + SessionCommand
	argv = slices.Clone(argv)
	if binding.Agent == "claude" {
		hooks := map[string]any{"hooks": map[string]any{"SessionStart": []any{map[string]any{"hooks": []any{map[string]any{"type": "command", "command": command, "timeout": 2}}}}}}
		settings, _ := json.Marshal(hooks)
		argv = append(argv, "--settings", string(settings))
	} else {
		value := "hooks.SessionStart=[{hooks=[{type=\"command\",command=" + strconv.Quote(command) + ",timeout=2}]}]"
		argv = append(argv, "-c", value)
	}
	if requireMCP {
		var err error
		argv, err = prepareMCP(dir, binding.Agent, argv)
		if err != nil {
			return nil, nil, err
		}
	}
	resultEnv := make([]string, 0, len(env)+2)
	for _, value := range env {
		if !strings.HasPrefix(value, SessionDirEnv+"=") && !strings.HasPrefix(value, HelperEnv+"=") && !strings.HasPrefix(value, mcpbridge.URLEnv+"=") && !strings.HasPrefix(value, mcpbridge.TokenEnv+"=") {
			resultEnv = append(resultEnv, value)
		}
	}
	resultEnv = append(resultEnv, SessionDirEnv+"="+dir, HelperEnv+"="+helper)
	return argv, resultEnv, nil
}

// A hard link keeps the helper callable after installer cleanup unlinks the old
// release. Copy only when the session directory cannot hard-link that binary.
func retainExecutable(destination string) error {
	program, err := os.Executable()
	if err != nil {
		return err
	}
	if err := os.Link(program, destination); err == nil {
		return nil
	} else if !errors.Is(err, syscall.EXDEV) && !errors.Is(err, syscall.EPERM) {
		return err
	}
	source, err := os.Open(program)
	if err != nil {
		return err
	}
	defer source.Close()
	target, err := os.OpenFile(destination, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0700)
	if err != nil {
		return err
	}
	_, err = io.Copy(target, source)
	return errors.Join(err, target.Close())
}
