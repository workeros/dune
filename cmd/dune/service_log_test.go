package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/aiomni/dune/internal/lifecycle"
)

func TestServiceStartupFailuresHaveBoundedPrivateDiagnostics(t *testing.T) {
	dir := t.TempDir()
	if err := os.Chmod(dir, 0700); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "config.json")
	if err := os.WriteFile(path, []byte(`{"token":"private-startup-token","bad":`), 0600); err != nil {
		t.Fatal(err)
	}
	previous := os.Args
	t.Cleanup(func() { os.Args = previous })
	os.Args = []string{"dune", "--service-log-dir", dir, "--config", path, "fabricd"}
	for range 600 {
		if err := run(); err == nil {
			t.Fatal("invalid service configuration ran")
		}
	}
	data, err := os.ReadFile(filepath.Join(dir, "service-events.jsonl"))
	if err != nil || len(data) > lifecycle.MaxBytes || !strings.Contains(string(data), "log_window_reset") || strings.Contains(string(data), "private-startup-token") {
		t.Fatal("unbounded or private service diagnostic", len(data), err)
	}
	for _, line := range strings.Split(strings.TrimSpace(string(data)), "\n") {
		var event lifecycle.Entry
		if err := json.Unmarshal([]byte(line), &event); err != nil || event.Kind != "service_starting" && event.Kind != "service_exit" && event.Kind != "log_window_reset" {
			t.Fatal("invalid event", event, err)
		}
	}
}
