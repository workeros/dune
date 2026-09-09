package tests

import (
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/aiomni/dune/internal/config"
)

func TestRemoteClientCommandsAreRemoved(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.yaml")
	must(t, config.Init(path, "127.0.0.1:7443"))
	for _, command := range []string{"capabilities", "profile", "runtime", "exec", "files", "upload", "git", "upload-file", "ports", "login", "logout", "machines", "runners"} {
		output, err := exec.Command(binary, "--config", path, command).CombinedOutput()
		if err == nil || !strings.Contains(string(output), "unknown command") {
			t.Fatalf("removed remote client command %q remained available: %v %s", command, err, output)
		}
	}
	for _, flag := range []string{"--login", "--target", "--runner"} {
		output, err := exec.Command(binary, flag, "removed").CombinedOutput()
		if err == nil || !strings.Contains(string(output), "flag provided but not defined") {
			t.Fatalf("removed remote client flag %q remained available: %v %s", flag, err, output)
		}
	}
	output, err := exec.Command(binary).CombinedOutput()
	if err == nil || !strings.Contains(string(output), "a Web or connector command is required") {
		t.Fatalf("bare dune still started the old supervisor: %v %s", err, output)
	}
}

func TestServiceCommandsRemainSeparate(t *testing.T) {
	output, err := exec.Command(binary, "help").CombinedOutput()
	if err != nil {
		t.Fatal(err)
	}
	help := string(output)
	for _, command := range []string{"gateway", "fabricd", "web"} {
		if !strings.Contains(help, " "+command) {
			t.Fatalf("separate %s service missing from help: %s", command, help)
		}
	}
	for _, command := range []string{" capabilities", " exec", " runtime", " login", " ports"} {
		if strings.Contains(help, command) {
			t.Fatalf("remote client command remained in help: %s", command)
		}
	}
}
