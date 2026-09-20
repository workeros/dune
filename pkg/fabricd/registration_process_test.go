package fabricd

import (
	"encoding/json"
	"io"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/aiomni/dune/pkg/api"
)

// Only the test executable can pause before the original host registers.
func registrationHostTestHelper(args []string) (int, bool) {
	if len(args) != 2 || args[0] != "_acp_host" {
		return 0, false
	}
	var boot sessionBootstrap
	if privateFile(filepath.Join(args[1], "bootstrap.json"), 8*1024*1024, &boot) != nil || boot.Profile.Env["DUNE_REGISTRATION_TEST_GATE"] == "" {
		return 0, false
	}
	conn, err := net.DialTimeout("tcp", boot.Profile.Env["DUNE_REGISTRATION_TEST_GATE"], 3*time.Second)
	if err != nil {
		return 1, true
	}
	defer conn.Close()
	_ = conn.SetDeadline(time.Now().Add(30 * time.Second))
	if json.NewEncoder(conn).Encode(boot.Registration.Runtime) != nil {
		return 1, true
	}
	var release [1]byte
	if _, err := io.ReadFull(conn, release[:]); err != nil {
		return 1, true
	}
	if err := runSessionHost(args[1]); err != nil {
		return 1, true
	}
	return 0, true
}

func TestLateOriginalHostRegistrationRecoversWithoutAnotherLaunch(t *testing.T) {
	mock := filepath.Join(t.TempDir(), "mock-acp")
	if out, err := exec.Command("go", "build", "-o", mock, "../../samples/mock-acp").CombinedOutput(); err != nil {
		t.Fatal(string(out), err)
	}
	h := newCleanupProcessHarness(t)
	h.start("")
	gate, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer gate.Close()
	key := cleanupTestKey(api.Runtime{}, "original-late-launch")
	key.Target.RuntimeID, key.Target.RuntimeIncarnation, key.Target.RuntimeGeneration = "", "", 0
	work := t.TempDir()
	profile := api.Profile{Version: 1, Kind: "agent", Adapter: "acp", ManagedACP: true, WorkingDirectory: work, Start: api.Command{Argv: []string{mock}}, Env: map[string]string{"DUNE_REGISTRATION_TEST_GATE": gate.Addr().String(), "DUNE_MOCK_PROCESS_LOG": filepath.Join(work, "process.log"), "DUNE_MOCK_RPC_LOG": filepath.Join(work, "rpc.log")}}
	startDone := make(chan error, 1)
	connection := h.client
	go func() {
		_, stream, err := connection.Start(h.ctx, api.StartRequest{SubmissionKey: key, Profile: profile})
		if stream != nil {
			stream.Close()
		}
		startDone <- err
	}()
	_ = gate.(*net.TCPListener).SetDeadline(time.Now().Add(8 * time.Second))
	paused, err := gate.Accept()
	if err != nil {
		t.Fatal(err)
	}
	defer paused.Close()
	_ = paused.SetDeadline(time.Now().Add(20 * time.Second))
	var original api.Runtime
	if err := json.NewDecoder(paused).Decode(&original); err != nil {
		t.Fatal(err)
	}
	if guard, report := PrepareUpgrade(h.ctx, h.state); guard != nil || report.Allowed || report.Issues[0].Code != "LAUNCH_IN_PROGRESS" {
		t.Fatal("upgrade crossed in-flight original launch", report)
	}
	h.kill()
	if err := <-startDone; err == nil {
		t.Fatal("unregistered launch unexpectedly confirmed")
	}
	guard, report := PrepareUpgrade(h.ctx, h.state)
	if guard == nil || !report.Allowed || len(report.Hosts) != 1 || report.Hosts[0].Runtime.ID != original.ID || report.Hosts[0].Protocol != sessionProtocol {
		t.Fatal("preflight missed the late original registration", report)
	}
	defer guard.Close()
	h.start("")
	receipt, err := h.client.QuerySubmission(h.ctx, key)
	if err != nil || receipt.Admission != api.SubmissionAccepted || receipt.Runtime == nil || receipt.Runtime.ID != original.ID || receipt.Stage != "host_starting" {
		t.Fatal(receipt, err)
	}
	page, err := h.client.List(h.ctx)
	if err != nil || page.Complete || len(page.Items) != 0 || len(page.Issues) != 1 || page.Issues[0].Code != "HOST_REGISTRATION_PENDING" || page.Issues[0].Runtime == nil || page.Issues[0].Runtime.ID != original.ID {
		t.Fatal("pending original host disappeared", page, err)
	}
	if _, err := h.client.Get(h.ctx, original); err == nil || !strings.Contains(err.Error(), "HOST_REGISTRATION_PENDING") {
		t.Fatal("pending original identity was declared stale", err)
	}
	if _, err := paused.Write([]byte{1}); err != nil {
		t.Fatal(err)
	}
	started := time.Now()
	waitTimeoutTest(t, func() bool {
		page, err = h.client.List(h.ctx)
		return err == nil && page.Complete && len(page.Items) == 1 && page.Items[0].ID == original.ID && page.Items[0].Availability == ""
	})
	state, err := h.client.ACPState(h.ctx, original)
	if err != nil || !state.Ready {
		t.Fatal(state, err)
	}
	guard.Close()
	receipt, err = h.client.QuerySubmission(h.ctx, key)
	if err != nil || receipt.Stage != "started" || receipt.Runtime == nil || receipt.Runtime.ID != original.ID {
		t.Fatal(receipt, err)
	}
	body, err := os.ReadFile(filepath.Join(work, "process.log"))
	if err != nil || strings.Count(string(body), "\n") != 1 {
		t.Fatal("replacement Agent started", string(body), err)
	}
	rpcs, err := os.ReadFile(filepath.Join(work, "rpc.log"))
	if err != nil || string(rpcs) != "initialize\n" {
		t.Fatal("discovery controlled Agent", string(rpcs), err)
	}
	t.Logf("late original registration callable after %s", time.Since(started))
	if err := testStopRuntime(h.client, h.ctx, original); err != nil {
		t.Fatal(err)
	}
	if err := testForgetRuntime(h.client, h.ctx, original); err != nil {
		t.Fatal(err)
	}
	for range 3 {
		time.Sleep(1100 * time.Millisecond)
		page, err := h.client.List(h.ctx)
		if err != nil || !page.Complete || len(page.Items) != 0 || len(page.Issues) != 0 {
			t.Fatal("scan resurrected forgotten target", page, err)
		}
	}
}
