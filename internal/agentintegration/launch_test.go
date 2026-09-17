package agentintegration

import (
	"encoding/json"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

func TestLaunchKeepsNativeSettingsAndStableHookDefinition(t *testing.T) {
	for _, agent := range []string{"claude", "codex"} {
		t.Run(agent, func(t *testing.T) {
			dir := t.TempDir()
			if err := os.Chmod(dir, 0700); err != nil {
				t.Fatal(err)
			}
			originalArgs := []string{filepath.Join("/usr/local/bin", agent)}
			originalEnv := []string{"CUSTOM=kept", SessionDirEnv + "=/wrong", HelperEnv + "=/wrong", "CODEX_THREAD_ID=parent-thread"}
			argv, env, err := Launch(dir, Binding{RuntimeID: "runtime", Incarnation: "boot", Agent: agent}, originalArgs, originalEnv, false)
			if err != nil {
				t.Fatal(err)
			}
			if len(originalArgs) != 1 || originalEnv[1] != SessionDirEnv+"=/wrong" {
				t.Fatal("mutated caller configuration")
			}
			if !slices.Contains(env, "CUSTOM=kept") || !slices.Contains(env, SessionDirEnv+"="+dir) || !slices.Contains(env, HelperEnv+"="+filepath.Join(dir, "helper")) || slices.Contains(env, HelperEnv+"=/wrong") {
				t.Fatal(env)
			}
			if slices.Contains(env, "CODEX_THREAD_ID=parent-thread") {
				t.Fatal("new Agent inherited the host's native identity")
			}
			if strings.Contains(strings.Join(argv, " "), "bypass") || strings.Contains(strings.Join(argv, " "), dir) {
				t.Fatal("trust bypass or variable hook definition", argv)
			}
			if agent == "claude" {
				if len(argv) != 3 || argv[1] != "--settings" {
					t.Fatal(argv)
				}
				var settings struct {
					Hooks map[string][]struct {
						Hooks []struct {
							Type, Command string
							Timeout       int
						}
					}
				}
				if err := json.Unmarshal([]byte(argv[2]), &settings); err != nil {
					t.Fatal(err)
				}
				hooks := settings.Hooks["SessionStart"]
				if len(settings.Hooks) != 1 || len(hooks) != 1 || len(hooks[0].Hooks) != 1 || hooks[0].Hooks[0].Type != "command" || hooks[0].Hooks[0].Command != `"$DUNE_AGENT_HELPER" __agent-session` {
					t.Fatal("invalid native hook definition", settings)
				}
			} else if len(argv) != 3 || argv[1] != "-c" || !strings.HasPrefix(argv[2], "hooks.SessionStart=[") {
				t.Fatal(argv)
			}
			info, err := os.Stat(filepath.Join(dir, "helper"))
			if err != nil || info.Mode().Perm()&0100 == 0 {
				t.Fatal("retained helper is not executable", err)
			}
		})
	}
}

func TestOnlyExplicitInteractiveLaunchesReceiveNativeHooks(t *testing.T) {
	for _, argv := range [][]string{nil, {"sh", "-c", "claude"}, {"codex", "exec", "task"}, {"claude", "--print", "task"}, {"claude", "--settings", "custom"}, {"gemini"}} {
		if Agent(argv) != "" {
			t.Fatal("opaque launch received integration", argv)
		}
	}
}
