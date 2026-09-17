package agentservice

import (
	"context"
	"errors"
	"sync"
	"time"

	"github.com/aiomni/dune/internal/authorization"
	"github.com/aiomni/dune/internal/metadata"
	"github.com/aiomni/dune/pkg/agents"
	"github.com/aiomni/dune/pkg/api"
	"github.com/aiomni/dune/pkg/client"
	"github.com/aiomni/dune/pkg/runner"
	"github.com/aiomni/dune/pkg/workbench"
)

func (s *Service) List(ctx context.Context, scope agents.Scope, query runner.Query) (agents.DirectoryPage, error) {
	ctx, cancel := context.WithTimeout(ctx, 20*time.Second)
	defer cancel()
	page := agents.DirectoryPage{Items: []agents.Agent{}, Runners: []agents.RunnerAvailability{}, Issues: []agents.DiscoveryIssue{}}
	discovered, err := s.Access.DiscoverTenant(ctx, scope.Principal, scope.OwnerID, query)
	if err != nil {
		return page, err
	}
	page.NextCursor = discovered.NextCursor
	ids := make([]string, 0, len(discovered.Items))
	for _, resource := range discovered.Items {
		if resource.OwnerID != scope.OwnerID {
			return page, metadata.ErrNotFound
		}
		if resource.Runner.Binding != nil {
			ids = append(ids, resource.Runner.Binding.MachineID)
		}
	}
	if s.Online == nil {
		return page, &api.Error{Code: "UNAVAILABLE", Detail: "Runner presence lookup is not configured"}
	}
	online, err := s.Online(ctx, ids)
	if err != nil {
		return page, err
	}
	type result struct {
		availability agents.RunnerAvailability
		items        []agents.Agent
		issue        string
	}
	results := make([]result, len(discovered.Items))
	jobs := make(chan int, len(discovered.Items))
	for i := range discovered.Items {
		jobs <- i
	}
	close(jobs)
	var group sync.WaitGroup
	for range min(4, len(discovered.Items)) {
		group.Go(func() {
			for i := range jobs {
				resource := discovered.Items[i]
				status := agents.RunnerAvailability{Runner: resource.Runner}
				if resource.Runner.Binding != nil {
					status.Online = online[resource.Runner.Binding.MachineID]
				}
				if status.Online {
					_, readyErr := s.Access.Check(ctx, scope.Principal, resource, "profile.start", resource.Runner.Kind)
					status.Ready = readyErr == nil
					callCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
					items, err := s.runnerAgents(callCtx, scope, resource)
					cancel()
					results[i].items = items
					if err != nil {
						results[i].issue = discoveryError(err)
						status.Ready = false
					}
				}
				results[i].availability = status
			}
		})
	}
	group.Wait()
	for _, result := range results {
		page.Runners = append(page.Runners, result.availability)
		page.Items = append(page.Items, result.items...)
		if result.issue != "" {
			page.Issues = append(page.Issues, agents.DiscoveryIssue{RunnerID: result.availability.Runner.ID, Code: result.issue})
		}
	}
	return page, nil
}

func (s *Service) runnerAgents(ctx context.Context, scope agents.Scope, resource authorization.Resource) ([]agents.Agent, error) {
	connection, closeConnection, err := s.Dial(ctx, scope, *resource.Runner.Binding, "runtime.list")
	if err != nil {
		return nil, err
	}
	defer closeConnection()
	runtimes, err := connection.List(ctx)
	if err != nil {
		return nil, err
	}
	items := make([]agents.Agent, 0, len(runtimes))
	for _, runtime := range runtimes {
		items = append(items, s.describe(ctx, scope, resource.Runner, runtime))
	}
	return items, nil
}

func (s *Service) Get(ctx context.Context, scope agents.Scope, value string) (agents.Agent, error) {
	ref, err := parseAgentRef(value)
	if err != nil {
		return agents.Agent{}, err
	}
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	resource, connection, closeConnection, err := s.connect(ctx, scope, ref.Target, "runtime.get")
	if err != nil {
		return agents.Agent{}, err
	}
	defer closeConnection()
	runtime, err := connection.Get(ctx, runtimeFor(ref.Target))
	if err != nil {
		return agents.Agent{}, err
	}
	if ref.NativeID != "" && (runtime.NativeSession == nil || runtime.NativeSession.ID != ref.NativeID || runtime.NativeSession.Cwd != ref.NativeCwd) {
		return agents.Agent{}, &api.Error{Code: "STALE_SESSION", Detail: "native session changed; discover the current Agent before continuing"}
	}
	return s.describe(ctx, scope, resource.Runner, runtime), nil
}

func (s *Service) connect(ctx context.Context, scope agents.Scope, target workbench.AgentTarget, operation string) (authorization.Resource, *client.Client, func(), error) {
	resource, _, err := s.Access.Resource(ctx, scope.Principal, target.Binding.RunnerID, false, operation)
	if err != nil {
		return resource, nil, nil, err
	}
	if scope.OwnerID == "" || resource.OwnerID != scope.OwnerID {
		return resource, nil, nil, metadata.ErrNotFound
	}
	if resource.Runner.Binding == nil || *resource.Runner.Binding != target.Binding {
		return resource, nil, nil, runner.ErrBindingChanged
	}
	connection, closeConnection, err := s.Dial(ctx, scope, target.Binding, operation)
	return resource, connection, closeConnection, err
}

// All values here came from an authorized fabricd reply, never from a browser
// or model-supplied observation. Index failures preserve the actual Runtime.
func (s *Service) describe(ctx context.Context, scope agents.Scope, logical runner.Runner, runtime api.Runtime) agents.Agent {
	target := targetFor(*logical.Binding, runtime)
	item := agents.Agent{Ref: agentRef(target, runtime.NativeSession), Target: target, Runtime: runtime, Runner: logical}
	if runtime.NativeSession != nil {
		if _, err := s.Store.ObserveAgentSession(ctx, scope.OwnerID, target, *runtime.NativeSession); err != nil && !errors.Is(err, metadata.ErrNotFound) {
			item.RecoveryError = "RECOVERY_INDEX_UNAVAILABLE"
		}
	}
	session, err := s.Store.RuntimeAgentSession(ctx, scope.OwnerID, target)
	if err == nil {
		if runtime.NativeSession == nil || session.Native != nil && session.Native.ID == runtime.NativeSession.ID && session.Native.Cwd == runtime.NativeSession.Cwd {
			summary := session.Summary()
			item.Session = &summary
		} else {
			item.RecoveryError = "RECOVERY_INDEX_UNAVAILABLE"
		}
	} else if !errors.Is(err, metadata.ErrNotFound) {
		item.RecoveryError = "RECOVERY_INDEX_UNAVAILABLE"
	}
	return item
}

func discoveryError(err error) string {
	var failure *api.Error
	if errors.As(err, &failure) {
		switch failure.Code {
		case "OFFLINE", "UNSUPPORTED", "ACCESS_DENIED", "BINDING_CHANGED":
			return failure.Code
		}
	}
	return "RUNNER_UNAVAILABLE"
}
