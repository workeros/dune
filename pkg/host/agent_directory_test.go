package host

import (
	"context"
	"encoding/json"
	"errors"
	"maps"
	"net"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/aiomni/dune/internal/metadata"
	"github.com/aiomni/dune/internal/wire"
	"github.com/aiomni/dune/pkg/agents"
	"github.com/aiomni/dune/pkg/api"
	"github.com/aiomni/dune/pkg/client"
	"github.com/aiomni/dune/pkg/runner"
	pb "github.com/aiomni/dune/proto/dune/dtp/v1"
	"github.com/hashicorp/yamux"
)

func directoryACP(t *testing.T, f executorFixture) (agents.LaunchResult, *client.Client) {
	return directoryACPWithEnvironment(t, f, nil)
}

func directoryACPWithEnvironment(t *testing.T, f executorFixture, environment map[string]string) (agents.LaunchResult, *client.Client) {
	profile := directoryACPProfile(t, f)
	maps.Copy(profile.Env, environment)
	return startDirectoryACP(t, f, agents.StartRequest{SubmissionID: wire.ID(), Binding: f.binding, Custom: &profile})
}

func directoryACPProfile(t *testing.T, f executorFixture) api.Profile {
	t.Helper()
	// This local transport fixture accepts --acp, without invoking an AI model.
	script := filepath.Join(f.workspace, "fixture-agent")
	executable := "'" + strings.ReplaceAll(os.Args[0], "'", "'\\''") + "'"
	if err := os.WriteFile(script, []byte("#!/bin/sh\nexec "+executable+" -test.run=^TestFakeACPChild$\n"), 0700); err != nil {
		t.Fatal(err)
	}
	return api.Profile{Version: 1, Kind: "agent", Adapter: "acp", WorkingDirectory: f.workspace,
		Start: api.Command{Argv: []string{script, "--acp"}}, Env: map[string]string{"DUNE_HOST_FAKE_ACP_CHILD": "1", "TEST_LAUNCH_SECRET": "not-for-discovery"}}
}

func startDirectoryACP(t *testing.T, f executorFixture, request agents.StartRequest) (agents.LaunchResult, *client.Client) {
	t.Helper()
	result, err := f.app.AgentLauncher().Start(t.Context(), f.agentScope(), request)
	if err != nil {
		t.Fatal(err)
	}
	connection, closeConnection, err := (&runnerExecutor{app: f.app}).connect(t.Context(), f.principal, f.owner, f.binding, "submission.acp")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(closeConnection)
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	for {
		var state AgentState
		if err := connection.CallID(ctx, "acp.state", wire.ID(), struct{}{}, &state, result.Runtime); err != nil {
			t.Fatal(err)
		}
		if state.Ready {
			break
		}
		select {
		case <-ctx.Done():
			t.Fatal("ACP fixture did not become ready")
		case <-time.After(5 * time.Millisecond):
		}
	}
	return result, connection
}

func directoryAction(t *testing.T, connection *client.Client, runtime api.Runtime, key api.SubmissionKey, action api.ACPAction) {
	t.Helper()
	key.SubmissionID = wire.ID()
	key.Target.RuntimeID, key.Target.RuntimeIncarnation, key.Target.RuntimeGeneration = runtime.ID, runtime.Incarnation, runtime.Generation
	op, err := connection.ACPSubmit(t.Context(), key, action)
	if err != nil {
		t.Fatal(err)
	}
	op, err = connection.WaitAgentOperation(t.Context(), runtime, api.AgentOperationWait{Ref: op.Ref, TimeoutMS: 3000})
	if err != nil || op.State != "completed" {
		t.Fatal("fixture lifecycle did not finish", op, err)
	}
}

func TestAgentDirectoryReadsCurrentNativeSessionsThroughGateway(t *testing.T) {
	f := openExecutorFixture(t)
	launched, connection := directoryACP(t, f)
	directory := f.app.AgentDirectory()
	page, err := directory.List(t.Context(), f.agentScope(), runner.Query{})
	if err != nil || len(page.Items) != 1 || len(page.Runners) != 1 || !page.Runners[0].Ready || page.Items[0].Runtime.NativeSession == nil {
		t.Fatal("initial discovery", page, err)
	}
	initial := page.Items[0]
	directoryAction(t, connection, *launched.Runtime, launched.SubmissionKey, api.ACPAction{Action: "new"})
	created, err := directory.Get(t.Context(), f.agentScope(), initial.Ref)
	if err != nil || created.Runtime.NativeSession == nil || created.Runtime.NativeSession.ID != launched.Runtime.NativeSession.ID || created.Ref != initial.Ref {
		t.Fatal("confirmed native session not captured", created, err)
	}
	directoryAction(t, connection, *launched.Runtime, launched.SubmissionKey, api.ACPAction{Action: "load", SessionID: "other-native", Cwd: "/other-session"})
	if _, err := directory.Get(t.Context(), f.agentScope(), created.Ref); err == nil {
		t.Fatal("an old Agent reference followed a different native conversation")
	} else {
		var failure *api.Error
		if !errors.As(err, &failure) || failure.Code != "STALE_SESSION" {
			t.Fatal(err)
		}
	}
	page, err = directory.List(t.Context(), f.agentScope(), runner.Query{})
	if err != nil || len(page.Items) != 1 || page.Items[0].Runtime.NativeSession == nil || page.Items[0].Runtime.NativeSession.ID != "other-native" {
		t.Fatal("switched session was not indexed", page, err)
	}
	encoded, _ := json.Marshal(page)
	if strings.Contains(string(encoded), "TEST_LAUNCH_SECRET") || strings.Contains(string(encoded), "not-for-discovery") || strings.Contains(string(encoded), "-test.run") {
		t.Fatal("discovery exposed private launch configuration")
	}
	wrong := f.agentScope()
	wrong.OwnerID = "another-tenant"
	if _, err := directory.Get(t.Context(), wrong, page.Items[0].Ref); !errors.Is(err, metadata.ErrNotFound) {
		t.Fatal("cross-Tenant reference accepted", err)
	}
	if _, err := directory.List(t.Context(), wrong, runner.Query{}); err == nil {
		t.Fatal("cross-Tenant discovery accepted")
	}
	if err := testStopSDK(connection, t.Context(), *launched.Runtime, launched.SubmissionKey); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(3 * time.Second)
	for {
		stopped, err := directory.Get(t.Context(), f.agentScope(), page.Items[0].Ref)
		if err != nil {
			t.Fatal("stopped Runtime lost exit status", err)
		}
		if stopped.Runtime.State == "exited" {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("Agent did not exit after stop")
		}
		time.Sleep(5 * time.Millisecond)
	}
}

func TestAgentDirectoryKeepsHealthyRunnersWhenAnotherRunnerIsOffline(t *testing.T) {
	f := openExecutorFixture(t)
	profile := launchShell(f.workspace, "true")
	if _, err := f.app.AgentLauncher().Start(t.Context(), f.agentScope(), agents.StartRequest{SubmissionID: wire.ID(), Binding: f.binding, Custom: &profile}); err != nil {
		t.Fatal(err)
	}
	logical := runner.Runner{ID: "zz-offline", Name: "Offline", Kind: "managed"}
	token, _, err := f.app.store.IssueManagedEnrollment(t.Context(), f.principal, f.owner, logical, "test-fabric")
	if err != nil {
		t.Fatal(err)
	}
	machine, _, err := f.app.store.Enroll(t.Context(), token, "linux", "amd64")
	if err != nil {
		t.Fatal(err)
	}
	page, err := f.app.AgentDirectory().List(t.Context(), f.agentScope(), runner.Query{Limit: 1})
	if err != nil || len(page.Items) != 1 || page.NextCursor == "" {
		t.Fatal("first directory page", page, err)
	}
	last, err := f.app.AgentDirectory().List(t.Context(), f.agentScope(), runner.Query{Limit: 1, Cursor: page.NextCursor})
	if err != nil || last.Complete || len(last.Items) != 0 || len(last.Runners) != 1 || last.Runners[0].Online || last.Runners[0].Ready {
		t.Fatal("offline directory page", last, err)
	}
	service := f.app.agentService()
	service.Online = func(context.Context, []string) (map[string]bool, error) {
		return map[string]bool{f.binding.MachineID: true, machine.ID: true}, nil
	}
	dial := service.Dial
	service.Dial = func(ctx context.Context, scope agents.Scope, binding runner.Binding, operation string) (*client.Client, func(), error) {
		if binding.MachineID == machine.ID {
			return nil, nil, &api.Error{Code: "OFFLINE", Detail: "stale route"}
		}
		return dial(ctx, scope, binding, operation)
	}
	partial, err := service.List(t.Context(), f.agentScope(), runner.Query{})
	if err != nil || partial.Complete || len(partial.Items) != 1 || len(partial.Issues) != 1 || partial.Issues[0].RunnerID != logical.ID || partial.Issues[0].Code != "OFFLINE" || partial.Runners[1].Ready {
		t.Fatal("one offline route hid the healthy Runner", partial, err)
	}
}

func TestAgentDirectoryPreservesPartialRuntimeDiscoveryFromOneRunner(t *testing.T) {
	f := openExecutorFixture(t)
	healthy := api.Runtime{ID: wire.ID(), Incarnation: wire.ID(), Generation: 1, Adapter: "acp", State: "running"}
	title, conversation := "batch title", "conversation"
	healthy.SessionMetadata = &api.SessionMetadata{Revision: 1 << 53, ConversationID: &conversation, Title: &title}
	sibling := healthy
	sibling.ID = wire.ID()
	broken := api.Runtime{ID: wire.ID(), Incarnation: wire.ID(), Generation: 1, Adapter: "acp", Availability: "unavailable"}
	service := f.app.agentService()
	var connections, calls atomic.Int32
	service.Dial = func(ctx context.Context, _ agents.Scope, binding runner.Binding, operation string) (*client.Client, func(), error) {
		connections.Add(1)
		if binding != f.binding || operation != "runtime.list" {
			t.Error("discovery changed target or performed a control", binding, operation)
		}
		local, remote := net.Pipe()
		server, err := yamux.Server(remote, wire.Config())
		if err != nil {
			return nil, nil, err
		}
		go func() {
			control, err := server.AcceptStream()
			if err != nil {
				return
			}
			if _, err = wire.Read(control); err != nil {
				return
			}
			if err = wire.Write(control, &pb.Message{Kind: "welcome", Payload: api.Payload(api.Binding{Target: binding.MachineID, Incarnation: "fixture", Generation: 1, Capabilities: []string{"runtime.list"}})}); err != nil {
				return
			}
			stream, err := server.AcceptStream()
			if err != nil {
				return
			}
			defer stream.Close()
			request, err := wire.Read(stream)
			if err != nil {
				return
			}
			if request.Operation != "runtime.list" {
				t.Error("unexpected RPC", request.Operation)
				return
			}
			calls.Add(1)
			if err := wire.Write(stream, &pb.Message{Kind: "accepted"}); err != nil {
				return
			}
			_ = wire.Write(stream, &pb.Message{Kind: "result", Payload: api.Payload(api.RuntimeList{Items: []api.Runtime{healthy, sibling}, Complete: false, Issues: []api.RuntimeDiscoveryIssue{{Runtime: &broken, Code: "REGISTRATION_INVALID"}}})})
		}()
		connection, err := client.Connect(ctx, local, binding.MachineID)
		if err != nil {
			server.Close()
			return nil, nil, err
		}
		return connection, func() { connection.Close(); server.Close() }, nil
	}
	page, err := service.List(t.Context(), f.agentScope(), runner.Query{})
	if err != nil || page.Complete || len(page.Items) != 2 || page.Items[0].Runtime.ID != healthy.ID || len(page.Runners) != 1 || !page.Runners[0].Ready || len(page.Issues) != 1 || page.Issues[0].Runtime == nil || page.Issues[0].Runtime.ID != broken.ID || page.Issues[0].Code != "REGISTRATION_INVALID" {
		t.Fatal("partial discovery hid healthy sibling or lost original issue", page, err)
	}
	for _, item := range page.Items {
		if string(api.Payload(item.Runtime.SessionMetadata)) != string(api.Payload(healthy.SessionMetadata)) {
			t.Fatal("batch discovery omitted metadata", item)
		}
	}
	if connections.Load() != 1 || calls.Load() != 1 {
		t.Fatal("batch discovery added per-Runtime reads", connections.Load(), calls.Load())
	}
}

func testStopSDK(connection *client.Client, ctx context.Context, runtime api.Runtime, key api.SubmissionKey) error {
	key.SubmissionID = "test-stop"
	key.Target.RuntimeID, key.Target.RuntimeIncarnation, key.Target.RuntimeGeneration = runtime.ID, runtime.Incarnation, runtime.Generation
	_, err := connection.Stop(ctx, key)
	return err
}
