package fabricd

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/aiomni/dune/internal/lifecycle"
	"github.com/aiomni/dune/internal/retainedprogram"
	"github.com/aiomni/dune/pkg/api"
)

func TestOriginalHostRetainsProgramAcrossReleaseDeletionAndExplicitOpen(t *testing.T) {
	release := filepath.Join(t.TempDir(), "old-release")
	if err := os.Mkdir(release, 0700); err != nil {
		t.Fatal(err)
	}
	oldProgram := filepath.Join(release, "dune")
	if _, err := retainedprogram.Copy(os.Args[0], oldProgram); err != nil {
		t.Fatal(err)
	}
	mock := filepath.Join(t.TempDir(), "mock-acp")
	if out, err := exec.Command("go", "build", "-o", mock, "../../samples/mock-acp").CombinedOutput(); err != nil {
		t.Fatal(string(out), err)
	}
	h := newCleanupProcessHarness(t)
	h.program = oldProgram
	h.start("")
	work := t.TempDir()
	runtime, stream, err := testStartProfile(h.client, h.ctx, api.Profile{Version: 1, Kind: "agent", Adapter: "acp", ManagedACP: true, WorkingDirectory: work, Start: api.Command{Argv: []string{mock}}, Env: map[string]string{"DUNE_MOCK_PROCESS_LOG": filepath.Join(work, "process.log"), "DUNE_MOCK_RPC_LOG": filepath.Join(work, "rpc.log")}})
	if err != nil {
		t.Fatal(err)
	}
	stream.Close()
	waitTimeoutTest(t, func() bool { state, err := h.client.ACPState(h.ctx, runtime); return err == nil && state.Ready })
	open := func(id string) {
		t.Helper()
		operation, err := h.client.ACPSubmit(h.ctx, cleanupTestKey(runtime, id), api.ACPAction{Action: "new"})
		if err != nil {
			t.Fatal(err)
		}
		operation, err = h.client.WaitAgentOperation(h.ctx, runtime, api.AgentOperationWait{Ref: operation.Ref, TimeoutMS: 5000})
		if err != nil || operation.State != "completed" {
			t.Fatal(operation, err)
		}
	}
	open("first-explicit-open")
	before, err := h.client.Get(h.ctx, runtime)
	if err != nil || before.ACPHost == nil || before.ACPHost.HostPID <= 1 || before.ACPHost.AgentPID <= 1 || before.ACPHost.GroupID <= 1 || !before.ACPHost.Connected {
		t.Fatal(before, err)
	}
	identity := *before.ACPHost
	if identity.LifecycleLog == nil || identity.LifecycleLog.Bytes.Limit != lifecycle.MaxBytes || identity.LifecycleLog.WriteErrors != 0 {
		t.Fatal("missing bounded log diagnostics", identity)
	}
	pinned := filepath.Join(h.state, "acp", "runtimes", runtime.ID, "program")
	if err := retainedprogram.Verify(pinned, retainedprogram.Identity{SHA256: identity.ProgramSHA256, Bytes: identity.ProgramBytes}); err != nil {
		t.Fatal(err)
	}
	h.kill()
	if err := os.RemoveAll(release); err != nil {
		t.Fatal(err)
	}
	h.program = ""
	h.start("")
	after, err := h.client.Get(h.ctx, runtime)
	if err != nil || after.ACPHost == nil {
		t.Fatal(after, err)
	}
	connected := *after.ACPHost
	if connected.HostPID != identity.HostPID || connected.AgentPID != identity.AgentPID || connected.GroupID != identity.GroupID || connected.Instance != identity.Instance || connected.ProgramSHA256 != identity.ProgramSHA256 || !connected.Connected || connected.ConnectorTerm <= identity.ConnectorTerm || connected.LastAttachedAt == nil || connected.StartedAt == nil || !connected.StartedAt.Equal(*identity.StartedAt) {
		t.Fatal("connector replacement changed original host", identity, connected)
	}
	open("second-explicit-open-after-release-deletion")
	after, err = h.client.Get(h.ctx, runtime)
	if err != nil || after.ACPHost == nil || after.ACPHost.HostPID != identity.HostPID || after.ACPHost.Instance != identity.Instance || after.ACPHost.ProgramSHA256 != identity.ProgramSHA256 || after.ACPHost.AgentPID == identity.AgentPID || after.ACPHost.GroupID == identity.GroupID {
		t.Fatal("explicit open did not use original host and retained guardian", after, err)
	}
	body, err := os.ReadFile(filepath.Join(work, "process.log"))
	if err != nil || strings.Count(string(body), "\n") != 2 {
		t.Fatal(string(body), err)
	}
	rpcs, err := os.ReadFile(filepath.Join(work, "rpc.log"))
	if err != nil || strings.Count(string(rpcs), "initialize\n") != 2 || strings.Count(string(rpcs), "session/new\n") != 2 {
		t.Fatal("replacement happened outside explicit open", string(rpcs), err)
	}
	const privatePrompt = "private-prompt-must-never-enter-lifecycle-log"
	prompt, err := h.client.ACPSubmit(h.ctx, cleanupTestKey(runtime, "private-log-probe"), api.ACPAction{Action: "prompt", Text: privatePrompt, ExpectedConversationID: after.ConversationID})
	if err != nil {
		t.Fatal(err)
	}
	prompt, err = h.client.WaitAgentOperation(h.ctx, runtime, api.AgentOperationWait{Ref: prompt.Ref, TimeoutMS: 5000})
	if err != nil || prompt.State != "completed" {
		t.Fatal(prompt, err)
	}
	if err := testStopRuntime(h.client, h.ctx, runtime); err != nil {
		t.Fatal(err)
	}
	logPath := filepath.Join(h.state, "acp", "runtimes", runtime.ID, "host-events.jsonl")
	waitTimeoutTest(t, func() bool {
		body, _ := os.ReadFile(logPath)
		if strings.Contains(string(body), privatePrompt) {
			t.Fatal("lifecycle log retained prompt content")
		}
		if len(body) > lifecycle.MaxBytes {
			t.Fatal("host lifecycle log exceeded bound")
		}
		for _, kind := range []string{"host_started", "admission_receipt", "attach", "detach", "control_takeover", "stop_accepted", "agent_exit"} {
			if !strings.Contains(string(body), `"kind":"`+kind+`"`) {
				return false
			}
		}
		return true
	})
	if err := testForgetRuntime(h.client, h.ctx, runtime); err != nil {
		t.Fatal(err)
	}
	waitTimeoutTest(t, func() bool {
		body, _ := os.ReadFile(filepath.Join(h.state, "connector-events.jsonl"))
		return strings.Contains(string(body), `"kind":"cleanup_completed"`) && strings.Contains(string(body), runtime.ID)
	})
	if _, err := os.Lstat(pinned); !os.IsNotExist(err) {
		t.Fatal("completed forget retained the helper", err)
	}
	t.Logf("host PID=%d; original Agent=%d; explicit replacement Agent=%d; retained program bytes=%d", identity.HostPID, identity.AgentPID, after.ACPHost.AgentPID, identity.ProgramBytes)
}
