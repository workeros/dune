package host

import (
	"context"
	"errors"
	"net"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/aiomni/dune/internal/authorization"
	localidentity "github.com/aiomni/dune/internal/identity"
	"github.com/aiomni/dune/internal/tmux"
	"github.com/aiomni/dune/pkg/access"
	"github.com/aiomni/dune/pkg/api"
	"github.com/aiomni/dune/pkg/fabricd"
	"github.com/aiomni/dune/pkg/identity"
	"github.com/aiomni/dune/pkg/runner"
)

func TestMain(m *testing.M) {
	if code, handled := fabricd.RunHelper(os.Args[1:]); handled {
		os.Exit(code)
	}
	if os.Getenv("DUNE_HOST_FAKE_NATIVE") == "1" {
		os.Exit(runHostNativeFixture())
	}
	os.Exit(m.Run())
}

type executorFixture struct {
	app       *App
	executor  RunnerExecutor
	principal identity.User
	owner     string
	binding   runner.Binding
	workspace string
}

func openExecutorFixture(t *testing.T) executorFixture {
	t.Helper()
	return openExecutorFixtureFor(t, 15*time.Second)
}

func openExecutorFixtureFor(t *testing.T, duration time.Duration) executorFixture {
	t.Helper()
	if os.Getenv("DUNE_TMUX") == "" {
		binary, _ := filepath.Abs("../../bin/tmux")
		t.Setenv("DUNE_TMUX", binary)
	}
	ctx, cancel := context.WithTimeout(context.Background(), duration)
	var app *App
	app, err := Open(ctx, Options{DataDir: filepath.Join(t.TempDir(), "metadata"), PublicURL: "http://dune.example.test/"})
	if err != nil {
		cancel()
		t.Fatal(err)
	}
	principal := identity.User{ID: "background-creator"}
	if err := app.store.RegisterAccount(ctx, localidentity.Account{User: principal, Salt: "unused", PasswordHash: "unused"}, "unused", time.Now().Add(time.Hour).Unix()); err != nil {
		cancel()
		app.Close()
		t.Fatal(err)
	}
	owner := principal.ID
	logical := runner.Runner{ID: "background-runner", Name: "Background Runner", Kind: "managed"}
	token, _, err := app.store.IssueManagedEnrollment(ctx, principal, owner, logical, "test-fabric")
	if err != nil {
		cancel()
		app.Close()
		t.Fatal(err)
	}
	machine, credential, err := app.store.Enroll(ctx, token, "linux", "amd64")
	if err != nil {
		cancel()
		app.Close()
		t.Fatal(err)
	}
	binding := runner.Binding{RunnerID: logical.ID, FabricID: "test-fabric", MachineID: machine.ID, Revision: 1}
	engineDir := filepath.Join(t.TempDir(), "fabricd")
	engine, err := fabricd.Open(ctx, engineDir)
	if err != nil {
		cancel()
		app.Close()
		t.Fatal(err)
	}
	grant, handler, err := app.authorizer.Authorize(credential)
	if err != nil {
		cancel()
		engine.Close()
		app.Close()
		t.Fatal(err)
	}
	left, right := net.Pipe()
	var wg sync.WaitGroup
	wg.Go(func() { _ = app.core.ServeConn(ctx, left, grant, handler) })
	wg.Go(func() { _ = engine.ServeConn(ctx, right, machine.ID) })
	for !app.core.Online(machine.ID) {
		select {
		case <-ctx.Done():
			t.Fatal("fabricd did not connect")
		case <-time.After(time.Millisecond):
		}
	}
	t.Cleanup(func() {
		cancel()
		app.Close()
		engine.Close()
		wg.Wait()
		if manager, err := tmux.Open(engineDir); err == nil {
			_ = manager.Close()
		}
	})
	return executorFixture{app: app, executor: app.RunnerExecutor(), principal: principal, owner: owner, binding: binding, workspace: t.TempDir()}
}

func (f executorFixture) execution(id string, profile api.Profile) RunnerExecutionRequest {
	return RunnerExecutionRequest{Principal: f.principal, OwnerID: f.owner, Binding: f.binding, ExecutionID: id, Profile: profile}
}

func (f executorFixture) status(id string) RunnerStatusRequest {
	return RunnerStatusRequest{Principal: f.principal, OwnerID: f.owner, Binding: f.binding, ExecutionID: id}
}

func TestRunnerExecutorPreparesThroughGatewayWithoutBrowserSession(t *testing.T) {
	f := openExecutorFixture(t)
	profile := api.Profile{Version: 1, Kind: "environment", WorkingDirectory: f.workspace}
	profile.Setup.Steps = []api.Command{{Argv: []string{"/bin/sh", "-c", "printf prepared > environment-ready; printf progress-output"}}}
	var progress []api.ProfileProgress
	result, err := f.executor.Prepare(context.Background(), f.execution("host-background-attempt", profile), func(event api.ProfileProgress) {
		progress = append(progress, event)
	})
	if err != nil {
		t.Fatal(err)
	}
	if result.Stage != "succeeded" || result.StepsCompleted != 1 || len(progress) != 4 || progress[0].Stage != "accepted" || progress[2].StepResult == nil || progress[2].StepResult.Stdout != "progress-output" {
		t.Fatalf("unexpected result/progress: %+v %+v", result, progress)
	}
	if content, err := os.ReadFile(filepath.Join(f.workspace, "environment-ready")); err != nil || string(content) != "prepared" {
		t.Fatal("environment Profile did not execute", string(content), err)
	}
	status, err := f.executor.Status(context.Background(), f.status("host-background-attempt"))
	if err != nil || status.State != "succeeded" || status.Result == nil {
		t.Fatalf("unexpected execution status: %+v %v", status, err)
	}

	internal := f.executor.(*runnerExecutor)
	sdk, closeClient, err := internal.connect(context.Background(), f.principal, f.owner, f.binding, "runtime.list")
	if err != nil {
		t.Fatal(err)
	}
	runtimes, err := sdk.List(context.Background())
	closeClient()
	if err != nil || len(runtimes) != 0 {
		t.Fatal("environment preparation created an Agent Runtime", runtimes, err)
	}
}

func TestRunnerExecutorRejectsUnauthorizedAndChangedBindings(t *testing.T) {
	f := openExecutorFixture(t)
	profile := api.Profile{Version: 1, Kind: "environment", WorkingDirectory: f.workspace}
	invalidMarker := filepath.Join(f.workspace, "invalid-profile-ran")
	invalid := profile
	invalid.WorkingDirectory = "relative"
	invalid.Setup.Steps = []api.Command{{Argv: []string{"/usr/bin/touch", invalidMarker}}}
	var invalidProgress []api.ProfileProgress
	if _, err := f.executor.Prepare(context.Background(), f.execution("invalid-profile-attempt", invalid), func(event api.ProfileProgress) {
		invalidProgress = append(invalidProgress, event)
	}); err == nil {
		t.Fatal("invalid Profile accepted")
	} else {
		var protocol *api.Error
		if !errors.As(err, &protocol) || protocol.Code != "INVALID_ARGUMENT" || len(invalidProgress) != 1 || invalidProgress[0].Stage != "failed" || invalidProgress[0].Failure == nil || invalidProgress[0].Failure.Step != -1 {
			t.Fatal("invalid Profile lost stable failure", err, invalidProgress)
		}
	}
	if _, err := os.Stat(invalidMarker); !os.IsNotExist(err) {
		t.Fatal("invalid Profile caused a side effect", err)
	}
	request := f.execution("unauthorized-attempt", profile)
	request.Principal.ID = "another-principal"
	if _, err := f.executor.Prepare(context.Background(), request, nil); !errors.Is(err, access.ErrDenied) {
		t.Fatal("unauthorized principal was not denied", err)
	}
	request = f.execution("wrong-owner-attempt", profile)
	request.OwnerID = "another-owner"
	if _, err := f.executor.Prepare(context.Background(), request, nil); !errors.Is(err, authorization.ErrNotFound) {
		t.Fatal("wrong owner was not denied", err)
	}
	stale := f.status("stale-binding-attempt")
	stale.Binding.Revision++
	if _, err := f.executor.Status(context.Background(), stale); !errors.Is(err, runner.ErrBindingChanged) {
		t.Fatal("stale binding was not rejected", err)
	}
	if err := (managedRunnerAccess{store: f.app.store, core: f.app.core}).SetSuspended(context.Background(), f.owner, f.binding.RunnerID, true); err != nil {
		t.Fatal(err)
	}
	if _, err := f.executor.Prepare(context.Background(), f.execution("suspended-attempt", profile), nil); !errors.Is(err, authorization.ErrNotFound) {
		t.Fatal("suspended Runner was not rejected", err)
	}
}

func TestRunnerExecutorParticipatesInAppShutdown(t *testing.T) {
	f := openExecutorFixture(t)
	profile := api.Profile{Version: 1, Kind: "environment", WorkingDirectory: f.workspace}
	profile.Setup.Steps = []api.Command{{Argv: []string{"/bin/sh", "-c", "sleep 0.2; touch drained-prepare"}}}
	started := make(chan struct{})
	result := make(chan error, 1)
	go func() {
		_, err := f.executor.Prepare(context.Background(), f.execution("drained-attempt", profile), func(event api.ProfileProgress) {
			if event.Stage == "setup_running" {
				select {
				case <-started:
				default:
					close(started)
				}
			}
		})
		result <- err
	}()
	select {
	case <-started:
	case <-time.After(2 * time.Second):
		t.Fatal("background execution did not start")
	}
	shutdownCtx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	if err := f.app.Shutdown(shutdownCtx); err != nil {
		t.Fatal(err)
	}
	if err := <-result; err != nil {
		t.Fatal("accepted execution was interrupted during drain", err)
	}
	if _, err := os.Stat(filepath.Join(f.workspace, "drained-prepare")); err != nil {
		t.Fatal("drain returned before Profile completed", err)
	}
	if _, err := f.executor.Status(context.Background(), f.status("drained-attempt")); err == nil {
		t.Fatal("closed App admitted another executor call")
	}
}
