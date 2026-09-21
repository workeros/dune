package fabricd

import (
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/aiomni/dune/internal/sessionregistry"
	"github.com/aiomni/dune/pkg/api"
)

func TestEachOrdinaryCapacityPreservesOriginalControlsAcrossConnectorCrash(t *testing.T) {
	mock := mockACPBinary(t)
	for _, dimension := range []string{"queue", "results", "evidence", "machine"} {
		t.Run(dimension, func(t *testing.T) {
			h := newCleanupProcessHarness(t)
			h.start("")
			work := t.TempDir()
			rpcLog, processLog := filepath.Join(work, "rpc.log"), filepath.Join(work, "process.log")
			runtime, stream, err := testStartProfile(h.client, h.ctx, api.Profile{Version: 1, Kind: "agent", Adapter: "acp", ManagedACP: true, WorkingDirectory: work, Start: api.Command{Argv: []string{mock}}, Env: map[string]string{"DUNE_MOCK_HISTORY": "1", "DUNE_MOCK_RPC_LOG": rpcLog, "DUNE_MOCK_PROCESS_LOG": processLog}})
			if err != nil {
				t.Fatal(err)
			}
			stream.Close()
			state := func() api.ACPState {
				t.Helper()
				s, err := h.client.ACPState(h.ctx, runtime)
				if err != nil {
					t.Fatal(err)
				}
				return s
			}
			capacity := func() api.SubmissionCapacity {
				t.Helper()
				var info api.MachineInfo
				if err := h.client.Call(h.ctx, "machine.info", struct{}{}, &info); err != nil || info.SubmissionCapacity == nil {
					t.Fatal(info, err)
				}
				return *info.SubmissionCapacity
			}
			waitTimeoutTest(t, func() bool { return state().Ready })
			open, err := h.client.ACPSubmit(h.ctx, cleanupTestKey(runtime, "open"), api.ACPAction{Action: "new", Cwd: work})
			if err != nil {
				t.Fatal(err)
			}
			wait := func(ref string) {
				t.Helper()
				r, err := h.client.WaitAgentOperation(h.ctx, runtime, api.AgentOperationWait{Ref: ref, TimeoutMS: 3000})
				if err != nil || !r.Terminal() {
					t.Fatal(r, err)
				}
			}
			wait(open.Ref)
			conversation := state().Conversation.ID
			submit := func(id, text string) api.AgentOperation {
				t.Helper()
				r, err := h.client.ACPSubmit(h.ctx, cleanupTestKey(runtime, id), api.ACPAction{Action: "prompt", Text: text, ExpectedConversationID: conversation})
				if err != nil {
					t.Fatal(r, err)
				}
				return r
			}
			if dimension == "results" {
				// One open + 61 completed prompts + the two original active/queued
				// prompts fill exactly 64 results, with the ordinary queue still free.
				for i := range maxRuntimeOperations - 3 {
					wait(submit(fmt.Sprintf("completed-%d", i), "complete this result").Ref)
				}
			}
			first := submit("first", "permission first")
			second := submit("second", "permission second")
			waitTimeoutTest(t, func() bool { return len(state().Permissions) == 1 })
			permission := state().Permissions[0].ID
			ended, registry := h.launch(mock)
			if err := testStopRuntime(h.client, h.ctx, ended); err != nil {
				t.Fatal(err)
			}
			switch dimension {
			case "queue":
				for i := 1; i < maxACPPending; i++ {
					submit(fmt.Sprintf("queued-%d", i), "permission queued")
				}
			case "evidence":
				// Seed historical closed rejections through the real registry
				// admission API, at the unchanged default limit. These have no
				// Agent side effects or operation-output slots to confound this test.
				for i := capacity().Ordinary.Used; i < sessionregistry.DefaultMaxKeys; i++ {
					key := cleanupTestKey(runtime, fmt.Sprintf("historical-rejection-%d", i))
					claim, _, err := registry.ClaimKey(h.ctx, key, sessionregistry.Digest("acp.action", nil), "historical-fixture")
					if err != nil {
						t.Fatal(err)
					}
					if _, err := registry.Reject(h.ctx, claim, "INVALID_ARGUMENT"); err != nil {
						t.Fatal(err)
					}
				}
			case "machine":
				for i := 2; i < sessionregistry.MaxLiveRuntimes; i++ {
					h.launch(mock)
				}
			}
			beforeState, beforeCapacity := state(), capacity()
			full := false
			switch dimension {
			case "queue":
				full = beforeState.Resources.Queue.Used == maxACPPending && beforeState.Resources.Operations.Records.Used < maxRuntimeOperations
			case "results":
				full = beforeState.Resources.Operations.Records.Used == maxRuntimeOperations && beforeState.Resources.Queue.Used == 1
			case "evidence":
				full = beforeCapacity.Ordinary.Used == sessionregistry.DefaultMaxKeys && beforeState.Resources.Operations.Records.Used == 3
			case "machine":
				full = beforeCapacity.Runtimes.Used == sessionregistry.MaxLiveRuntimes && beforeCapacity.Ordinary.Used < sessionregistry.DefaultMaxKeys
			}
			if !full {
				t.Fatal("requested dimension was not independently saturated", beforeState.Resources, beforeCapacity)
			}
			h.kill()
			h.start("")
			if after := state(); after.Permissions[0].ID != permission || after.Conversation.ID != conversation || !reflect.DeepEqual(after.Resources, beforeState.Resources) {
				t.Fatal("reconnect changed original host state or capacity", after.Resources, beforeState.Resources)
			}
			for range 3 {
				if after := capacity(); !reflect.DeepEqual(after, beforeCapacity) {
					t.Fatal("public read consumed/resized durable capacity", after, beforeCapacity)
				}
				if r, err := h.client.QuerySubmission(h.ctx, cleanupTestKey(runtime, "first")); err != nil || r.OperationRef != first.Ref {
					t.Fatal("original query failed under pressure", r, err)
				}
			}
			if dimension == "machine" {
				key := cleanupTestKey(api.Runtime{}, "refused-launch")
				result, stream, err := h.client.Start(h.ctx, api.StartRequest{SubmissionKey: key, Profile: api.Profile{Version: 1, Kind: "agent", Adapter: "acp", ManagedACP: true, WorkingDirectory: work, Start: api.Command{Argv: []string{mock}}}})
				if stream != nil {
					stream.Close()
				}
				requireSubmissionCode(t, err, "SUBMISSION_CAPACITY_EXHAUSTED")
				if result.Admission != api.SubmissionNotAccepted || capacity().Runtimes.Used != sessionregistry.MaxLiveRuntimes {
					t.Fatal("excess launch consumed another Runtime", result)
				}
			} else {
				key := cleanupTestKey(runtime, "refused-prompt")
				action := api.ACPAction{Action: "prompt", Text: "must not execute", ExpectedConversationID: conversation}
				r, err := h.client.ACPSubmit(h.ctx, key, action)
				requireSubmissionCode(t, err, "SUBMISSION_CAPACITY_EXHAUSTED")
				if dimension != "evidence" && (r.Submission == nil || r.Submission.Admission != api.SubmissionNotAccepted) {
					t.Fatal("capacity rejection was not sealed", r)
				}
			}
			answer, err := h.client.ACPControl(h.ctx, cleanupTestKey(runtime, "answer"), api.ACPAction{Action: "permission", PermissionID: permission, OptionID: "allow"})
			if err != nil || answer.Stage != "written" {
				t.Fatal("ordinary capacity blocked valid permission", answer, err)
			}
			wait(first.Ref)
			waitTimeoutTest(t, func() bool { s := state(); return len(s.Permissions) == 1 && s.Permissions[0].ID != permission })
			cancelled, err := h.client.ACPControl(h.ctx, cleanupTestKey(runtime, "cancel"), api.ACPAction{Action: "cancel", OperationRef: second.Ref})
			if err != nil || cancelled.Stage != "written" {
				t.Fatal("ordinary capacity blocked effective cancel", cancelled, err)
			}
			wait(second.Ref)
			waitTimeoutTest(t, func() bool { b, _ := os.ReadFile(rpcLog); return strings.Count(string(b), "session/cancel\n") == 1 })
			stopped, err := h.client.Stop(h.ctx, cleanupTestKey(runtime, "stop"))
			if err != nil || stopped.Stage != "stopped" {
				t.Fatal(stopped, err)
			}
			forgetKey := cleanupTestKey(ended, "forget")
			if r, err := h.client.Forget(h.ctx, forgetKey); err != nil || r.Admission != api.SubmissionAccepted {
				t.Fatal("ordinary capacity blocked eligible cleanup", r, err)
			}
			waitTimeoutTest(t, func() bool {
				r, err := h.client.QuerySubmission(h.ctx, forgetKey)
				return err == nil && r.Stage == "completed"
			})
			for _, id := range []string{"answer", "cancel", "stop"} {
				r, err := h.client.QuerySubmission(h.ctx, cleanupTestKey(runtime, id))
				if err != nil || r.Admission != api.SubmissionAccepted {
					t.Fatal(id, r, err)
				}
			}
			processes, err := os.ReadFile(processLog)
			if err != nil || strings.Count(string(processes), "\n") != 1 {
				t.Fatal("capacity recovery restarted the original Agent", string(processes), err)
			}
			rpcs, err := os.ReadFile(rpcLog)
			if err != nil || strings.Count(string(rpcs), "initialize\n") != 1 || strings.Count(string(rpcs), "session/new\n") != 1 || strings.Count(string(rpcs), "session/cancel\n") != 1 {
				t.Fatal("capacity recovery replayed lifecycle/control requests", string(rpcs), err)
			}
			history, err := os.ReadFile(filepath.Join(work, ".dune-mock-acp-history.json"))
			if err != nil || strings.Contains(string(history), "must not execute") || strings.Count(string(history), "Mock permission response received") != 2 {
				t.Fatal("control delivery count or rejected work changed", string(history), err)
			}
			after := capacity()
			for class, usage := range after.Controls {
				if usage.Used > usage.Limit || usage.Used != usage.Reserved+usage.Claimed+usage.Accepted+usage.Rejected {
					t.Fatal("control budget is unbounded or inconsistent", class, usage)
				}
			}
			t.Logf("dimension=%s before=%+v after=%+v host_resources=%+v", dimension, beforeCapacity, after, beforeState.Resources)
		})
	}
}
