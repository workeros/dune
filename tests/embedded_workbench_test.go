package tests

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/aiomni/dune/pkg/host"
)

// Build and run the published sample from an unrelated module, then reuse the
// full enrollment/terminal test against that process's actual HTTP handler.
func externalWorkbench(t *testing.T, ctx context.Context, address string, options host.Options) func() {
	t.Helper()
	repo, err := filepath.Abs("..")
	must(t, err)
	dir := t.TempDir()
	source, err := os.ReadFile(filepath.Join(repo, "samples/workbench/main.go"))
	must(t, err)
	must(t, os.WriteFile(filepath.Join(dir, "main.go"), source, 0600))
	module := fmt.Sprintf("module external.example/workbench\n\ngo 1.27.0\n\nrequire github.com/aiomni/dune v0.0.0\nreplace github.com/aiomni/dune => %q\n", repo)
	must(t, os.WriteFile(filepath.Join(dir, "go.mod"), []byte(module), 0600))
	binary := filepath.Join(dir, "host")
	build := exec.CommandContext(ctx, "go", "build", "-mod=mod", "-race", "-o", binary, ".")
	build.Dir = dir
	if output, err := build.CombinedOutput(); err != nil {
		t.Fatalf("external workbench build: %v\n%s", err, output)
	}
	assets := filepath.Join(dir, "assets")
	must(t, os.Mkdir(assets, 0700))
	must(t, os.WriteFile(filepath.Join(assets, "index.html"), []byte("external workbench asset"), 0600))
	log, err := os.Create(filepath.Join(dir, "host.log"))
	must(t, err)
	command := exec.CommandContext(ctx, binary, "--listen", address, "--url", options.PublicURL, "--data", options.DataDir, "--assets", assets)
	command.Stdout, command.Stderr = log, log
	must(t, command.Start())
	done := make(chan error, 1)
	go func() { done <- command.Wait() }()
	stopped := false
	stop := func() {
		if stopped {
			return
		}
		stopped = true
		command.Process.Signal(syscall.SIGTERM)
		select {
		case err := <-done:
			if err != nil {
				t.Errorf("external workbench exit: %v", err)
			}
		case <-time.After(5 * time.Second):
			command.Process.Kill()
			<-done
			t.Error("external workbench failed to close")
		}
		log.Close()
		output, _ := os.ReadFile(log.Name())
		if strings.Contains(string(output), "DATA RACE") {
			t.Errorf("external workbench race: %s", output)
		}
		if t.Failed() {
			t.Log(string(output))
		}
	}
	t.Cleanup(stop)
	client := &http.Client{Timeout: time.Second}
	for {
		response, err := client.Get("http://" + address + "/health")
		if err == nil {
			body, _ := io.ReadAll(response.Body)
			response.Body.Close()
			if response.StatusCode == 200 && string(body) == "host alive" {
				break
			}
		}
		select {
		case <-ctx.Done():
			t.Fatal("external workbench did not start")
		case <-time.After(25 * time.Millisecond):
		}
	}
	response, err := client.Get(options.PublicURL)
	must(t, err)
	body, err := io.ReadAll(response.Body)
	response.Body.Close()
	must(t, err)
	if response.StatusCode != 200 || string(body) != "external workbench asset" {
		t.Fatal("external host did not serve the mounted workbench")
	}
	return stop
}
