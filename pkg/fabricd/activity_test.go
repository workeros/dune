package fabricd

import (
	"bufio"
	"encoding/json"
	"io"
	"sync"
	"testing"
	"time"

	"github.com/aiomni/dune/internal/process"
	"github.com/aiomni/dune/pkg/api"
)

func TestActivityTracksACPWithoutContentSubscription(t *testing.T) {
	reader, writer := io.Pipe()
	defer reader.Close()
	defer writer.Close()
	r := &runtime{id: "runtime", inc: "boot", adapter: "acp", cwd: "/tmp", subs: map[*subscription]bool{}, p: &process.Process{Input: writer}}
	a := newACPController(r)
	r.acp = a
	finish := make(chan struct{})
	var finishOnce sync.Once
	finishPrompt := func() { finishOnce.Do(func() { close(finish) }) }
	var responders sync.WaitGroup
	defer func() { finishPrompt(); responders.Wait() }()
	go func() {
		scanner := bufio.NewScanner(reader)
		for scanner.Scan() {
			var message struct {
				ID     string `json:"id"`
				Method string `json:"method"`
			}
			if json.Unmarshal(scanner.Bytes(), &message) != nil {
				return
			}
			var result any
			switch message.Method {
			case "initialize":
				result = map[string]any{"protocolVersion": 1, "agentInfo": map[string]string{"name": "test-agent"}}
			case "session/new":
				result = map[string]string{"sessionId": "native"}
			case "session/prompt":
				a.receive([]byte(`{"jsonrpc":"2.0","id":"permission","method":"session/request_permission","params":{"sessionId":"native","options":[{"optionId":"allow"}]}}`))
				responders.Go(func() {
					<-finish
					a.receive(api.Payload(map[string]any{"id": message.ID, "result": map[string]string{"stopReason": "end_turn"}}))
				})
				continue
			default:
				continue
			}
			a.receive(api.Payload(map[string]any{"id": message.ID, "result": result}))
		}
	}()
	awaitActivity := func(want string) api.AgentActivity {
		t.Helper()
		deadline := time.Now().Add(3 * time.Second)
		for time.Now().Before(deadline) {
			activity := *r.info().Activity
			if activity.State == want {
				return activity
			}
			time.Sleep(time.Millisecond)
		}
		t.Fatalf("activity=%+v, want %s", r.info().Activity, want)
		return api.AgentActivity{}
	}
	initial := awaitActivity("working")
	a.initialize()
	awaitActivity("idle")
	if _, err := a.action(acpAction{Action: "new"}); err != nil {
		t.Fatal(err)
	}
	awaitActivity("idle")
	if _, err := a.action(acpAction{Action: "prompt", Text: "a task"}); err != nil {
		t.Fatal(err)
	}
	blocked := awaitActivity("blocked")
	if blocked.Source != "acp" || blocked.Agent != "test-agent" || blocked.Epoch != initial.Epoch || blocked.Sequence <= initial.Sequence {
		t.Fatal(blocked)
	}
	permissions := a.snapshot().Permissions
	if len(permissions) != 1 {
		t.Fatal(permissions)
	}
	if _, err := a.action(acpAction{Action: "permission", PermissionID: permissions[0].ID, OptionID: "allow"}); err != nil {
		t.Fatal(err)
	}
	awaitActivity("working")
	finishPrompt()
	final := awaitActivity("idle")
	if final.Sequence <= blocked.Sequence || len(r.subs) != 0 {
		t.Fatal("summary required a transcript subscription", final)
	}
	r.finish(0)
	if info := r.info(); info.State != "exited" || info.Activity.State != "unknown" || info.Activity.Sequence <= final.Sequence {
		t.Fatal(info)
	}
}

func TestPTYActivityDoesNotInferTaskCompletion(t *testing.T) {
	r := &runtime{id: "runtime", inc: "persistent-pty", adapter: "pty"}
	r.observeForeground("claude")
	first := r.info().Activity
	if first.State != "unknown" || first.Agent != "claude" || first.Foreground != "claude" {
		t.Fatal(first)
	}
	r.observeForeground("claude")
	if r.info().Activity.Sequence != first.Sequence {
		t.Fatal("polling created false activity")
	}
	r.observeForeground("sh")
	second := r.info().Activity
	if second.Agent != "" || second.State != "unknown" || second.Sequence <= first.Sequence {
		t.Fatal("shell retained Agent state", second)
	}
	restored := &runtime{id: r.id, inc: r.inc, adapter: "pty"}
	if restored.info().Activity.Epoch == first.Epoch {
		t.Fatal("new fabricd reused old event epoch")
	}
}
