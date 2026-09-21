package fabricd

import (
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/aiomni/dune/pkg/api"
)

func TestFailedACPStartupRetainsOriginalReceiptAndCanBeRetired(t *testing.T) {
	for _, test := range []struct {
		name, fault, agent, code string
		managed                  bool
	}{
		{name: "before_entry", fault: "exit_before_entry", agent: "/bin/cat", code: "HOST_EXITED_BEFORE_ENTRY"},
		{name: "bootstrap", fault: "corrupt_bootstrap", agent: "/bin/cat", code: "HOST_VALIDATION_FAILED"},
		{name: "permissions", fault: "invalid_permissions", agent: "/bin/cat", code: "HOST_VALIDATION_FAILED"},
		{name: "agent_exec", agent: "/nonexistent/dune-test-agent", code: "AGENT_START_FAILED"},
		{name: "agent_initialize", agent: "mock", managed: true, code: "AGENT_INITIALIZATION_FAILED"},
	} {
		t.Run(test.name, func(t *testing.T) {
			h := newCleanupProcessHarness(t)
			h.start("")
			agent := test.agent
			if agent == "mock" {
				agent = mockACPBinary(t)
			}
			work := t.TempDir()
			sentinel := filepath.Join(work, "user-history")
			if err := os.WriteFile(sentinel, []byte("preserve"), 0600); err != nil {
				t.Fatal(err)
			}
			key := cleanupTestKey(api.Runtime{}, "original-failed-launch")
			key.Target.RuntimeID, key.Target.RuntimeIncarnation, key.Target.RuntimeGeneration = "", "", 0
			request := api.StartRequest{SubmissionKey: key, Profile: api.Profile{Version: 1, Kind: "agent", Adapter: "acp", ManagedACP: test.managed, WorkingDirectory: work, Start: api.Command{Argv: []string{agent}}, Env: map[string]string{"DUNE_HOST_STARTUP_TEST_FAULT": test.fault, "PRIVATE_SECRET": "private-environment-sentinel", "DUNE_MOCK_INITIALIZE_FAILURE": "1"}}}
			_, stream, err := h.client.Start(h.ctx, request)
			if stream != nil {
				stream.Close()
			}
			if err == nil || !strings.Contains(err.Error(), test.code) {
				t.Fatal("wrong startup failure", err)
			}
			before, err := h.client.QuerySubmission(h.ctx, key)
			if err != nil || before.Admission != api.SubmissionAccepted || before.Stage != "failed" || before.ErrorCode != test.code || before.Runtime == nil || before.Runtime.ACPHost.Startup.Code != test.code {
				t.Fatal(before, err)
			}
			assertStartupCapacity(t, h, 1)
			if strings.Contains(string(api.Payload(before)), "private-") {
				t.Fatal("diagnostic leaked private input")
			}
			h.kill()
			h.start("")
			after, err := h.client.QuerySubmission(h.ctx, key)
			if err != nil || !reflect.DeepEqual(before, after) {
				t.Fatal("restart changed receipt", after, err)
			}
			_, duplicateStream, err := h.client.Start(h.ctx, request)
			if duplicateStream != nil {
				duplicateStream.Close()
			}
			if err == nil || !strings.Contains(err.Error(), "ALREADY_SUBMITTED") {
				t.Fatal("duplicate launched again", err)
			}
			if err := testStopRuntime(h.client, h.ctx, *before.Runtime); err != nil {
				t.Fatal(err)
			}
			if err := testForgetRuntime(h.client, h.ctx, *before.Runtime); err != nil {
				t.Fatal(err)
			}
			assertStartupCapacity(t, h, 0)
			after, err = h.client.QuerySubmission(h.ctx, key)
			if err != nil || !reflect.DeepEqual(before, after) {
				t.Fatal("cleanup changed launch receipt", after, err)
			}
			if _, err := os.Stat(filepath.Join(h.state, "acp", "runtimes", before.Runtime.ID)); !os.IsNotExist(err) {
				t.Fatal("failed host directory retained", err)
			}
			if body, err := os.ReadFile(sentinel); err != nil || string(body) != "preserve" {
				t.Fatal("user history changed", err)
			}
		})
	}
}

func assertStartupCapacity(t *testing.T, h *cleanupProcessHarness, want int) {
	t.Helper()
	var info api.MachineInfo
	if err := h.client.Call(h.ctx, "machine.info", struct{}{}, &info); err != nil || info.SubmissionCapacity == nil || info.SubmissionCapacity.Runtimes.Used != want {
		t.Fatal("unexpected retained Runtime capacity", info.SubmissionCapacity, err)
	}
}
