package host

import (
	"context"
	"encoding/json"
	"errors"
	"maps"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/aiomni/dune/internal/metadata"
	"github.com/aiomni/dune/internal/wire"
	"github.com/aiomni/dune/pkg/agents"
	"github.com/aiomni/dune/pkg/api"
	"github.com/aiomni/dune/pkg/client"
	"github.com/aiomni/dune/pkg/runner"
)

func directoryACP(t *testing.T, f executorFixture) (agents.LaunchResult, *client.Client) {
	return directoryACPWithEnvironment(t, f, nil)
}

func directoryACPWithEnvironment(t *testing.T, f executorFixture, environment map[string]string) (agents.LaunchResult, *client.Client) {
	profile := directoryACPProfile(t, f)
	maps.Copy(profile.Env, environment)
	return startDirectoryACP(t, f, agents.StartRequest{Binding: f.binding, Custom: &profile})
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
	connection, closeConnection, err := (&runnerExecutor{app: f.app}).connect(t.Context(), f.principal, f.owner, f.binding, "acp.action")
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

func directoryAction(t *testing.T, connection *client.Client, runtime api.Runtime, action api.ACPAction) {
	t.Helper()
	op, err := connection.ACPSubmit(t.Context(), runtime, action)
	if err != nil {
		t.Fatal(err)
	}
	op, err = connection.WaitAgentOperation(t.Context(), runtime, api.AgentOperationWait{Ref: op.Ref, TimeoutMS: 3000})
	if err != nil || op.State != "completed" {
		t.Fatal("fixture lifecycle did not finish", op, err)
	}
}

func TestAgentDirectoryCapturesNativeSessionsThroughGateway(t *testing.T) {
	f := openExecutorFixture(t)
	launched, connection := directoryACP(t, f)
	directory := f.app.AgentDirectory()
	page, err := directory.List(t.Context(), f.agentScope(), runner.Query{})
	if err != nil || len(page.Items) != 1 || len(page.Runners) != 1 || !page.Runners[0].Ready || page.Items[0].Session.Status != "pending_capture" {
		t.Fatal("initial discovery", page, err)
	}
	initial := page.Items[0]
	directoryAction(t, connection, *launched.Runtime, api.ACPAction{Action: "new"})
	created, err := directory.Get(t.Context(), f.agentScope(), initial.Ref)
	if err != nil || created.Session == nil || created.Session.ID != launched.Session.ID || created.Session.Status != "available" || created.Ref == initial.Ref {
		t.Fatal("confirmed native session not captured", created, err)
	}
	directoryAction(t, connection, *launched.Runtime, api.ACPAction{Action: "load", SessionID: "other-native", Cwd: "/other-session"})
	if _, err := directory.Get(t.Context(), f.agentScope(), created.Ref); err == nil {
		t.Fatal("an old Agent reference followed a different native conversation")
	} else {
		var failure *api.Error
		if !errors.As(err, &failure) || failure.Code != "STALE_SESSION" {
			t.Fatal(err)
		}
	}
	page, err = directory.List(t.Context(), f.agentScope(), runner.Query{})
	if err != nil || len(page.Items) != 1 || page.Items[0].Session == nil || page.Items[0].Session.ID == created.Session.ID || page.Items[0].Session.Native.ID != "other-native" {
		t.Fatal("switched session was not indexed", page, err)
	}
	old, err := f.app.store.AgentSession(t.Context(), f.owner, created.Session.ID)
	if err != nil || old.Selected || old.Native.ID != "fake-acp-session" {
		t.Fatal("old native recovery lost", err)
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
	if err := connection.Stop(t.Context(), *launched.Runtime); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(3 * time.Second)
	for {
		stopped, err := directory.Get(t.Context(), f.agentScope(), page.Items[0].Ref)
		if err != nil || stopped.Session == nil {
			t.Fatal("stopped Runtime lost recovery discovery", err)
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
	if _, err := f.app.AgentLauncher().Start(t.Context(), f.agentScope(), agents.StartRequest{Binding: f.binding, Custom: &profile}); err != nil {
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
	if err != nil || len(last.Items) != 0 || len(last.Runners) != 1 || last.Runners[0].Online || last.Runners[0].Ready {
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
	if err != nil || len(partial.Items) != 1 || len(partial.Issues) != 1 || partial.Issues[0].RunnerID != logical.ID || partial.Issues[0].Code != "OFFLINE" || partial.Runners[1].Ready {
		t.Fatal("one offline route hid the healthy Runner", partial, err)
	}
}
