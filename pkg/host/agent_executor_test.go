package host

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/aiomni/dune/internal/authorization"
	"github.com/aiomni/dune/pkg/access"
	"github.com/aiomni/dune/pkg/api"
)

func TestAgentExecutorUsesAuthorizedRunnerScope(t *testing.T) {
	f := openExecutorFixture(t)
	scope := AgentScope{Principal: f.principal, OwnerID: f.owner, RunnerID: f.binding.RunnerID}
	connection, err := f.app.AgentExecutor().Open(context.Background(), scope)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := connection.AgentConfig(context.Background(), "missing-agent"); err == nil {
		t.Fatal("missing Runner-local AgentConfig was reported present")
	}
	if _, err := connection.Start(context.Background(), api.Profile{Version: 1, Kind: "agent", Adapter: "pty"}); err == nil {
		t.Fatal("non-managed ACP Agent started through IM executor")
	}
	if err := connection.Action(context.Background(), api.Runtime{}, AgentAction{Action: "permission"}); err == nil {
		t.Fatal("IM executor allowed a permission action")
	}
	if err := connection.Close(); err != nil {
		t.Fatal(err)
	}
	scope.Principal.ID = "another-person"
	if _, err := f.app.AgentExecutor().Open(context.Background(), scope); !errors.Is(err, access.ErrDenied) {
		t.Fatalf("unauthorized principal opened Agent connection: %v", err)
	}
	scope = AgentScope{Principal: f.principal, OwnerID: "another-owner", RunnerID: f.binding.RunnerID}
	if _, err := f.app.AgentExecutor().Open(context.Background(), scope); !errors.Is(err, authorization.ErrNotFound) {
		t.Fatalf("wrong Tenant opened Agent connection: %v", err)
	}
	scope = AgentScope{Principal: f.principal, OwnerID: f.owner, RunnerID: "another-runner"}
	if _, err := f.app.AgentExecutor().Open(context.Background(), scope); !errors.Is(err, authorization.ErrNotFound) {
		t.Fatalf("unknown Runner opened Agent connection: %v", err)
	}
}

func TestFakeACPChild(t *testing.T) {
	if os.Getenv("DUNE_HOST_FAKE_ACP_CHILD") != "1" {
		return
	}
	scanner := bufio.NewScanner(os.Stdin)
	scanner.Buffer(make([]byte, 4096), 1<<20)
	for scanner.Scan() {
		var request struct {
			ID     json.RawMessage `json:"id"`
			Method string          `json:"method"`
		}
		if json.Unmarshal(scanner.Bytes(), &request) != nil || len(request.ID) == 0 {
			continue
		}
		switch request.Method {
		case "initialize":
			fmt.Fprintf(os.Stdout, `{"jsonrpc":"2.0","id":%s,"result":{"protocolVersion":1,"agentCapabilities":{"loadSession":true}}}`+"\n", request.ID)
		case "session/new":
			fmt.Fprintf(os.Stdout, `{"jsonrpc":"2.0","id":%s,"result":{"sessionId":"fake-acp-session"}}`+"\n", request.ID)
		case "session/load":
			fmt.Fprintf(os.Stdout, `{"jsonrpc":"2.0","id":%s,"result":{}}`+"\n", request.ID)
		case "session/prompt":
			fmt.Fprintln(os.Stdout, `{"jsonrpc":"2.0","method":"session/update","params":{"sessionId":"fake-acp-session","update":{"sessionUpdate":"agent_message_chunk","content":{"type":"text","text":"fake answer"}}}}`)
			fmt.Fprintf(os.Stdout, `{"jsonrpc":"2.0","id":%s,"result":{"stopReason":"end_turn"}}`+"\n", request.ID)
		}
	}
}

func TestAgentExecutorRunsManagedACPThroughGateway(t *testing.T) {
	f := openExecutorFixture(t)
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	connection, err := f.app.AgentExecutor().Open(ctx, AgentScope{Principal: f.principal, OwnerID: f.owner, RunnerID: f.binding.RunnerID})
	if err != nil {
		t.Fatal(err)
	}
	defer connection.Close()
	config := api.AgentConfig{Name: "Fake ACP", Command: os.Args[0], Args: []string{"-test.run=^TestFakeACPChild$"}, Adapter: "acp", Env: map[string]string{"DUNE_HOST_FAKE_ACP_CHILD": "1"}}
	var saved api.AgentConfig
	if err := connection.(*agentConnection).sdk.Call(ctx, "agent.config", api.AgentConfigRequest{Action: "save", Config: &config}, &saved); err != nil {
		t.Fatal(err)
	}
	loaded, err := connection.AgentConfig(ctx, saved.ID)
	if err != nil || loaded.ID != saved.ID {
		t.Fatalf("AgentConfig not resolved on selected Runner: %+v %v", loaded, err)
	}
	profile := loaded.Profile(f.workspace)
	profile.ManagedACP = true
	runtime, err := connection.Start(ctx, profile)
	if err != nil {
		t.Fatal(err)
	}
	defer connection.Stop(context.Background(), runtime)
	waitState := func(selected api.Runtime, ready func(AgentState) bool) AgentState {
		t.Helper()
		for {
			state, err := connection.State(ctx, selected)
			if err != nil {
				t.Fatal(err)
			}
			if state.Error != "" {
				t.Fatal(state.Error)
			}
			if ready(state) {
				return state
			}
			select {
			case <-ctx.Done():
				t.Fatal(ctx.Err())
			case <-time.After(20 * time.Millisecond):
			}
		}
	}
	waitState(runtime, func(state AgentState) bool { return state.Ready && state.Busy == "" })
	if err := connection.Action(ctx, runtime, AgentAction{Action: "new", Cwd: f.workspace}); err != nil {
		t.Fatal(err)
	}
	waitState(runtime, func(state AgentState) bool { return state.SessionID == "fake-acp-session" && state.Busy == "" })
	subscription, err := connection.Observe(ctx, runtime)
	if err != nil {
		t.Fatal(err)
	}
	defer subscription.Close()
	if err := connection.Action(ctx, runtime, AgentAction{Action: "prompt", Text: "hello"}); err != nil {
		t.Fatal(err)
	}
	var answer, stopReason string
	for stopReason == "" {
		message, err := subscription.Recv()
		if err != nil {
			t.Fatal(err)
		}
		if message.Kind == "acp_update" && strings.Contains(string(message.Payload), "agent_message_chunk") {
			answer = string(message.Payload)
		}
		if message.Kind == "acp_state" {
			var state AgentState
			if err := json.Unmarshal(message.Payload, &state); err != nil {
				t.Fatal(err)
			}
			stopReason = state.StopReason
		}
	}
	if !strings.Contains(answer, "fake answer") || stopReason != "end_turn" {
		t.Fatalf("managed ACP output incomplete: answer=%s stopReason=%s", answer, stopReason)
	}
	if err := connection.Stop(ctx, runtime); err != nil {
		t.Fatal(err)
	}
	replacement, err := connection.Start(ctx, profile)
	if err != nil {
		t.Fatal(err)
	}
	defer connection.Stop(context.Background(), replacement)
	replacementState := waitState(replacement, func(state AgentState) bool { return state.Ready && state.Busy == "" })
	if !replacementState.CanLoad {
		t.Fatal("fake ACP load capability was not published")
	}
	if err := connection.Action(ctx, replacement, AgentAction{Action: "load", Cwd: f.workspace, SessionID: "fake-acp-session"}); err != nil {
		t.Fatal(err)
	}
	waitState(replacement, func(state AgentState) bool {
		return state.Ready && state.Busy == "" && state.SessionID == "fake-acp-session"
	})
}
