package fabric

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"
)

func TestConnectorStartPlanUsesOriginalBootstrapIdentity(t *testing.T) {
	plan, err := NewConnectorStartPlan("original bootstrap")
	if err != nil {
		t.Fatal(err)
	}
	completion, err := BootstrapCompletionPath("original bootstrap")
	if err != nil || plan.Root != filepath.Dir(completion) {
		t.Fatalf("unexpected installation root: %+v, %v", plan, err)
	}
	if strings.Contains(plan.Script, "enroll") || strings.Contains(plan.Script, "curl") || strings.Contains(plan.Script, "--token") {
		t.Fatal("restart plan contains enrollment or download behavior")
	}
	if _, err := NewConnectorStartPlan(""); err == nil {
		t.Fatal("missing original action ID accepted")
	}
}

func TestConnectorStartProcessLifecycle(t *testing.T) {
	if runtime.GOOS != "darwin" && runtime.GOOS != "linux" {
		t.Skip("connector start supports Linux and macOS")
	}
	t.Setenv("COLUMNS", "80")
	root := t.TempDir()
	actionID := "original bootstrap"
	installFakeConnector(t, root, actionID)
	plan := newConnectorStartPlan(root, actionID)
	defer stopFakeConnectors(root)

	// Concurrent first calls launch one real process that outlives the shells.
	runConcurrentConnectorPlans(t, plan)
	first := waitFakeConnectorPIDs(t, root, 1)
	if len(first) != 1 || !processExists(first[0]) {
		t.Fatalf("connector did not survive command exit: %v", first)
	}
	runConnectorPlan(t, plan)
	if got := fakeConnectorPIDs(t, root); len(got) != 1 {
		t.Fatalf("sequential retry launched another connector: %v", got)
	}

	runConcurrentConnectorPlans(t, plan)
	if got := fakeConnectorPIDs(t, root); len(got) != 1 || got[0] != first[0] {
		t.Fatalf("concurrent retry replaced or duplicated connector: %v", got)
	}

	if err := syscall.Kill(first[0], syscall.SIGTERM); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(5 * time.Second)
	for processExists(first[0]) && time.Now().Before(deadline) {
		time.Sleep(20 * time.Millisecond)
	}
	if processExists(first[0]) {
		t.Fatal("fake connector did not exit")
	}
	runConnectorPlan(t, plan)
	second := waitFakeConnectorPIDs(t, root, 2)
	if len(second) != 2 || second[1] == first[0] || !processExists(second[1]) {
		t.Fatalf("connector did not restart after process loss: %v", second)
	}
	for name, expected := range map[string]string{
		"bootstrap.complete": actionID + "\ntest-version\n",
		"config.yaml":        "test-config\n",
		"sessions/keep":      "persistent-data\n",
	} {
		content, err := os.ReadFile(filepath.Join(root, name))
		if err != nil || string(content) != expected {
			t.Fatalf("%s changed: %q, %v", name, content, err)
		}
	}
}

func TestConnectorStartRejectsInvalidInstallationAndExit(t *testing.T) {
	if runtime.GOOS != "darwin" && runtime.GOOS != "linux" {
		t.Skip("connector start supports Linux and macOS")
	}
	missingRoot := filepath.Join(t.TempDir(), "absent")
	output, err := exec.Command("/bin/sh", "-c", newConnectorStartPlan(missingRoot, "original-bootstrap").Script).CombinedOutput()
	if err == nil || !strings.Contains(string(output), "connector installation missing: "+missingRoot) {
		t.Fatalf("missing installation was not reported: %v, %s", err, output)
	}
	for _, tc := range []struct {
		name, remove, marker, config, want string
	}{
		{name: "missing marker", remove: "bootstrap.complete", want: "bootstrap marker missing"},
		{name: "missing config", remove: "config.yaml", want: "config missing"},
		{name: "missing executable", remove: "dune", want: "executable missing"},
		{name: "wrong bootstrap", marker: "another action\ntest-version\n", want: "identity mismatch"},
		{name: "missing version", marker: "original-bootstrap\n", want: "marker has no version"},
		{name: "immediate exit", config: "exit\n", want: "exited during startup"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			root := t.TempDir()
			installFakeConnector(t, root, "original-bootstrap")
			if tc.remove != "" {
				if err := os.Remove(filepath.Join(root, tc.remove)); err != nil {
					t.Fatal(err)
				}
			}
			if tc.marker != "" {
				if err := os.WriteFile(filepath.Join(root, "bootstrap.complete"), []byte(tc.marker), 0600); err != nil {
					t.Fatal(err)
				}
			}
			if tc.config != "" {
				if err := os.WriteFile(filepath.Join(root, "config.yaml"), []byte(tc.config), 0600); err != nil {
					t.Fatal(err)
				}
			}
			output, err := exec.Command("/bin/sh", "-c", newConnectorStartPlan(root, "original-bootstrap").Script).CombinedOutput()
			if err == nil || !strings.Contains(string(output), tc.want) || !strings.Contains(string(output), filepath.Join(root, "fabricd.log")) {
				t.Fatalf("unexpected result: %v, %s", err, output)
			}
			if got := fakeConnectorPIDs(t, root); len(got) > 0 && tc.name != "immediate exit" {
				t.Fatalf("invalid installation launched process: %v", got)
			}
		})
	}
}

func installFakeConnector(t *testing.T, root, actionID string) {
	t.Helper()
	source := `package main
import ("fmt"; "os"; "strconv"; "strings"; "time")
func main() {
 b, _ := os.ReadFile(os.Args[2]); if strings.TrimSpace(string(b)) == "exit" { os.Exit(7) }
 f, err := os.OpenFile(strings.TrimSuffix(os.Args[2], "config.yaml")+"starts", os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0600); if err != nil { panic(err) }
 fmt.Fprintln(f, strconv.Itoa(os.Getpid())); f.Close()
 for { time.Sleep(time.Hour) }
}`
	sourcePath := filepath.Join(t.TempDir(), "fake.go")
	if err := os.WriteFile(sourcePath, []byte(source), 0600); err != nil {
		t.Fatal(err)
	}
	output, err := exec.Command("go", "build", "-o", filepath.Join(root, "dune"), sourcePath).CombinedOutput()
	if err != nil {
		t.Fatalf("building fake connector: %v: %s", err, output)
	}
	writeBootstrapMarkerFromPlan(t, root, actionID)
	if err := os.WriteFile(filepath.Join(root, "config.yaml"), []byte("test-config\n"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(filepath.Join(root, "sessions"), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "sessions", "keep"), []byte("persistent-data\n"), 0600); err != nil {
		t.Fatal(err)
	}
}

func writeBootstrapMarkerFromPlan(t *testing.T, root, actionID string) {
	t.Helper()
	call := bootstrapCall()
	call.Action.ID = actionID
	call.Version = "test-version"
	plan, err := NewBootstrapPlan(call, BootstrapPlatform{OS: runtime.GOOS, Arch: runtime.GOARCH})
	if err != nil {
		t.Fatal(err)
	}
	var markerCommands []string
	for _, line := range strings.Split(plan.Script, "\n") {
		if strings.Contains(line, "bootstrap.complete.tmp") {
			markerCommands = append(markerCommands, line)
		}
	}
	if len(markerCommands) != 2 {
		t.Fatalf("bootstrap marker commands changed: %v", markerCommands)
	}
	script := "set -eu\nroot=" + shellQuote(root) + "\n" + strings.Join(markerCommands, "\n")
	if output, err := exec.Command("/bin/sh", "-c", script).CombinedOutput(); err != nil {
		t.Fatalf("writing bootstrap marker: %v: %s", err, output)
	}
}

func runConnectorPlan(t *testing.T, plan ConnectorStartPlan) {
	t.Helper()
	output, err := exec.Command("/bin/sh", "-c", plan.Script).CombinedOutput()
	if err != nil {
		t.Fatalf("connector start failed: %v: %s", err, output)
	}
}

func runConcurrentConnectorPlans(t *testing.T, plan ConnectorStartPlan) {
	t.Helper()
	var wg sync.WaitGroup
	errs := make(chan error, 6)
	for i := 0; i < 6; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			output, err := exec.Command("/bin/sh", "-c", plan.Script).CombinedOutput()
			if err != nil {
				errs <- fmt.Errorf("%w: %s", err, output)
			}
		}()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Error(err)
	}
}

func fakeConnectorPIDs(t *testing.T, root string) []int {
	t.Helper()
	content, err := os.ReadFile(filepath.Join(root, "starts"))
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		t.Fatal(err)
	}
	var pids []int
	for _, line := range strings.Fields(string(content)) {
		pid, err := strconv.Atoi(line)
		if err != nil {
			t.Fatal(err)
		}
		pids = append(pids, pid)
	}
	return pids
}

func waitFakeConnectorPIDs(t *testing.T, root string, count int) []int {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for {
		pids := fakeConnectorPIDs(t, root)
		if len(pids) >= count || time.Now().After(deadline) {
			return pids
		}
		time.Sleep(20 * time.Millisecond)
	}
}

func stopFakeConnectors(root string) {
	content, _ := os.ReadFile(filepath.Join(root, "starts"))
	for _, line := range strings.Fields(string(content)) {
		pid, err := strconv.Atoi(line)
		if err == nil {
			_ = syscall.Kill(pid, syscall.SIGTERM)
		}
	}
}

func processExists(pid int) bool { return syscall.Kill(pid, 0) == nil }
