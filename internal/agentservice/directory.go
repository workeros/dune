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
	page := agents.DirectoryPage{Items: []agents.Agent{}, Runners: []agents.RunnerAvailability{}, Issues: []agents.DiscoveryIssue{}, Complete: true}
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
		issues       []api.RuntimeDiscoveryIssue
		complete     bool
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
				results[i].complete = false
				if resource.Runner.Binding != nil {
					status.Online = online[resource.Runner.Binding.MachineID]
				}
				if status.Online {
					_, readyErr := s.Access.Check(ctx, scope.Principal, resource, "profile.start", resource.Runner.Kind)
					status.Ready = readyErr == nil
					callCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
					items, discovery, err := s.runnerAgents(callCtx, scope, resource)
					cancel()
					results[i].items = items
					results[i].issues, results[i].complete = discovery.Issues, discovery.Complete
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
		page.Complete = page.Complete && result.complete
		for _, issue := range result.issues {
			page.Issues = append(page.Issues, agents.DiscoveryIssue{RunnerID: result.availability.Runner.ID, Runtime: issue.Runtime, Code: issue.Code, ArtifactRef: issue.ArtifactRef})
		}
		if result.issue != "" {
			page.Issues = append(page.Issues, agents.DiscoveryIssue{RunnerID: result.availability.Runner.ID, Code: result.issue})
		}
	}
	return page, nil
}

func (s *Service) runnerAgents(ctx context.Context, scope agents.Scope, resource authorization.Resource) ([]agents.Agent, api.RuntimeList, error) {
	connection, closeConnection, err := s.Dial(ctx, scope, *resource.Runner.Binding, "runtime.list")
	if err != nil {
		return nil, api.RuntimeList{}, err
	}
	defer closeConnection()
	runtimes, err := connection.List(ctx)
	if err != nil {
		return nil, runtimes, err
	}
	items := make([]agents.Agent, 0, len(runtimes.Items))
	for _, runtime := range runtimes.Items {
		items = append(items, describe(resource.Runner, runtime))
	}
	return items, runtimes, nil
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
	runtime, err := currentRuntime(ctx, connection, ref)
	if err != nil {
		return agents.Agent{}, err
	}
	return describe(resource.Runner, runtime), nil
}

func currentRuntime(ctx context.Context, connection *client.Client, ref agentReference) (api.Runtime, error) {
	runtime, err := connection.Get(ctx, runtimeFor(ref.Target))
	if err != nil {
		return runtime, err
	}
	if runtime.Adapter != ref.Target.Runtime.Adapter {
		return api.Runtime{}, &api.Error{Code: "STALE_RUNTIME", Detail: "Runtime adapter does not match the selected Agent"}
	}
	if ref.NativeID != "" && (runtime.NativeSession == nil || runtime.NativeSession.ID != ref.NativeID || runtime.NativeSession.Cwd != ref.NativeCwd) {
		return api.Runtime{}, &api.Error{Code: "STALE_SESSION", Detail: "native session changed; discover the current Agent before continuing"}
	}
	return runtime, nil
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
	capability := operation
	// The permission vocabulary describes the business action; the advertised
	// transport capability is the mandatory identified submission envelope.
	if operation == "acp.action" {
		capability = "submission.acp"
	}
	connection, closeConnection, err := s.Dial(ctx, scope, target.Binding, capability)
	return resource, connection, closeConnection, err
}

// Discovery reads current fabricd state; it never creates a Runtime or a database record.
func describe(logical runner.Runner, runtime api.Runtime) agents.Agent {
	target := targetFor(*logical.Binding, runtime)
	return agents.Agent{Ref: agentRef(target, runtime.NativeSession), Target: target, Runtime: runtime, Runner: logical}
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
