package tests

import (
	"errors"
	"io"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/aiomni/dune/internal/process"
	"github.com/aiomni/dune/internal/sessionregistry"
	"github.com/aiomni/dune/pkg/api"
)

func TestHostLossUsesIndependentEvidenceAndDoesNotReplay(t *testing.T) {
	h := start(t)
	mock := mockACPBinary(t)
	gate, err := net.Listen("tcp", "127.0.0.1:0")
	must(t, err)
	defer gate.Close()
	p := profile(h.dir, "acp", mock)
	p.ManagedACP = true
	log := filepath.Join(h.dir, "process.log")
	p.Env = map[string]string{"DUNE_MOCK_PROCESS_LOG": log, "DUNE_MOCK_PROMPT_GATE": gate.Addr().String()}
	runtime, stream, err := testStartProfile(h.client, h.ctx, p)
	must(t, err)
	stream.Close()
	waitManagedACPReady(t, h, runtime)
	opened, err := testACPSubmit(h.client, h.ctx, runtime, api.ACPAction{Action: "new", Cwd: h.dir})
	must(t, err)
	opened, err = h.client.WaitAgentOperation(h.ctx, runtime, api.AgentOperationWait{Ref: opened.Ref, TimeoutMS: 3000})
	must(t, err)
	if opened.State != "completed" {
		t.Fatal(opened)
	}
	state, err := h.client.ACPState(h.ctx, runtime)
	must(t, err)
	key := api.SubmissionKey{SubmissionID: "task-before-host-loss", Target: api.SubmissionTarget{OwnerID: "standalone-owner", RunnerID: "standalone-runner", FabricID: "standalone-fabric", MachineID: h.c.Target, BindingRevision: 1, RuntimeID: runtime.ID, RuntimeIncarnation: runtime.Incarnation, RuntimeGeneration: runtime.Generation}}
	accepted, err := h.client.ACPSubmit(h.ctx, key, api.ACPAction{Action: "prompt", Text: "barrier task", ExpectedConversationID: state.Conversation.ID})
	must(t, err)
	_ = gate.(*net.TCPListener).SetDeadline(time.Now().Add(5 * time.Second))
	blocked, err := gate.Accept()
	must(t, err)
	defer blocked.Close()
	var entered [1]byte
	_, err = blocked.Read(entered[:])
	must(t, err)
	registry, err := sessionregistry.Open(h.ctx, filepath.Join(h.c.SessionDir, "registry"), sessionregistry.Options{})
	must(t, err)
	defer registry.Close()
	record, err := registry.Host(h.ctx, key.Target)
	must(t, err)
	if record.PID <= 1 || record.GroupID <= 1 || record.GroupGeneration != 1 {
		t.Fatal("missing pre-execution evidence", record)
	}
	// tmux resumes stopped pane children. Suspend this test's private supervisor
	// first, then verify the host really entered the stopped state.
	parentText, err := exec.Command("ps", "-o", "ppid=", "-p", strconv.Itoa(record.PID)).Output()
	must(t, err)
	parent, err := strconv.Atoi(strings.TrimSpace(string(parentText)))
	must(t, err)
	command, err := exec.Command("ps", "-o", "comm=", "-p", strconv.Itoa(parent)).Output()
	must(t, err)
	if parent <= 1 || !strings.Contains(string(command), "tmux") {
		t.Fatal("isolated host parent is not its tmux supervisor", string(command))
	}
	must(t, syscall.Kill(parent, syscall.SIGSTOP))
	parentSuspended := true
	defer func() {
		if parentSuspended {
			_ = syscall.Kill(parent, syscall.SIGCONT)
		}
	}()
	must(t, syscall.Kill(record.PID, syscall.SIGSTOP))
	suspended := true
	defer func() {
		if suspended {
			_ = syscall.Kill(record.PID, syscall.SIGCONT)
		}
	}()
	for deadline := time.Now().Add(3 * time.Second); ; {
		state, err := exec.Command("ps", "-o", "state=", "-p", strconv.Itoa(record.PID)).Output()
		must(t, err)
		if strings.Contains(string(state), "T") {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("host did not enter the suspended state", string(state))
		}
	}
	listed, err := h.client.List(h.ctx)
	must(t, err)
	if len(listed.Items) != 1 || listed.Items[0].ID != runtime.ID || listed.Items[0].State != "running" || listed.Items[0].Availability != "unavailable" {
		t.Fatal("IPC timeout was treated as process loss", listed)
	}
	must(t, syscall.Kill(record.PID, syscall.SIGKILL))
	suspended = false
	must(t, syscall.Kill(parent, syscall.SIGCONT))
	parentSuspended = false
	for deadline := time.Now().Add(5 * time.Second); ; time.Sleep(5 * time.Millisecond) {
		absent, err := process.Absent(record.BootID, record.PID, record.GroupID)
		must(t, err)
		if absent {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("guardian did not clean the original Agent group")
		}
	}
	_ = blocked.SetReadDeadline(time.Now().Add(time.Second))
	if _, err := blocked.Read(entered[:]); err != io.EOF {
		t.Fatal("Agent continued its blocked task after host loss", err)
	}
	// The independent record survives missing Runtime metadata and discovery
	// must not manufacture a new host to fill that hole.
	directory := filepath.Join(h.c.SessionDir, "acp", "runtimes", runtime.ID)
	must(t, os.RemoveAll(directory))
	h.client.Close()
	h.stopProcess("fabricd", syscall.SIGKILL)
	h.startProcess("fabricd")
	h.reconnect()
	listed, err = h.client.List(h.ctx)
	must(t, err)
	if len(listed.Items) != 1 || listed.Items[0].ID != runtime.ID || listed.Items[0].Incarnation != runtime.Incarnation || listed.Items[0].State != "lost" || listed.Items[0].Availability != "lost" || listed.Items[0].ExitCode != nil {
		t.Fatal("lost original identity vanished or acquired a fabricated exit", listed)
	}
	receipt, err := h.client.QuerySubmission(h.ctx, key)
	must(t, err)
	if receipt.Admission != api.SubmissionAccepted || receipt.OperationRef != accepted.Ref {
		t.Fatal("loss changed original task admission", receipt)
	}
	_, err = h.client.WaitAgentOperation(h.ctx, runtime, api.AgentOperationWait{Ref: accepted.Ref})
	var failure *api.Error
	if !errors.As(err, &failure) || failure.Code != "SESSION_LOST" {
		t.Fatal("lost task acquired a fabricated result", err)
	}
	if _, err := os.Stat(directory); !os.IsNotExist(err) {
		t.Fatal("read-only discovery recreated Runtime resources", err)
	}
	data, err := os.ReadFile(log)
	must(t, err)
	if strings.Count(strings.TrimSpace(string(data)), "\n")+1 != 1 {
		t.Fatal("loss discovery started another Agent", string(data))
	}
}
