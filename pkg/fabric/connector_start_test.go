package fabric

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func TestConnectorStartRepairsOriginalRegisteredServices(t *testing.T) {
	for _, valid := range []bool{true, false} {
		t.Run(map[bool]string{true: "original", false: "other-bootstrap"}[valid], func(t *testing.T) {
			root := t.TempDir()
			recovery := filepath.Join(root, "installation", "recovery")
			if err := os.MkdirAll(recovery, 0700); err != nil {
				t.Fatal(err)
			}
			marker := "original\nversion\n"
			if !valid {
				marker = "another-action\nversion\n"
			}
			if err := os.WriteFile(filepath.Join(root, "bootstrap.complete"), []byte(marker), 0600); err != nil {
				t.Fatal(err)
			}
			outputPath := filepath.Join(root, "repair-arguments")
			script := "#!/bin/sh\nprintf '%s\\n' \"$@\" > " + shellQuote(outputPath) + "\n"
			if err := os.WriteFile(filepath.Join(recovery, "dune"), []byte(script), 0700); err != nil {
				t.Fatal(err)
			}
			plan := newConnectorStartPlan(root, "original")
			output, err := exec.Command("/bin/sh", "-c", plan.Script).CombinedOutput()
			if !valid {
				if err == nil || !strings.Contains(string(output), "identity mismatch") {
					t.Fatal("foreign bootstrap repaired", err, string(output))
				}
				if _, err := os.Stat(outputPath); !os.IsNotExist(err) {
					t.Fatal("foreign bootstrap reached repair")
				}
				return
			}
			if err != nil {
				t.Fatal(err, string(output))
			}
			arguments, err := os.ReadFile(outputPath)
			if err != nil || string(arguments) != "repair-services\n--root\n"+filepath.Join(root, "installation")+"\n" {
				t.Fatal("repair did not use original installation", string(arguments), err)
			}
		})
	}
	plan, err := NewConnectorStartPlan("original")
	if err != nil {
		t.Fatal(err)
	}
	completion, err := BootstrapCompletionPath("original")
	if err != nil || plan.Root != filepath.Dir(completion) {
		t.Fatal(plan, err)
	}
	if _, err := NewConnectorStartPlan(""); err == nil {
		t.Fatal("missing bootstrap identity accepted")
	}
}
