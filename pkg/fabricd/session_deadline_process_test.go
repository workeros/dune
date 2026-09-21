package fabricd

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/aiomni/dune/pkg/api"
)

func TestOriginalACPDeadlineExpiresWhileConnectorIsDead(t *testing.T) {
	mock := mockACPBinary(t)
	for _, managed := range []bool{false, true} {
		mode := map[bool]string{false: "raw", true: "managed"}[managed]
		t.Run(mode, func(t *testing.T) {
			h := newCleanupProcessHarness(t)
			h.start("")
			work := t.TempDir()
			runtime, stream, err := testStartProfile(h.client, h.ctx, api.Profile{Version: 1, Kind: "agent", Adapter: "acp", ManagedACP: managed, WorkingDirectory: work, Start: api.Command{Argv: []string{mock}, TimeoutSeconds: 3}, Env: map[string]string{"DUNE_MOCK_PROCESS_LOG": filepath.Join(work, "process.log"), "DUNE_MOCK_RPC_LOG": filepath.Join(work, "rpc.log")}})
			if err != nil {
				t.Fatal(err)
			}
			stream.Close()
			before, err := h.client.Get(h.ctx, runtime)
			if err != nil || before.DeadlineAt == nil || before.ACPHost == nil {
				t.Fatal(before, err)
			}
			h.kill()
			// The original host must publish its terminal fact before any
			// replacement connector exists. Poll facts, not an assumed sleep.
			waitTimeoutTest(t, func() bool {
				body, err := os.ReadFile(filepath.Join(h.state, "acp", "runtimes", runtime.ID, "registration.json"))
				var reg sessionRegistration
				return err == nil && json.Unmarshal(body, &reg) == nil && reg.Runtime.State == "exited" && reg.Runtime.StopReason == "timeout"
			})
			h.start("")
			after, err := h.client.Get(h.ctx, runtime)
			if err != nil || after.State != "exited" || after.StopReason != "timeout" || after.DeadlineAt == nil || !after.DeadlineAt.Equal(*before.DeadlineAt) || after.ACPHost == nil || after.ACPHost.HostPID != before.ACPHost.HostPID || after.Incarnation != runtime.Incarnation || after.Generation != runtime.Generation {
				t.Fatal("reconnect changed original deadline or terminal fact", before, after, err)
			}
			processes, err := os.ReadFile(filepath.Join(work, "process.log"))
			if err != nil || strings.Count(string(processes), "\n") != 1 {
				t.Fatal("deadline/reconnect replaced Agent", err)
			}
			forgotten, err := h.client.Forget(h.ctx, cleanupTestKey(runtime, "forget-timed-out"))
			if err != nil || forgotten.Stage != "completed" {
				t.Fatal(forgotten, err)
			}
		})
	}
}
