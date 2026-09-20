package feishu

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/aiomni/dune/im/channel"
	"github.com/aiomni/dune/im/duneagent"
	"github.com/aiomni/dune/pkg/api"
	"github.com/aiomni/dune/pkg/fabricd"
	"github.com/aiomni/dune/pkg/host"
	"github.com/aiomni/dune/pkg/identity"
	"github.com/aiomni/dune/pkg/managed"
	"github.com/aiomni/dune/pkg/runner"
	"github.com/aiomni/dune/pkg/transport/ws"
)

func TestMain(m *testing.M) {
	if code, handled := fabricd.RunHelper(os.Args[1:]); handled {
		os.Exit(code)
	}
	if os.Getenv("DUNE_IM_TEST_ACP") == "1" {
		runIMTestACP()
		os.Exit(0)
	}
	os.Exit(m.Run())
}

// This is an actual stdio subprocess, not a model. Its small on-disk counter
// makes new/load/attach and conversation isolation observable in the answer.
func runIMTestACP() {
	input := bufio.NewScanner(os.Stdin)
	input.Buffer(make([]byte, 4096), 256*1024)
	output := json.NewEncoder(os.Stdout)
	var session string
	turn := 0
	for input.Scan() {
		var request struct {
			ID     json.RawMessage `json:"id"`
			Method string          `json:"method"`
			Params struct {
				SessionID string                  `json:"sessionId"`
				Prompt    []struct{ Text string } `json:"prompt"`
			} `json:"params"`
		}
		if json.Unmarshal(input.Bytes(), &request) != nil {
			os.Exit(2)
		}
		var result any
		switch request.Method {
		case "initialize":
			result = map[string]any{"protocolVersion": 1, "agentCapabilities": map[string]bool{"loadSession": true}}
		case "session/new":
			session = fmt.Sprintf("im-test-%d", os.Getpid())
			result = map[string]string{"sessionId": session}
		case "session/load":
			session = request.Params.SessionID
			data, err := os.ReadFile(session + ".json")
			if err != nil || json.Unmarshal(data, &turn) != nil {
				os.Exit(3)
			}
			result = map[string]any{}
		case "session/prompt":
			turn++
			data, _ := json.Marshal(turn)
			if os.WriteFile(session+".json", data, 0600) != nil {
				os.Exit(4)
			}
			for _, update := range []struct{ kind, text string }{
				{"agent_thought_chunk", "private thought"},
				{"agent_message_chunk", fmt.Sprintf("turn %d: ", turn)},
				{"agent_message_chunk", request.Params.Prompt[0].Text},
			} {
				_ = output.Encode(map[string]any{"jsonrpc": "2.0", "method": "session/update", "params": map[string]any{
					"sessionId": session, "update": map[string]any{"sessionUpdate": update.kind, "content": map[string]string{"type": "text", "text": update.text}},
				}})
			}
			result = map[string]string{"stopReason": "end_turn"}
		default:
			continue
		}
		_ = output.Encode(map[string]any{"jsonrpc": "2.0", "id": request.ID, "result": result})
	}
}

type gatewayManagedFixture struct {
	managed.Service
	access managed.RunnerAccess
}

func (m *gatewayManagedFixture) BindRunnerAccess(access managed.RunnerAccess) error {
	m.access = access
	return nil
}

type gatewayScopeFixture struct {
	scope   host.AgentScope
	profile api.Profile
}

func (f gatewayScopeFixture) ResolveAgentProfile(context.Context, channel.ConversationSession) (api.Profile, error) {
	return f.profile, nil
}

func (f gatewayScopeFixture) ResolveAgentScope(_ context.Context, conversation channel.ConversationSession) (host.AgentScope, error) {
	if conversation.Key.TenantID != "tenant-a" || conversation.Target.RunnerID != f.scope.Binding.RunnerID {
		return host.AgentScope{}, fmt.Errorf("unexpected test conversation scope")
	}
	return f.scope, nil
}

// Assemble through public host/managed/fabricd APIs. The connector uses an
// actual WebSocket; AgentExecutor traverses Gateway and its authorization.
func gatewayAgentFixture(t *testing.T) (duneagent.Backend, channel.AgentTarget) {
	t.Helper()
	if os.Getenv("DUNE_TMUX") == "" {
		binary, err := filepath.Abs("../../bin/tmux")
		if err != nil {
			t.Fatal(err)
		}
		t.Setenv("DUNE_TMUX", binary)
	}
	ctx, cancel := context.WithTimeout(t.Context(), 45*time.Second)
	t.Cleanup(cancel)
	mux := http.NewServeMux()
	server := httptest.NewServer(mux)
	t.Cleanup(server.Close)
	manager := &gatewayManagedFixture{}
	app, err := host.Open(ctx, host.Options{DataDir: filepath.Join(t.TempDir(), "metadata"), PublicURL: server.URL + "/", Managed: manager})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = app.Close() })
	mux.Handle("/", app)
	scope := host.AgentScope{Principal: identity.User{ID: "im-test-owner"}, OwnerID: "im-test-owner", Binding: runner.Binding{RunnerID: "im-test-runner"}}
	enrollment, err := manager.access.IssueEnrollment(ctx, scope.Principal, scope.OwnerID, runner.Runner{ID: scope.Binding.RunnerID, Name: "IM test", Kind: "managed"}, "im-test-fabric")
	if err != nil {
		t.Fatal(err)
	}
	body, _ := json.Marshal(map[string]string{"token": enrollment.Token, "os": runtime.GOOS, "arch": runtime.GOARCH})
	request := httptest.NewRequest(http.MethodPost, server.URL+"/api/v1/enroll", strings.NewReader(string(body))).WithContext(ctx)
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("X-Dune-Request", "1")
	response := httptest.NewRecorder()
	app.ServeHTTP(response, request)
	var enrolled struct {
		Machine    struct{ ID string }
		Credential string
		Gateway    string
	}
	if response.Code != http.StatusOK || json.Unmarshal(response.Body.Bytes(), &enrolled) != nil || enrolled.Credential == "" {
		t.Fatalf("test Runner enrollment failed: status=%d", response.Code)
	}
	stateDir := filepath.Join(t.TempDir(), "fabricd")
	if err := os.Mkdir(stateDir, 0700); err != nil {
		t.Fatal(err)
	}
	profile := api.Profile{Version: 1, Kind: "agent", WorkingDirectory: t.TempDir(), Adapter: "acp", Start: api.Command{Argv: []string{os.Args[0]}}, Env: map[string]string{"DUNE_IM_TEST_ACP": "1"}}
	engine, err := fabricd.Open(ctx, stateDir)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(engine.Close)
	conn, err := ws.Dial(ctx, enrolled.Gateway, enrolled.Credential, nil)
	if err != nil {
		t.Fatal(err)
	}
	done := make(chan struct{})
	go func() {
		defer close(done)
		_ = engine.ServeConn(ctx, conn, enrolled.Machine.ID)
	}()
	t.Cleanup(func() {
		_ = conn.Close()
		select {
		case <-done:
		case <-time.After(5 * time.Second):
			t.Error("IM test connector did not stop")
		}
	})
	for {
		state, err := manager.access.State(ctx, scope.OwnerID, scope.Binding.RunnerID)
		if err != nil {
			t.Fatal(err)
		}
		if state.Online {
			scope.Binding = *state.Runner.Binding
			break
		}
		select {
		case <-ctx.Done():
			t.Fatal(ctx.Err())
		case <-time.After(10 * time.Millisecond):
		}
	}
	backend := duneagent.Backend{Executor: app.AgentExecutor(), Scopes: gatewayScopeFixture{scope: scope}, Profiles: gatewayScopeFixture{profile: profile}}
	return backend, channel.AgentTarget{OwnerID: scope.OwnerID, RunnerID: scope.Binding.RunnerID, FabricID: scope.Binding.FabricID, MachineID: scope.Binding.MachineID, RunnerBindingRevision: scope.Binding.Revision, ProfileID: "im-test-agent", ProfileRevision: 1, WorkingDirectory: t.TempDir()}
}

func TestDuneGatewaySessionIsolationAttachAndExplicitNew(t *testing.T) {
	backend, target := gatewayAgentFixture(t)
	ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
	defer cancel()
	var conversations []channel.ConversationSession
	var sessions []channel.AgentSession
	for _, sender := range []string{"alice", "bob"} {
		conversation := channel.ConversationSession{Key: channel.SessionKey{TenantID: "tenant-a", BindingID: "bot-a", ChatID: "direct", SubjectID: sender}, Target: target}
		session, err := backend.Start(ctx, conversation)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = backend.Stop(context.Background(), conversation, session) })
		conversations = append(conversations, conversation)
		sessions = append(sessions, session)
	}
	if sessions[0].Runtime.ID == sessions[1].Runtime.ID || sessions[0].ACPSessionID == sessions[1].ACPSessionID {
		t.Fatal("two conversations share an ACP process/session")
	}
	sequence := 0
	prompt := func(i int, want string) {
		sequence++
		t.Helper()
		var visible strings.Builder
		answer, err := backend.Prompt(ctx, conversations[i], sessions[i], channel.PromptRequest{SubmissionID: fmt.Sprintf("prompt-%d", sequence), Text: "hello"}, func(event channel.AgentEvent) error {
			if event.Kind == channel.AgentDelta {
				visible.WriteString(event.Text)
			}
			return nil
		})
		if err != nil || answer != want || visible.String() != want {
			t.Fatalf("ACP answer=%q deltas=%q want=%q err=%v", answer, visible.String(), want, err)
		}
	}
	prompt(0, "turn 1: hello")
	prompt(1, "turn 1: hello")
	attached, err := backend.Attach(ctx, conversations[0], sessions[0])
	if err != nil || attached != sessions[0] {
		t.Fatalf("attach replaced the live session: %+v %v", attached, err)
	}
	prompt(0, "turn 2: hello")
	if err := backend.Stop(ctx, conversations[0], sessions[0]); err != nil {
		t.Fatal(err)
	}
	if _, err := backend.Attach(ctx, conversations[0], sessions[0]); err == nil {
		t.Fatal("attachment silently replaced the stopped Runtime")
	}
	// A new persisted revision is a separate explicit caller intent.
	conversations[0].Revision++
	created, err := backend.Start(ctx, conversations[0])
	if err != nil || created.Runtime.ID == sessions[0].Runtime.ID {
		t.Fatal("explicit new did not create a fresh Runtime", created, err)
	}
	sessions[0] = created
	t.Cleanup(func() { _ = backend.Stop(context.Background(), conversations[0], created) })
	prompt(0, "turn 1: hello")
	prompt(1, "turn 2: hello")
}
