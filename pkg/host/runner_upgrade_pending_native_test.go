package host

import (
	"context"
	"encoding/json"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/aiomni/dune/internal/wire"
	"github.com/aiomni/dune/pkg/api"
)

// The source release is instrumented at the executable entry point only. A Go
// build overlay keeps the pause out of product binaries and retains the actual
// installer, worker, tmux, registry and host entry implementation under test.
func nativePendingHostOverlay(t *testing.T) string {
	t.Helper()
	mainPath, err := filepath.Abs("../../cmd/dune/main.go")
	if err != nil {
		t.Fatal(err)
	}
	body, err := os.ReadFile(mainPath)
	if err != nil {
		t.Fatal(err)
	}
	source := strings.Replace(string(body), "import (", "import (\n\"net\"\n\"io\"\n\"path/filepath\"", 1)
	source = strings.Replace(source, "func main() {", "func main() {\n pauseNativeAcceptanceHost()", 1)
	source += `
func pauseNativeAcceptanceHost() {
	if len(os.Args) != 3 || os.Args[1] != "_acp_host" { return }
	body, err := os.ReadFile(filepath.Join(os.Args[2], "bootstrap.json"))
	if err != nil { panic(err) }
	var boot struct {
		Profile struct { Env map[string]string }
		Registration struct { Runtime json.RawMessage }
	}
	if json.Unmarshal(body, &boot) != nil { panic("invalid test bootstrap") }
	address := boot.Profile.Env["DUNE_NATIVE_ENTRY_GATE"]
	if address == "" { return }
	connection, err := net.DialTimeout("tcp", address, 5*time.Second)
	if err != nil { panic(err) }
	defer connection.Close()
	connection.SetDeadline(time.Now().Add(6*time.Minute))
	if err := json.NewEncoder(connection).Encode(map[string]any{"pid": os.Getpid(), "runtime": boot.Registration.Runtime}); err != nil { panic(err) }
	var resume [1]byte
	if _, err := io.ReadFull(connection, resume[:]); err != nil { panic(err) }
}
`
	directory := t.TempDir()
	instrumented := filepath.Join(directory, "main.go")
	if err := os.WriteFile(instrumented, []byte(source), 0600); err != nil {
		t.Fatal(err)
	}
	overlay := filepath.Join(directory, "overlay.json")
	encoded, err := json.Marshal(map[string]any{"Replace": map[string]string{mainPath: instrumented}})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(overlay, encoded, 0600); err != nil {
		t.Fatal(err)
	}
	return overlay
}

type nativePendingHost struct {
	key     api.SubmissionKey
	runtime api.Runtime
	pid     int
	gate    net.Conn
	rpcLog  string
	entered bool
}

func startNativePendingHost(t *testing.T, ctx context.Context, agent *agentConnection, profile api.Profile, rpcLog string) *nativePendingHost {
	t.Helper()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	profile.Env = map[string]string{"DUNE_NATIVE_ENTRY_GATE": listener.Addr().String(), "DUNE_MOCK_RPC_LOG": rpcLog}
	late := &nativePendingHost{key: api.SubmissionKey{SubmissionID: wire.ID(), Target: agent.target}, rpcLog: rpcLog}
	finished := make(chan error, 1)
	go func() {
		_, stream, err := agent.sdk.Start(ctx, api.StartRequest{SubmissionKey: late.key, Profile: profile})
		if stream != nil {
			stream.Close()
		}
		finished <- err
	}()
	_ = listener.(*net.TCPListener).SetDeadline(time.Now().Add(15 * time.Second))
	late.gate, err = listener.Accept()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { late.gate.Close() })
	_ = late.gate.SetDeadline(time.Now().Add(6 * time.Minute))
	var entered struct {
		PID     int         `json:"pid"`
		Runtime api.Runtime `json:"runtime"`
	}
	if err := json.NewDecoder(late.gate).Decode(&entered); err != nil {
		t.Fatal(err)
	}
	late.runtime, late.pid = entered.Runtime, entered.PID
	select {
	case err := <-finished:
		if err == nil || !strings.Contains(err.Error(), "RESULT_UNKNOWN") {
			t.Fatal("pending host was incorrectly confirmed", err)
		}
	case <-ctx.Done():
		t.Fatal(ctx.Err())
	}
	receipt, err := agent.sdk.QuerySubmission(ctx, late.key)
	if err != nil || receipt.Admission != api.SubmissionAccepted || receipt.Stage != "host_starting" || receipt.Runtime == nil || receipt.Runtime.ID != late.runtime.ID {
		t.Fatal("pending launch lost durable admission", receipt, err)
	}
	t.Logf("v1 host %d durably admitted; execution paused before registry entry", late.pid)
	return late
}

func (late *nativePendingHost) resume(t *testing.T) {
	t.Helper()
	if _, err := late.gate.Write([]byte{1}); err != nil {
		t.Fatal(err)
	}
	late.entered = true
}

func (late *nativePendingHost) verify(t *testing.T, ctx context.Context, agent *agentConnection) {
	t.Helper()
	if !late.entered {
		receipt, err := agent.sdk.QuerySubmission(ctx, late.key)
		if err != nil || receipt.Admission != api.SubmissionAccepted || receipt.Stage != "host_starting" {
			t.Fatal("upgrade terminated pending original launch", receipt, err)
		}
		if _, err := os.Stat(late.rpcLog); !os.IsNotExist(err) {
			t.Fatal("pending host created an Agent before entry", err)
		}
		return
	}
	deadline := time.Now().Add(10 * time.Second)
	for {
		state, err := agent.State(ctx, late.runtime)
		if err == nil && state.Ready {
			break
		}
		if time.Now().After(deadline) || ctx.Err() != nil {
			t.Fatal("original pending host failed to enter", err)
		}
		time.Sleep(100 * time.Millisecond)
	}
	running, err := agent.Get(ctx, late.runtime)
	if err != nil || running.ACPHost == nil || running.ACPHost.HostPID != late.pid || running.ACPHost.Build.Version != "v1" {
		t.Fatal("pending host was replaced instead of resumed", running, err)
	}
	conversation := running.ConversationID
	if conversation == "" {
		opened, err := agent.Submit(ctx, late.runtime, AgentAction{SubmissionID: wire.ID(), Action: "new", Cwd: running.WorkingDirectory})
		if err == nil {
			opened, err = agent.WaitOperation(ctx, late.runtime, api.AgentOperationWait{Ref: opened.Ref, TimeoutMS: 5000})
		}
		if err != nil || opened.State != "completed" {
			t.Fatal(opened, err)
		}
		conversation = opened.ConversationID
	}
	operation, err := agent.Submit(ctx, late.runtime, AgentAction{SubmissionID: wire.ID(), Action: "prompt", Text: "permission for originally pending v1 host", ExpectedConversationID: conversation})
	if err != nil {
		t.Fatal(err)
	}
	var permission string
	for time.Now().Before(deadline) && ctx.Err() == nil {
		state, err := agent.sdk.ACPState(ctx, late.runtime)
		if err == nil && len(state.Permissions) == 1 {
			permission = state.Permissions[0].ID
			break
		}
		time.Sleep(50 * time.Millisecond)
	}
	if permission == "" {
		t.Fatal("pending v1 host did not preserve permission semantics")
	}
	target := agent.target
	target.RuntimeID, target.RuntimeIncarnation, target.RuntimeGeneration = late.runtime.ID, late.runtime.Incarnation, late.runtime.Generation
	_, err = agent.sdk.ACPControl(ctx, api.SubmissionKey{SubmissionID: wire.ID(), Target: target}, api.ACPAction{Action: "permission", PermissionID: permission, OptionID: "allow"})
	if err != nil {
		t.Fatal(err)
	}
	operation, err = agent.WaitOperation(ctx, late.runtime, api.AgentOperationWait{Ref: operation.Ref, TimeoutMS: 5000})
	if err != nil || operation.State != "completed" {
		t.Fatal("old pending host cannot write/query current state", operation, err)
	}
	body, err := os.ReadFile(late.rpcLog)
	if err != nil || strings.Count(string(body), "initialize\n") != 1 || strings.Count(string(body), "session/new\n") != 1 {
		t.Fatal("old pending host replayed initialization", fmt.Sprint(err), string(body))
	}
}
