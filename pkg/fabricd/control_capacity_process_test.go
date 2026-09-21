package fabricd

import (
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/aiomni/dune/internal/sessionregistry"
	"github.com/aiomni/dune/pkg/api"
)

type capacityAgent struct {
	runtime                        api.Runtime
	prompt                         api.AgentOperation
	conversation, permission, work string
}

func startCapacityAgent(t *testing.T, h *cleanupProcessHarness, mock, gate string) capacityAgent {
	t.Helper()
	a := capacityAgent{work: t.TempDir()}
	env := map[string]string{
		"DUNE_MOCK_HISTORY": "1", "DUNE_MOCK_RPC_LOG": filepath.Join(a.work, "rpc.log"),
		"DUNE_MOCK_PROCESS_LOG": filepath.Join(a.work, "process.log"), "DUNE_MOCK_RESPONSE_LOG": filepath.Join(a.work, "responses.log"),
	}
	text := "permission active"
	if gate != "" {
		env["DUNE_MOCK_PROMPT_GATE"], env["DUNE_MOCK_PROMPT_GATE_TIMEOUT"] = gate, "10m"
		text = "barrier permission after capacity fills"
	}
	r, stream, err := testStartProfile(h.client, h.ctx, api.Profile{Version: 1, Kind: "agent", Adapter: "acp", ManagedACP: true, WorkingDirectory: a.work, Start: api.Command{Argv: []string{mock}}, Env: env})
	if err != nil {
		t.Fatal(err)
	}
	a.runtime = r
	stream.Close()
	waitTimeoutTest(t, func() bool { s, err := h.client.ACPState(h.ctx, r); return err == nil && s.Ready })
	opened, err := h.client.ACPSubmit(h.ctx, cleanupTestKey(r, "open"), api.ACPAction{Action: "new", Cwd: a.work})
	if err != nil {
		t.Fatal(err)
	}
	if op, err := h.client.WaitAgentOperation(h.ctx, r, api.AgentOperationWait{Ref: opened.Ref, TimeoutMS: 3000}); err != nil || op.State != "completed" {
		t.Fatal(op, err)
	}
	s, err := h.client.ACPState(h.ctx, r)
	if err != nil {
		t.Fatal(err)
	}
	a.conversation = s.Conversation.ID
	a.prompt, err = h.client.ACPSubmit(h.ctx, cleanupTestKey(r, "original-prompt"), api.ACPAction{Action: "prompt", Text: text, ExpectedConversationID: a.conversation})
	if err != nil {
		t.Fatal(err)
	}
	if gate == "" {
		waitTimeoutTest(t, func() bool {
			s, err := h.client.ACPState(h.ctx, r)
			if err == nil && len(s.Permissions) == 1 {
				a.permission = s.Permissions[0].ID
				return true
			}
			return false
		})
	}
	return a
}

func TestCompletedControlEvidenceAndCompetingSubmissionsPreserveOtherTargets(t *testing.T) {
	mock := mockACPBinary(t)
	// Full production evidence pools require thousands of synchronous durable
	// commits. Extend only this test lifetime, never product capacity or deadlines.
	h := newCleanupProcessHarnessWithLifetime(t, 10*time.Minute)
	h.start("")
	a := startCapacityAgent(t, h, mock, "")
	b := startCapacityAgent(t, h, mock, "")
	c := startCapacityAgent(t, h, mock, "")
	gate, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer gate.Close()
	probe := startCapacityAgent(t, h, mock, gate.Addr().String())
	_ = gate.(*net.TCPListener).SetDeadline(time.Now().Add(3 * time.Second))
	blocked, err := gate.Accept()
	if err != nil {
		t.Fatal(err)
	}
	defer blocked.Close()
	var entered [1]byte
	if _, err := blocked.Read(entered[:]); err != nil {
		t.Fatal(err)
	}
	ended, registry := h.launch(mock)
	if err := testStopRuntime(h.client, h.ctx, ended); err != nil {
		t.Fatal(err)
	}
	capacity := func() api.SubmissionCapacity {
		t.Helper()
		var info api.MachineInfo
		if err := h.client.Call(h.ctx, "machine.info", struct{}{}, &info); err != nil || info.SubmissionCapacity == nil {
			t.Fatal(info, err)
		}
		return *info.SubmissionCapacity
	}
	historical := make(map[string]api.SubmissionRequest)
	next := make(map[string]int)
	fillCompleted := func(kind string) {
		t.Helper()
		used := capacity().Controls[kind].Used
		for ; used < sessionregistry.DefaultMaxControls; used++ {
			i := next[kind]
			next[kind]++
			id := fmt.Sprintf("historical-%s-%d", kind, i)
			action := api.ACPAction{Action: kind, PermissionID: id, OptionID: "allow"}
			if kind == "cancel" {
				action = api.ACPAction{Action: kind, OperationRef: id}
			}
			key := cleanupTestKey(ended, id)
			request := api.SubmissionRequest{SubmissionKey: key, Operation: "acp.action", Payload: api.Payload(action)}
			if i == 0 {
				historical[kind] = request
			}
			// Prepare retained historical facts through production registry
			// transitions. These fixtures do not claim historical Agent execution.
			if err := registry.ReserveControl(h.ctx, key.Target, kind, id); err != nil {
				t.Fatal(err)
			}
			claim, _, err := registry.ClaimControl(h.ctx, key, sessionregistry.Digest("acp.action", request.Payload), "acp:"+ended.Incarnation, kind, id)
			if err != nil || !claim.Acquired() {
				t.Fatal("historical fixture did not acquire its own key", err)
			}
			if _, err := registry.Accept(h.ctx, claim, id); err != nil {
				t.Fatal(err)
			}
			if _, err := registry.Progress(h.ctx, key, id, "written", "", nil); err != nil {
				t.Fatal(err)
			}
			if (used+1)%1024 == 0 {
				t.Logf("retained %s evidence: %d/%d", kind, used+1, sessionregistry.DefaultMaxControls)
			}
		}
	}
	fillCompleted("permission")
	fillCompleted("cancel")
	full := capacity()
	for _, kind := range []string{"permission", "cancel"} {
		u := full.Controls[kind]
		if u.Used != u.Limit || u.Limit != sessionregistry.DefaultMaxControls || u.Completed+u.Reserved != u.Used {
			t.Fatal("completed evidence did not fill the original budget", kind, u)
		}
	}
	// An already-admitted prompt asks for a new permission after evidence fills.
	// The host must return a resource error, without publishing or choosing it.
	if _, err := blocked.Write([]byte{1}); err != nil {
		t.Fatal(err)
	}
	if op, err := h.client.WaitAgentOperation(h.ctx, probe.runtime, api.AgentOperationWait{Ref: probe.prompt.Ref, TimeoutMS: 3000}); err != nil || !op.Terminal() {
		t.Fatal(op, err)
	}
	responses, err := os.ReadFile(filepath.Join(probe.work, "responses.log"))
	var rejected struct {
		Error struct {
			Code    int    `json:"code"`
			Message string `json:"message"`
		} `json:"error"`
		Result json.RawMessage `json:"result"`
	}
	if err != nil || json.Unmarshal(responses, &rejected) != nil || rejected.Error.Code != -32000 || !strings.Contains(rejected.Error.Message, "capacity unavailable") || len(rejected.Result) != 0 {
		t.Fatal("capacity exhaustion silently answered a new permission", string(responses), err)
	}
	if s, err := h.client.ACPState(h.ctx, probe.runtime); err != nil || len(s.Permissions) != 0 {
		t.Fatal("unreserved permission became publicly answerable", s, err)
	}
	waitTimeoutTest(t, func() bool { return capacity().Controls["cancel"].Reserved == 3 })
	fillCompleted("cancel") // replace only the probe's ended, unused reservation
	_, err = h.client.ACPSubmit(h.ctx, cleanupTestKey(probe.runtime, "cannot-reserve-cancel"), api.ACPAction{Action: "prompt", ExpectedConversationID: probe.conversation, Text: "must not execute"})
	requireSubmissionCode(t, err, "CONTROL_CAPACITY_EXHAUSTED")
	for i := capacity().Ordinary.Used; i < sessionregistry.DefaultMaxKeys; i++ {
		key := cleanupTestKey(ended, fmt.Sprintf("historical-rejected-%d", i))
		claim, _, err := registry.ClaimKey(h.ctx, key, sessionregistry.Digest("invalid", nil), "historical-fixture")
		if err != nil {
			t.Fatal(err)
		}
		if _, err := registry.Reject(h.ctx, claim, "INVALID_ARGUMENT"); err != nil {
			t.Fatal(err)
		}
	}
	before := capacity()
	h.kill()
	h.start("")
	for range 3 {
		if after := capacity(); !reflect.DeepEqual(before, after) {
			t.Fatal("reconnect/read changed retained evidence", before, after)
		}
	}
	for _, agent := range []capacityAgent{a, b, c} {
		if s, err := h.client.ACPState(h.ctx, agent.runtime); err != nil || len(s.Permissions) != 1 || s.Permissions[0].ID != agent.permission {
			t.Fatal("original effective target changed", s, err)
		}
	}
	// Four distinct valid submissions compete for one effective permission.
	// Only its winning key may acquire evidence; the others stay unknown.
	type attempted struct {
		key     api.SubmissionKey
		receipt api.SubmissionReceipt
		err     error
	}
	compete := func(agent capacityAgent, action api.ACPAction, prefix string) attempted {
		t.Helper()
		start := make(chan struct{})
		results := make(chan attempted, 4)
		var group sync.WaitGroup
		for i := range 4 {
			group.Go(func() {
				<-start
				key := cleanupTestKey(agent.runtime, fmt.Sprintf("%s-%d", prefix, i))
				r, err := h.client.ACPControl(h.ctx, key, action)
				results <- attempted{key, r, err}
			})
		}
		close(start)
		group.Wait()
		close(results)
		var winner attempted
		for result := range results {
			if result.err == nil {
				if winner.key.SubmissionID != "" || result.receipt.Stage != "written" {
					t.Fatal("competing controls acquired more than one result", result)
				}
				winner = result
			} else {
				requireSubmissionCode(t, result.err, "INVALID_ARGUMENT")
				if result.receipt.Admission != api.SubmissionUnknown {
					t.Fatal("losing control consumed evidence", result)
				}
			}
		}
		if winner.key.SubmissionID == "" {
			t.Fatal("effective control had no reserved capacity")
		}
		return winner
	}
	answer := api.ACPAction{Action: "permission", PermissionID: a.permission, OptionID: "allow"}
	winner := compete(a, answer, "answer")
	if op, err := h.client.WaitAgentOperation(h.ctx, a.runtime, api.AgentOperationWait{Ref: a.prompt.Ref, TimeoutMS: 3000}); err != nil || !op.Terminal() {
		t.Fatal(op, err)
	}
	waitTimeoutTest(t, func() bool { return capacity().Controls["cancel"].Reserved == 2 })
	fillCompleted("cancel") // only A's unused cancellation slot may be reused
	if usage := capacity().Controls["cancel"]; usage.Used != sessionregistry.DefaultMaxControls {
		t.Fatal("effective cancellation was not tested at the hard limit", usage)
	}
	cancel := api.ACPAction{Action: "cancel", OperationRef: b.prompt.Ref}
	cancelled := compete(b, cancel, "cancel")
	stable := capacity()
	// Repeated originals, altered payloads and expired targets run concurrently.
	// None may retain a new key, steal C's reservation or execute again.
	var storms sync.WaitGroup
	for worker := range 4 {
		storms.Go(func() {
			for i := range 20 {
				for _, item := range []struct {
					accepted attempted
					action   api.ACPAction
				}{{winner, answer}, {cancelled, cancel}} {
					r, err := h.client.ACPControl(h.ctx, item.accepted.key, item.action)
					if err != nil || r.OperationRef != item.accepted.receipt.OperationRef {
						t.Error("duplicate changed original control", r, err)
					}
					altered := item.action
					if altered.Action == "permission" {
						altered.OptionID = "reject"
					} else {
						altered.OperationRef = c.prompt.Ref
					}
					_, err = h.client.ACPControl(h.ctx, item.accepted.key, altered)
					var failure *api.Error
					if !errors.As(err, &failure) || failure.Code != "SUBMISSION_CONFLICT" {
						t.Error("altered control did not conflict", err)
					}
					fresh := item.accepted.key
					fresh.SubmissionID = fmt.Sprintf("expired-%s-%d-%d", item.action.Action, worker, i)
					r, err = h.client.ACPControl(h.ctx, fresh, item.action)
					if err == nil || r.Admission != api.SubmissionUnknown {
						t.Error("expired control consumed evidence", r, err)
					}
				}
			}
		})
	}
	storms.Wait()
	if after := capacity(); !reflect.DeepEqual(stable, after) {
		t.Fatal("duplicate/conflict/expired storm changed durable budgets", stable, after)
	}
	if s, err := h.client.ACPState(h.ctx, c.runtime); err != nil || len(s.Permissions) != 1 || s.Permissions[0].ID != c.permission {
		t.Fatal("other valid target lost its reservation", s, err)
	}
	if r, err := h.client.ACPControl(h.ctx, cleanupTestKey(c.runtime, "answer-c"), api.ACPAction{Action: "permission", PermissionID: c.permission, OptionID: "allow"}); err != nil || r.Stage != "written" {
		t.Fatal("other valid answer blocked", r, err)
	}
	for _, request := range historical {
		if r, err := h.client.Submit(h.ctx, request); err != nil || r.Stage != "written" || r.OperationRef != request.SubmissionID {
			t.Fatal("old retained duplicate was not preserved", r, err)
		}
	}
	for _, agent := range []capacityAgent{a, b, c, probe} {
		if r, err := h.client.Stop(h.ctx, cleanupTestKey(agent.runtime, "stop")); err != nil || r.Stage != "stopped" {
			t.Fatal("stop reservation consumed by other controls", r, err)
		}
		processes, err := os.ReadFile(filepath.Join(agent.work, "process.log"))
		if err != nil || strings.Count(string(processes), "\n") != 1 {
			t.Fatal("reconnect replaced Agent", string(processes), err)
		}
	}
	forget := cleanupTestKey(ended, "forget")
	if r, err := h.client.Forget(h.ctx, forget); err != nil || r.Admission != api.SubmissionAccepted {
		t.Fatal("retained control records consumed cleanup reservation", r, err)
	}
	waitTimeoutTest(t, func() bool {
		r, err := h.client.QuerySubmission(h.ctx, forget)
		return err == nil && r.Stage == "completed"
	})
	for _, request := range historical {
		if r, err := h.client.QuerySubmission(h.ctx, request.SubmissionKey); err != nil || r.Stage != "written" {
			t.Fatal("cleanup removed another control's evidence", r, err)
		}
	}
	for _, agent := range []capacityAgent{a, b, c} {
		responses, err := os.ReadFile(filepath.Join(agent.work, "responses.log"))
		if err != nil || strings.Count(string(responses), "\n") != 1 {
			t.Fatal("control sent extra permission responses", string(responses), err)
		}
		rpcs, err := os.ReadFile(filepath.Join(agent.work, "rpc.log"))
		wantCancel := 0
		if agent.runtime.ID == b.runtime.ID {
			wantCancel = 1
		}
		if err != nil || strings.Count(string(rpcs), "initialize\n") != 1 || strings.Count(string(rpcs), "session/new\n") != 1 || strings.Count(string(rpcs), "session/prompt\n") != 1 || strings.Count(string(rpcs), "session/cancel\n") != wantCancel {
			t.Fatal("control storm changed actual RPC counts", string(rpcs), err)
		}
	}
	after := capacity()
	if after.Ordinary.Used != sessionregistry.DefaultMaxKeys {
		t.Fatal("ordinary evidence was evicted", after)
	}
	for kind, u := range after.Controls {
		if u.Used > u.Limit || u.Used != u.Reserved+u.Claimed+u.Accepted+u.Rejected {
			t.Fatal("unbounded control evidence", kind, u)
		}
	}
	t.Logf("control capacity before=%+v after=%+v; one effective A answer, B cancel and C answer", before, after)
}
