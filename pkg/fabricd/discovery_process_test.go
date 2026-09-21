package fabricd

import (
	"database/sql"
	"encoding/json"
	"io"
	"net"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/aiomni/dune/internal/sessionregistry"
	"github.com/aiomni/dune/pkg/api"
)

func TestDiscoveryRecoversEightHostsBesideUnresponsiveEndpointAndCorruptRegistration(t *testing.T) {
	mock := mockACPBinary(t)
	h := newCleanupProcessHarness(t)
	h.start("")
	var runtimes []api.Runtime
	var workspaces []string
	for range 10 {
		work := t.TempDir()
		r, stream, err := testStartProfile(h.client, h.ctx, api.Profile{Version: 1, Kind: "agent", Adapter: "acp", ManagedACP: true, WorkingDirectory: work, Start: api.Command{Argv: []string{mock}}, Env: map[string]string{"DUNE_MOCK_PROCESS_LOG": filepath.Join(work, "process.log"), "DUNE_MOCK_RPC_LOG": filepath.Join(work, "rpc.log")}})
		if err != nil {
			t.Fatal(err)
		}
		stream.Close()
		waitTimeoutTest(t, func() bool { state, err := h.client.ACPState(h.ctx, r); return err == nil && state.Ready })
		runtimes = append(runtimes, r)
		workspaces = append(workspaces, work)
	}
	registry, err := sessionregistry.Open(h.ctx, filepath.Join(h.state, "registry"), sessionregistry.Options{})
	if err != nil {
		t.Fatal(err)
	}
	defer registry.Close()
	slow, err := registry.Host(h.ctx, cleanupTestKey(runtimes[8], "read").Target)
	if err != nil {
		t.Fatal(err)
	}
	corrupted, err := registry.Host(h.ctx, cleanupTestKey(runtimes[9], "read").Target)
	if err != nil {
		t.Fatal(err)
	}
	h.kill()
	var registration sessionRegistration
	if err := json.Unmarshal(slow.Registration, &registration); err != nil {
		t.Fatal(err)
	}
	socket, err := sessionSocket(registration)
	if err != nil {
		t.Fatal(err)
	}
	// tmux resumes stopped foreground children. Replace only this endpoint with
	// a listener that accepts without answering; the original host keeps running.
	restoreEndpoint, probeClosed := unresponsiveSessionEndpoint(t, socket)
	defer restoreEndpoint()
	db, err := sql.Open("sqlite", filepath.Join(h.state, "registry", "registry.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	_, err = db.Exec(`UPDATE session_hosts SET registration=? WHERE instance=(SELECT instance FROM session_hosts WHERE target=?)`, []byte("invalid-private-registration"), string(api.Payload(cleanupTestKey(runtimes[9], "read").Target)))
	db.Close()
	if err != nil {
		t.Fatal(err)
	}
	started := time.Now()
	h.start("")
	listDone := make(chan api.RuntimeList, 1)
	listErr := make(chan error, 1)
	go func() { page, err := h.client.List(h.ctx); listDone <- page; listErr <- err }()
	// A list probes a truly unresponsive endpoint. Other exact targets must remain
	// callable instead of waiting behind the Engine's former global mutex.
	for _, r := range runtimes[:8] {
		callStarted := time.Now()
		state, err := h.client.ACPState(h.ctx, r)
		if err != nil || !state.Ready || time.Since(callStarted) > time.Second {
			t.Fatal("bad host blocked healthy exact call", err, time.Since(callStarted))
		}
	}
	var page api.RuntimeList
	select {
	case page = <-listDone:
	case <-time.After(5 * time.Second):
		t.Fatal("bounded discovery did not finish")
	}
	if err := <-listErr; err != nil {
		t.Fatal(err)
	}
	// An overlapping startup probe may briefly report a healthy target as
	// recovering. Read-only backoff converges without creating another process.
	deadline := time.Now().Add(8 * time.Second)
	for len(page.Issues) != 3 && time.Now().Before(deadline) {
		time.Sleep(50 * time.Millisecond)
		page, err = h.client.List(h.ctx)
		if err != nil {
			t.Fatal(err)
		}
	}
	if page.Complete || len(page.Items) != 9 || len(page.Issues) != 3 {
		t.Fatal(page)
	}
	issues := map[string]string{}
	replacedSocket := false
	for _, issue := range page.Issues {
		if issue.Runtime == nil {
			t.Fatal(issue)
		}
		issues[issue.Runtime.ID] = issue.Code
		if issue.Runtime.ID == runtimes[8].ID && issue.Code == "IPC_SOCKET_REPLACED" {
			replacedSocket = true
		}
	}
	if !replacedSocket || issues[runtimes[8].ID] != "SESSION_UNAVAILABLE" || issues[runtimes[9].ID] != "REGISTRATION_INVALID" {
		t.Fatal(issues)
	}
	for _, item := range page.Items {
		if item.ID == runtimes[8].ID {
			if item.State != "running" || item.Availability != "unavailable" {
				t.Fatal("timeout fabricated exit", item)
			}
		} else if item.Availability != "" || item.LastConfirmedAt == nil {
			t.Fatal("healthy host unconfirmed", item)
		}
	}
	if time.Since(started) > 30*time.Second {
		t.Fatal("healthy recovery exceeded target", time.Since(started))
	}
	healthyRecovery := time.Since(started)
	select {
	case <-probeClosed:
	case <-time.After(4 * time.Second):
		t.Fatal("unresponsive endpoint probe exceeded its deadline")
	}
	if _, err := h.client.Get(h.ctx, runtimes[9]); err == nil || !strings.Contains(err.Error(), "REGISTRATION_INVALID") {
		t.Fatal("corruption presented as stale/ended", err)
	}
	restoreEndpoint()
	state, err := h.client.ACPState(h.ctx, runtimes[8])
	if err != nil || !state.Ready {
		t.Fatal("same endpoint did not recover", state, err)
	}
	db, err = sql.Open("sqlite", filepath.Join(h.state, "registry", "registry.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	_, err = db.Exec(`UPDATE session_hosts SET registration=? WHERE instance=?`, []byte(corrupted.Registration), corrupted.Instance)
	db.Close()
	if err != nil {
		t.Fatal(err)
	}
	waitTimeoutTest(t, func() bool {
		page, err := h.client.List(h.ctx)
		return err == nil && page.Complete && len(page.Items) == 10
	})
	if state, err := h.client.ACPState(h.ctx, runtimes[9]); err != nil || !state.Ready {
		t.Fatal("repaired original registration was not rediscovered", state, err)
	}
	for i, work := range workspaces {
		body, err := os.ReadFile(filepath.Join(work, "process.log"))
		if err != nil || strings.Count(string(body), "\n") != 1 {
			t.Fatal(i, string(body), err)
		}
		var identity struct {
			PID int `json:"pid"`
		}
		if json.Unmarshal(body, &identity) != nil || identity.PID == 0 {
			t.Fatal(string(body))
		}
		rpcs, err := os.ReadFile(filepath.Join(work, "rpc.log"))
		if err != nil || strings.Count(string(rpcs), "initialize\n") != 1 || strings.Contains(string(rpcs), "session/") {
			t.Fatal("discovery changed Agent RPCs", i, string(rpcs), err)
		}
	}
	t.Logf("8 original healthy hosts callable; unresponsive endpoint timed out separately; corrupt registration separate; healthy recovery=%s", healthyRecovery)
}

func unresponsiveSessionEndpoint(t *testing.T, socket string) (func(), <-chan struct{}) {
	t.Helper()
	original := socket + ".original"
	if err := os.Rename(socket, original); err != nil {
		t.Fatal(err)
	}
	listener, err := net.Listen("unix", socket)
	if err != nil {
		_ = os.Rename(original, socket)
		t.Fatal(err)
	}
	if err := os.Chmod(socket, 0600); err != nil {
		t.Fatal(err)
	}
	connections := make(chan net.Conn, 64)
	probeClosed := make(chan struct{})
	var observed sync.Once
	go func() {
		defer close(connections)
		for {
			conn, err := listener.Accept()
			if err != nil {
				return
			}
			select {
			case connections <- conn:
				go func() {
					_, _ = io.Copy(io.Discard, conn)
					observed.Do(func() { close(probeClosed) })
				}()
			default:
				conn.Close()
			}
		}
	}()
	var once sync.Once
	return func() {
		once.Do(func() {
			listener.Close()
			for conn := range connections {
				conn.Close()
			}
			if err := os.Rename(original, socket); err != nil {
				t.Error(err)
			}
		})
	}, probeClosed
}
