package tests

import (
	"os/exec"
	"strings"
	"testing"
)

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
}
