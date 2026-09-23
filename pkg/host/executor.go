package host

import (
	"context"
	"net"

	"github.com/aiomni/dune/pkg/api"
	"github.com/aiomni/dune/pkg/client"
	"github.com/aiomni/dune/pkg/gateway"
	"github.com/aiomni/dune/pkg/identity"
	"github.com/aiomni/dune/pkg/runner"
)

// RunnerExecutionRequest fixes the trusted principal, owner and concrete
// Runner binding for one Environment Profile attempt. ExecutionID is carried
// unchanged as the protocol request ID and must be stable across status checks.
type RunnerExecutionRequest struct {
	Principal   identity.User
	OwnerID     string
	Binding     runner.Binding
	ExecutionID string
	Profile     api.Profile
}

// RunnerStatusRequest selects one prior attempt in the same authorized Runner
// scope. Status never starts or retries execution.
type RunnerStatusRequest struct {
	Principal   identity.User
	OwnerID     string
	Binding     runner.Binding
	ExecutionID string
}

// RunnerExecutor is the trusted in-process Dune execution surface for a host's
// background work. Progress callbacks run synchronously and must return promptly.
type RunnerExecutor interface {
	Prepare(context.Context, RunnerExecutionRequest, func(api.ProfileProgress)) (api.ProfileResult, error)
	Status(context.Context, RunnerStatusRequest) (api.ProfileStatus, error)
}

type runnerExecutor struct{ app *App }

// RunnerExecutor returns the background execution capability owned by this App.
// Calls participate in App draining and are cancelled when the App closes.
func (a *App) RunnerExecutor() RunnerExecutor { return &runnerExecutor{app: a} }

func (e *runnerExecutor) Prepare(ctx context.Context, request RunnerExecutionRequest, onProgress func(api.ProfileProgress)) (api.ProfileResult, error) {
	var result api.ProfileResult
	if err := api.ValidateExecutionID(request.ExecutionID); err != nil {
		return result, &api.Error{Code: "INVALID_ARGUMENT", Detail: err.Error()}
	}
	if request.Profile.Kind != "environment" {
		err := &api.Error{Code: "INVALID_ARGUMENT", Detail: "RunnerExecutor.Prepare requires kind: environment"}
		emitHostProfileFailure(request.ExecutionID, err, onProgress)
		return result, err
	}
	if err := request.Profile.Validate(); err != nil {
		invalid := &api.Error{Code: "INVALID_ARGUMENT", Detail: err.Error()}
		emitHostProfileFailure(request.ExecutionID, invalid, onProgress)
		return result, invalid
	}
	ctx, finish, err := e.app.adminContext(ctx)
	if err != nil {
		return result, err
	}
	defer finish()
	sdk, closeClient, err := e.connect(ctx, request.Principal, request.OwnerID, request.Binding, "profile.prepare")
	if err != nil {
		return result, err
	}
	defer closeClient()
	return sdk.PrepareID(ctx, request.ExecutionID, request.Profile, onProgress)
}

func emitHostProfileFailure(executionID string, err *api.Error, onProgress func(api.ProfileProgress)) {
	if onProgress == nil {
		return
	}
	failure := &api.ProfileFailure{Code: err.Code, Detail: err.Detail, Step: -1}
	onProgress(api.ProfileProgress{ExecutionID: executionID, Stage: "failed", Step: -1, Failure: failure})
}

func (e *runnerExecutor) Status(ctx context.Context, request RunnerStatusRequest) (api.ProfileStatus, error) {
	var result api.ProfileStatus
	if err := api.ValidateExecutionID(request.ExecutionID); err != nil {
		return result, &api.Error{Code: "INVALID_ARGUMENT", Detail: err.Error()}
	}
	ctx, finish, err := e.app.adminContext(ctx)
	if err != nil {
		return result, err
	}
	defer finish()
	sdk, closeClient, err := e.connect(ctx, request.Principal, request.OwnerID, request.Binding, "profile.status")
	if err != nil {
		return result, err
	}
	defer closeClient()
	return sdk.ProfileStatus(ctx, request.ExecutionID)
}

func (e *runnerExecutor) connect(ctx context.Context, principal identity.User, ownerID string, binding runner.Binding, capability string) (*client.Client, func(), error) {
	grant, handler, err := e.app.authorizer.BackgroundRunner(ctx, principal, ownerID, binding)
	if err != nil {
		return nil, nil, err
	}
	return e.app.connectGrantedRunner(ctx, binding, capability, grant, handler)
}

func (a *App) connectGrantedRunner(ctx context.Context, binding runner.Binding, capability string, grant gateway.BindingContext, handler gateway.ConnectionHandler) (*client.Client, func(), error) {
	gatewayConn, sdkConn := net.Pipe()
	done := make(chan struct{})
	go func() {
		_ = a.core.ServeConn(ctx, gatewayConn, grant, handler)
		close(done)
	}()
	sdk, err := client.Connect(ctx, sdkConn, binding.MachineID)
	if err != nil {
		sdkConn.Close()
		<-done
		return nil, nil, err
	}
	found := false
	for _, available := range sdk.Binding.Capabilities {
		if available == capability {
			found = true
			break
		}
	}
	if !found {
		sdk.Close()
		<-done
		return nil, nil, &api.Error{Code: "UNSUPPORTED", Detail: capability + " is not supported by the current fabricd binding"}
	}
	return sdk, func() {
		sdk.Close()
		<-done
	}, nil
}
