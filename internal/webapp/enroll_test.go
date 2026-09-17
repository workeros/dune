package webapp

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/aiomni/dune/internal/config"
)

func TestEnrollmentExistingConfigNeverConsumesToken(t *testing.T) {
	var requests atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { requests.Add(1) }))
	defer server.Close()
	for _, kind := range []string{"valid", "corrupt", "dangling-link"} {
		t.Run(kind, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "config.yaml")
			switch kind {
			case "valid":
				if err := config.Create(path, config.Config{Gateway: "ws://localhost:7443/api/v1/ws/tunnel", Target: "existing", Token: strings.Repeat("a", 64)}); err != nil {
					t.Fatal(err)
				}
			case "corrupt":
				if err := os.WriteFile(path, []byte("bad: ["), 0600); err != nil {
					t.Fatal(err)
				}
			case "dangling-link":
				if err := os.Symlink(filepath.Join(t.TempDir(), "missing"), path); err != nil {
					t.Fatal(err)
				}
			}
			err := EnrollMachine(context.Background(), path, server.URL, strings.Repeat("t", 64), "", "stable-runner")
			if err == nil || !strings.Contains(err.Error(), "already exists") {
				t.Fatal(err)
			}
		})
	}
	if requests.Load() != 0 {
		t.Fatal("existing configuration consumed token")
	}
}
func TestEnrollmentUnknownDoesNotReplayOrClobber(t *testing.T) {
	for _, scenario := range []string{"lost-response", "local-race"} {
		t.Run(scenario, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "config.yaml")
			var requests atomic.Int32
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				requests.Add(1)
				if scenario == "lost-response" {
					w.Write([]byte("{"))
					return
				}
				if err := os.WriteFile(path, []byte("another installer owns this"), 0600); err != nil {
					t.Error(err)
				}
				json.NewEncoder(w).Encode(map[string]any{"machine": Machine{ID: "machine", RunnerID: "stable-runner"}, "credential": strings.Repeat("c", 64), "gateway": "ws://localhost:7443/api/v1/ws/tunnel"})
			}))
			defer server.Close()
			err := EnrollMachine(context.Background(), path, server.URL, strings.Repeat("t", 64), "", "stable-runner")
			if err == nil || !strings.Contains(err.Error(), "result unknown for Runner stable-runner") {
				t.Fatal(err)
			}
			if requests.Load() != 1 {
				t.Fatal("enrollment replayed")
			}
			if scenario == "local-race" {
				data, _ := os.ReadFile(path)
				if string(data) != "another installer owns this" {
					t.Fatal("overwrote raced config")
				}
			}
		})
	}
}
