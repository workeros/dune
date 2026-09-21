package fabricd

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/aiomni/dune/internal/sessionregistry"
	"github.com/aiomni/dune/internal/tmux"
	"github.com/aiomni/dune/internal/wire"
	"github.com/aiomni/dune/pkg/api"
)

func TestDiscoveryClassifiesPrivateArtifactsWithoutAdoptionOrCleanup(t *testing.T) {
	mock := mockACPBinary(t)
	h := newCleanupProcessHarness(t)
	h.start("")
	work := t.TempDir()
	runtime, stream, err := testStartProfile(h.client, h.ctx, api.Profile{Version: 1, Kind: "agent", Adapter: "acp", ManagedACP: true, WorkingDirectory: work, Start: api.Command{Argv: []string{mock}}, Env: map[string]string{"DUNE_MOCK_PROCESS_LOG": filepath.Join(work, "process.log"), "DUNE_MOCK_RPC_LOG": filepath.Join(work, "rpc.log")}})
	if err != nil {
		t.Fatal(err)
	}
	stream.Close()
	waitTimeoutTest(t, func() bool { state, err := h.client.ACPState(h.ctx, runtime); return err == nil && state.Ready })
	registry, err := sessionregistry.Open(h.ctx, filepath.Join(h.state, "registry"), sessionregistry.Options{})
	if err != nil {
		t.Fatal(err)
	}
	defer registry.Close()
	host, err := registry.Host(h.ctx, cleanupTestKey(runtime, "read").Target)
	if err != nil {
		t.Fatal(err)
	}
	directory := host.Resources.Directory.Path
	registration, err := os.ReadFile(filepath.Join(directory, "registration.json"))
	if err != nil {
		t.Fatal(err)
	}
	check := func(codes ...string) {
		t.Helper()
		waitTimeoutTest(t, func() bool {
			page, err := h.client.List(h.ctx)
			if err != nil || page.Complete || len(page.Items) != 1 || page.Items[0].ID != runtime.ID || page.Items[0].Availability != "" {
				return false
			}
			found := make(map[string]bool)
			for _, issue := range page.Issues {
				found[issue.Code] = true
			}
			for _, code := range codes {
				if !found[code] {
					return false
				}
			}
			body := string(api.Payload(page))
			if strings.Contains(body, "private-placeholder") {
				t.Error("private artifact name/content exposed")
			}
			return true
		})
		if state, err := h.client.ACPState(h.ctx, runtime); err != nil || !state.Ready {
			t.Fatal("diagnostic artifact disabled the verified host", state, err)
		}
	}
	if err := os.Remove(filepath.Join(directory, "registration.json")); err != nil {
		t.Fatal(err)
	}
	check("REGISTRATION_FILE_MISSING")
	if err := os.WriteFile(filepath.Join(directory, "registration.json"), []byte("private-placeholder"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(directory, ".pending-private-placeholder"), []byte("private-placeholder"), 0600); err != nil {
		t.Fatal(err)
	}
	check("REGISTRATION_FILE_INVALID", "REGISTRATION_TEMPORARY_FILE")
	if err := os.WriteFile(filepath.Join(directory, "registration.json"), registration, 0600); err != nil {
		t.Fatal(err)
	}
	victim := t.TempDir()
	sentinel := filepath.Join(victim, "keep")
	if err := os.WriteFile(sentinel, []byte("private-placeholder"), 0600); err != nil {
		t.Fatal(err)
	}
	saved := directory + ".saved"
	if err := os.Rename(directory, saved); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(victim, directory); err != nil {
		t.Fatal(err)
	}
	check("RUNTIME_DIRECTORY_INVALID", "UNREGISTERED_RUNTIME_ARTIFACT")
	if err := os.Remove(directory); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(saved, directory); err != nil {
		t.Fatal(err)
	}
	body, err := os.ReadFile(sentinel)
	if err != nil || string(body) != "private-placeholder" {
		t.Fatal("scan changed unrelated directory", string(body), err)
	}
	manager, err := tmux.Open(filepath.Join(h.state, "acp"))
	if err != nil {
		t.Fatal(err)
	}
	orphan, instance := wire.ID(), wire.ID()
	if _, err := manager.CreateHost(orphan, instance, "/usr/bin/true", t.TempDir()); err != nil {
		t.Fatal(err)
	}
	check("UNREGISTERED_HOST_PANE")
	panes, err := manager.HostPanes(h.ctx)
	if err != nil || len(panes) != 2 {
		t.Fatal("discovery removed unregistered pane", panes, err)
	}
	if err := manager.DestroyHost(orphan, instance); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(filepath.Join(directory, ".pending-private-placeholder")); err != nil {
		t.Fatal(err)
	}
	waitTimeoutTest(t, func() bool {
		page, err := h.client.List(h.ctx)
		return err == nil && page.Complete && len(page.Items) == 1
	})
	body, err = os.ReadFile(filepath.Join(work, "process.log"))
	if err != nil || strings.Count(string(body), "\n") != 1 {
		t.Fatal(string(body), err)
	}
	rpcs, err := os.ReadFile(filepath.Join(work, "rpc.log"))
	if err != nil || string(rpcs) != "initialize\n" {
		t.Fatal("artifact checks performed Agent control", string(rpcs), err)
	}
	if err := testStopRuntime(h.client, h.ctx, runtime); err != nil {
		t.Fatal(err)
	}
	if err := testForgetRuntime(h.client, h.ctx, runtime); err != nil {
		t.Fatal(err)
	}
}

func TestArtifactDiagnosticsHaveBoundedEntriesAndOpaqueReferences(t *testing.T) {
	d := newEngine(t.Context())
	defer d.Close()
	d.stateDir = t.TempDir()
	root := filepath.Join(d.stateDir, "acp", "runtimes")
	if err := os.MkdirAll(root, 0700); err != nil {
		t.Fatal(err)
	}
	var err error
	d.acpTmux, err = tmux.Open(filepath.Join(d.stateDir, "acp"))
	if err != nil {
		t.Fatal(err)
	}
	for range 140 {
		if err := os.WriteFile(filepath.Join(root, "private-placeholder-"+wire.ID()), nil, 0600); err != nil {
			t.Fatal(err)
		}
	}
	issues := d.sessionArtifactIssues(t.Context(), sessionregistry.HostDiscovery{})
	if len(issues) != 129 || issues[128].Code != "ARTIFACT_SCAN_INCOMPLETE" {
		t.Fatal("unbounded or silently truncated issues", len(issues))
	}
	for _, issue := range issues[:128] {
		if issue.Runtime != nil || issue.Code != "UNREGISTERED_RUNTIME_ARTIFACT" || !strings.HasPrefix(issue.ArtifactRef, "directory:") {
			t.Fatal("unregistered artifact acquired Runtime authority", issue)
		}
	}
	encoded, _ := json.Marshal(issues)
	if strings.Contains(string(encoded), "private-placeholder") {
		t.Fatal("artifact filename exposed")
	}
	entries, err := os.ReadDir(root)
	if err != nil || len(entries) != 140 {
		t.Fatal("observation deleted artifacts", len(entries), err)
	}
}
