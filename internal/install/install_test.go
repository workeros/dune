package install

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestInstallRejectsExistingOrAliasedLayoutWithoutRecreation(t *testing.T) {
	for _, kind := range []string{"current", "releases", "installation.json"} {
		t.Run(kind, func(t *testing.T) {
			root := t.TempDir()
			if err := os.Chmod(root, 0700); err != nil {
				t.Fatal(err)
			}
			state := filepath.Join(root, "state")
			if err := os.Mkdir(state, 0700); err != nil {
				t.Fatal(err)
			}
			configPath := filepath.Join(root, "config.yaml")
			body := "gateway: ws://127.0.0.1:7443/api/v1/ws/tunnel\ntarget: machine\ntoken: " + strings.Repeat("a", 64) + "\nsession_dir: " + state + "\nupgrade_control_url: https://example.test/control\n"
			if err := os.WriteFile(configPath, []byte(body), 0600); err != nil {
				t.Fatal(err)
			}
			outside := t.TempDir()
			if err := os.Symlink(outside, filepath.Join(root, kind)); err != nil {
				t.Fatal(err)
			}
			if err := Run(t.Context(), configPath, root, "fixture", "service"); err == nil {
				t.Fatal("unrecognized installation was replaced")
			}
			entries, err := os.ReadDir(outside)
			if err != nil || len(entries) != 0 {
				t.Fatal("wrote through aliased installation", entries, err)
			}
			after, err := os.ReadFile(configPath)
			if err != nil || string(after) != body {
				t.Fatal("configuration changed", err)
			}
		})
	}
}
