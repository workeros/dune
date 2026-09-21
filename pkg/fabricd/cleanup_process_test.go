package fabricd

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/aiomni/dune/internal/process"
	"github.com/aiomni/dune/internal/sessionregistry"
	"github.com/aiomni/dune/internal/tmux"
	"github.com/aiomni/dune/pkg/access"
	"github.com/aiomni/dune/pkg/api"
	"github.com/aiomni/dune/pkg/client"
	"github.com/aiomni/dune/pkg/gateway"
)

// This subprocess uses the actual engine/guardian/host and Gateway protocol.
// Fault control exists only in the test executable, never in a product endpoint.
func TestCleanupProcessHelper(t *testing.T) {
	address := os.Getenv("DUNE_CLEANUP_TEST_CONTROL")
	if address == "" {
		t.Skip("cleanup fault subprocess only")
	}
	control, err := net.Dial("tcp", address)
	if err != nil {
		t.Fatal(err)
	}
	defer control.Close()
	_, err = fmt.Fprintln(control, "ready")
	if err != nil {
		t.Fatal(err)
	}
	engine, err := openWithCleanupBarrier(t.Context(), os.Getenv("DUNE_CLEANUP_TEST_STATE"), func(_ api.SubmissionKey, point string) error {
		if point != os.Getenv("DUNE_CLEANUP_TEST_POINT") {
			return nil
		}
		if _, err := fmt.Fprintln(control, point); err != nil {
			return err
		}
		var resume [1]byte
		_, err := io.ReadFull(control, resume[:])
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
	defer engine.Close()
	conn, err := net.Dial("tcp", os.Getenv("DUNE_CLEANUP_TEST_DAEMON"))
	if err != nil {
		t.Fatal(err)
	}
	_ = engine.ServeConn(t.Context(), conn, "cleanup-test")
}

type cleanupProcessHarness struct {
	t        *testing.T
	ctx      context.Context
	state    string
	g        *gateway.Gateway
	client   *client.Client
	cmd      *exec.Cmd
	control  net.Conn
	events   *bufio.Reader
	log      *os.File
	lifetime time.Duration
	program  string
}

func newCleanupProcessHarness(t *testing.T) *cleanupProcessHarness {
	t.Helper()
	return newCleanupProcessHarnessWithLifetime(t, 80*time.Second)
}

func newCleanupProcessHarnessWithLifetime(t *testing.T, lifetime time.Duration) *cleanupProcessHarness {
	t.Helper()
	ctx, cancel := context.WithTimeout(t.Context(), lifetime)
	h := &cleanupProcessHarness{t: t, ctx: ctx, lifetime: lifetime, state: filepath.Join(t.TempDir(), "state"), g: gateway.New()}
	var err error
	h.log, err = os.Create(filepath.Join(t.TempDir(), "process.log"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		h.kill()
		h.g.Close()
		cancel()
		for _, dir := range []string{h.state, filepath.Join(h.state, "acp")} {
			if s, err := tmux.Open(dir); err == nil {
				_ = s.Close()
			}
		}
		h.log.Close()
		body, _ := os.ReadFile(h.log.Name())
		if strings.Contains(string(body), "DATA RACE") {
			t.Error("cleanup subprocess race", string(body))
		}
		if t.Failed() {
			t.Log(string(body))
		}
	})
	return h
}

func (h *cleanupProcessHarness) start(point string) {
	h.t.Helper()
	control, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		h.t.Fatal(err)
	}
	defer control.Close()
	daemon, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		h.t.Fatal(err)
	}
	defer daemon.Close()
	program := h.program
	if program == "" {
		program = os.Args[0]
	}
	h.cmd = exec.Command(program, "-test.run=^TestCleanupProcessHelper$", "-test.timeout="+(h.lifetime+10*time.Second).String())
	h.cmd.Env = append(os.Environ(), "DUNE_CLEANUP_TEST_CONTROL="+control.Addr().String(), "DUNE_CLEANUP_TEST_DAEMON="+daemon.Addr().String(), "DUNE_CLEANUP_TEST_STATE="+h.state, "DUNE_CLEANUP_TEST_POINT="+point)
	h.cmd.Stdout, h.cmd.Stderr = h.log, h.log
	if err := h.cmd.Start(); err != nil {
		h.t.Fatal(err)
	}
	_ = control.(*net.TCPListener).SetDeadline(time.Now().Add(10 * time.Second))
	h.control, err = control.Accept()
	if err != nil {
		h.t.Fatal(err)
	}
	h.events = bufio.NewReader(h.control)
	h.point("ready")
	_ = daemon.(*net.TCPListener).SetDeadline(time.Now().Add(10 * time.Second))
	conn, err := daemon.Accept()
	if err != nil {
		h.t.Fatal(err)
	}
	binding, handler, err := (access.Grant{Target: "cleanup-test", Role: gateway.RoleDaemon}).Bind()
	if err != nil {
		h.t.Fatal(err)
	}
	go h.g.ServeConn(h.ctx, conn, binding, handler)
	waitTimeoutTest(h.t, func() bool { return h.g.Online("cleanup-test") })
	left, right := net.Pipe()
	binding, handler, err = (access.Grant{Target: "cleanup-test", Role: gateway.RoleSDK}).Bind()
	if err != nil {
		h.t.Fatal(err)
	}
	go h.g.ServeConn(h.ctx, left, binding, handler)
	h.client, err = client.Connect(h.ctx, right, "cleanup-test")
	if err != nil {
		h.t.Fatal(err)
	}
}

func (h *cleanupProcessHarness) kill() {
	if h.client != nil {
		h.client.Close()
		h.client = nil
	}
	if h.cmd != nil {
		_ = h.cmd.Process.Kill()
		_ = h.cmd.Wait()
		h.cmd = nil
	}
	if h.control != nil {
		h.control.Close()
		h.control = nil
	}
}

func (h *cleanupProcessHarness) point(want string) {
	h.t.Helper()
	_ = h.control.SetReadDeadline(time.Now().Add(10 * time.Second))
	got, err := h.events.ReadString('\n')
	if err != nil || strings.TrimSpace(got) != want {
		h.t.Fatalf("cleanup barrier: got %q, want %q: %v", got, want, err)
	}
}

func cleanupTestKey(runtime api.Runtime, id string) api.SubmissionKey {
	return api.SubmissionKey{SubmissionID: id, Target: api.SubmissionTarget{OwnerID: "test-owner", RunnerID: "test-runner", FabricID: "test-fabric", MachineID: "cleanup-test", BindingRevision: 1, RuntimeID: runtime.ID, RuntimeIncarnation: runtime.Incarnation, RuntimeGeneration: runtime.Generation}}
}

func (h *cleanupProcessHarness) launch(argv ...string) (api.Runtime, *sessionregistry.Registry) {
	h.t.Helper()
	if len(argv) == 0 {
		argv = []string{"/bin/sleep", "300"}
	}
	runtime, stream, err := testStartProfile(h.client, h.ctx, api.Profile{Version: 1, Kind: "agent", Adapter: "acp", ManagedACP: true, WorkingDirectory: h.t.TempDir(), Start: api.Command{Argv: argv}})
	if err != nil {
		h.t.Fatal(err)
	}
	stream.Close()
	registry, err := sessionregistry.Open(h.ctx, filepath.Join(h.state, "registry"), sessionregistry.Options{})
	if err != nil {
		h.t.Fatal(err)
	}
	h.t.Cleanup(func() { registry.Close() })
	return runtime, registry
}

func TestForgetResumesOnlyOriginalCleanupAfterProcessCrashes(t *testing.T) {
	for _, point := range []string{"accepted", "host:removed", "ipc:quarantined", "runtime_directory:contents_removed", "runtime_directory:marker_removed", "runtime_directory:removed"} {
		t.Run(point, func(t *testing.T) {
			h := newCleanupProcessHarness(t)
			h.start(point)
			runtime, registry := h.launch()
			key := cleanupTestKey(runtime, "original-forget")
			projectFile := filepath.Join(runtime.WorkingDirectory, "agent-native-history")
			if err := os.WriteFile(projectFile, []byte("preserve original native history"), 0600); err != nil {
				t.Fatal(err)
			}
			if err := testStopRuntime(h.client, h.ctx, runtime); err != nil {
				t.Fatal(err)
			}
			waitTimeoutTest(t, func() bool {
				host, err := registry.Host(h.ctx, key.Target)
				return err == nil && host.Phase == "exited"
			})
			first := h.client
			returned := make(chan error, 1)
			go func() { _, err := first.Forget(h.ctx, key); returned <- err }()
			h.point(point)
			if point == "accepted" {
				_, err := h.client.ACPSubmit(h.ctx, cleanupTestKey(runtime, "late-business"), api.ACPAction{Action: "list"})
				var failure *api.Error
				if !errors.As(err, &failure) || failure.Code != "STALE_RUNTIME" {
					t.Fatal("late host request crossed cleanup seal", err)
				}
			}
			before, err := h.client.QuerySubmission(h.ctx, key)
			if err != nil || before.Admission != api.SubmissionAccepted || before.Stage != "cleaning" || before.Cleanup == nil {
				t.Fatal("missing pre-deletion admission", before, err)
			}
			if point == "runtime_directory:removed" {
				if _, err := os.Lstat(filepath.Join(h.state, "acp", "runtimes", runtime.ID)); !os.IsNotExist(err) {
					t.Fatal("final barrier preceded physical deletion", err)
				}
			}
			h.kill()
			<-returned
			waitTimeoutTest(t, func() bool { return !h.g.Online("cleanup-test") })
			resumePoint := before.Cleanup.Remaining[0] + ":before"
			h.start(resumePoint)
			h.point(resumePoint)
			// The replacement executor is paused. Public reads must not advance
			// any steps or create a replacement host/Agent.
			for range 3 {
				observed, err := h.client.QuerySubmission(h.ctx, key)
				if err != nil || !reflect.DeepEqual(observed, before) {
					t.Fatal("query advanced or lost original cleanup", observed, before, err)
				}
			}
			if _, err := h.control.Write([]byte{1}); err != nil {
				t.Fatal(err)
			}
			var completed api.SubmissionReceipt
			waitTimeoutTest(t, func() bool {
				completed, err = h.client.QuerySubmission(h.ctx, key)
				return err == nil && completed.Stage == "completed"
			})
			if completed.OperationRef != before.OperationRef || len(completed.Cleanup.Remaining) != 0 {
				t.Fatal(completed)
			}
			duplicate, err := h.client.Forget(h.ctx, key)
			if err != nil || !reflect.DeepEqual(duplicate, completed) {
				t.Fatal("duplicate cleanup changed original receipt", duplicate, err)
			}
			listed, err := h.client.List(h.ctx)
			if err != nil || len(listed.Items) != 0 {
				t.Fatal("completed Runtime still discovered", listed, err)
			}
			data, err := os.ReadFile(projectFile)
			if err != nil || string(data) != "preserve original native history" {
				t.Fatal("cleanup removed Agent history", err)
			}
		})
	}
}

func TestForgetConfirmedLostHostNeedsNoStopOrReplacement(t *testing.T) {
	h := newCleanupProcessHarness(t)
	mock := mockACPBinary(t)
	h.start("")
	runtime, registry := h.launch(mock)
	waitTimeoutTest(t, func() bool { state, err := h.client.ACPState(h.ctx, runtime); return err == nil && state.Ready })
	occupied := cleanupTestKey(runtime, "host-owned-key")
	opened, err := h.client.ACPSubmit(h.ctx, occupied, api.ACPAction{Action: "new", Cwd: runtime.WorkingDirectory})
	if err != nil {
		t.Fatal(err)
	}
	opened, err = h.client.WaitAgentOperation(h.ctx, runtime, api.AgentOperationWait{Ref: opened.Ref, TimeoutMS: 3000})
	if err != nil || opened.State != "completed" {
		t.Fatal(opened, err)
	}
	original, err := h.client.QuerySubmission(h.ctx, occupied)
	if err != nil || original.Admission != api.SubmissionAccepted {
		t.Fatal(original, err)
	}
	conflict := func() {
		t.Helper()
		_, err := h.client.Forget(h.ctx, occupied)
		var failure *api.Error
		if !errors.As(err, &failure) || failure.Code != "SUBMISSION_CONFLICT" {
			t.Fatal("forget reused a host-owned key", err)
		}
	}
	conflict()
	key := cleanupTestKey(runtime, "forget-after-loss")
	if receipt, err := h.client.Forget(h.ctx, key); err == nil || receipt.Admission != api.SubmissionUnknown {
		t.Fatal("live Runtime was admitted for cleanup", receipt, err)
	}
	host, err := registry.Host(h.ctx, key.Target)
	if err != nil {
		t.Fatal(err)
	}
	if err := syscall.Kill(host.PID, syscall.SIGKILL); err != nil {
		t.Fatal(err)
	}
	waitTimeoutTest(t, func() bool {
		absent, err := process.Absent(host.BootID, host.PID, host.GroupID)
		return err == nil && absent
	})
	conflict()
	receipt, err := h.client.Forget(h.ctx, key)
	if err != nil || receipt.Stage != "completed" {
		t.Fatal("confirmed loss required a stop or host acknowledgement", receipt, err)
	}
	if _, err := os.Lstat(host.Resources.Directory.Path); !os.IsNotExist(err) {
		t.Fatal(err)
	}
	if _, err := os.Lstat(host.Resources.Socket.Path); !os.IsNotExist(err) {
		t.Fatal(err)
	}
	stored, err := registry.Host(h.ctx, key.Target)
	if err != nil || stored.Instance != host.Instance || stored.GroupGeneration != host.GroupGeneration {
		t.Fatal("cleanup replaced the original host", stored, err)
	}
	observed, err := h.client.QuerySubmission(h.ctx, occupied)
	if err != nil || !reflect.DeepEqual(observed, original) {
		t.Fatal("cleanup rewrote another operation's result", observed, err)
	}
}
