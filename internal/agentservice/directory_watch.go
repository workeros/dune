package agentservice

import (
	"context"
	"errors"
	"sync"
	"time"

	"github.com/aiomni/dune/internal/authorization"
	"github.com/aiomni/dune/internal/latest"
	"github.com/aiomni/dune/internal/metadata"
	"github.com/aiomni/dune/internal/wire"
	"github.com/aiomni/dune/pkg/access"
	"github.com/aiomni/dune/pkg/agents"
	"github.com/aiomni/dune/pkg/api"
	"github.com/aiomni/dune/pkg/runner"
	"github.com/aiomni/dune/pkg/workbench"
)

const maxDirectoryRunners = 128

type directorySubscription struct {
	service *Service
	scope   agents.Scope
	query   agents.DirectoryWatch
	id      string
	ctx     context.Context
	cancel  context.CancelCauseFunc
	mailbox *latest.Mailbox[workbench.AgentTarget, agents.DirectoryEvent]
	done    chan struct{}
	ready   chan struct{}
	workers sync.WaitGroup
	once    sync.Once
}

func (s *Service) Subscribe(ctx context.Context, scope agents.Scope, query agents.DirectoryWatch) (agents.DirectorySubscription, error) {
	if scope.OwnerID == "" {
		return nil, invalid("directory subscriptions require a Tenant owner")
	}
	if len(query.RunnerIDs) > maxDirectoryRunners {
		return nil, invalid("directory subscriptions accept at most 128 Runner IDs")
	}
	query.RunnerIDs = append([]string(nil), query.RunnerIDs...)
	ctx, cancel := context.WithCancelCause(ctx)
	sub := &directorySubscription{service: s, scope: scope, query: query, id: wire.ID(), ctx: ctx, cancel: cancel,
		mailbox: latest.New[workbench.AgentTarget, agents.DirectoryEvent](agents.MaxDirectoryMembers), done: make(chan struct{}), ready: make(chan struct{})}
	resources, err := sub.resources(ctx)
	if err != nil {
		cancel(err)
		return nil, err
	}
	go sub.run(resources)
	select {
	case <-sub.ready:
	case <-ctx.Done():
		_ = sub.Close()
		return nil, context.Cause(ctx)
	}
	if ctx.Err() != nil {
		_ = sub.Close()
		return nil, context.Cause(ctx)
	}
	return sub, nil
}

func (s *directorySubscription) ID() string { return s.id }

func (s *directorySubscription) Close() error {
	s.fail("directory subscription closed; establish a new subscription before discovery")
	<-s.done
	return nil
}

func (s *directorySubscription) fail(detail string) {
	s.failWith(&api.Error{Code: "RESYNC_REQUIRED", Detail: detail})
}

func (s *directorySubscription) failWith(err error) {
	s.once.Do(func() {
		s.mailbox.Fail(err)
		s.cancel(err)
	})
}

func (s *directorySubscription) Next(ctx context.Context) (agents.DirectoryEvent, error) {
	readCtx, cancel := context.WithCancel(ctx)
	stop := context.AfterFunc(s.ctx, cancel)
	defer stop()
	defer cancel()
	event, err := s.mailbox.Next(readCtx)
	if err != nil {
		if ctx.Err() != nil {
			return agents.DirectoryEvent{}, ctx.Err()
		}
		s.fail("directory continuity was interrupted; resubscribe and rediscover")
		return agents.DirectoryEvent{}, &api.Error{Code: "RESYNC_REQUIRED", Detail: "directory continuity was interrupted"}
	}
	// Check current membership and policy after dequeue, not only when the
	// source admitted the event. Buffered observations grant no enduring access.
	resource, _, err := s.service.Access.Resource(readCtx, s.scope.Principal, event.Agent.Target.Binding.RunnerID, false, "runtime.list")
	if err != nil || resource.OwnerID != s.scope.OwnerID || resource.Runner.Binding == nil || *resource.Runner.Binding != event.Agent.Target.Binding || s.ctx.Err() != nil {
		s.fail("Runner access or binding changed; reauthorize discovery")
		return agents.DirectoryEvent{}, &api.Error{Code: "RESYNC_REQUIRED", Detail: "Runner access or binding changed"}
	}
	event.Agent.Runner = resource.Runner
	return event, nil
}

func (s *directorySubscription) resources(ctx context.Context) ([]authorization.Resource, error) {
	if len(s.query.RunnerIDs) > 0 {
		if _, err := s.service.Access.Check(ctx, s.scope.Principal, authorization.Resource{OwnerID: s.scope.OwnerID}, "runner.list", "tenant"); err != nil {
			return nil, err
		}
		resources := make([]authorization.Resource, 0, len(s.query.RunnerIDs))
		seen := map[string]bool{}
		for _, id := range s.query.RunnerIDs {
			if id == "" || seen[id] {
				return nil, invalid("directory Runner IDs must be nonempty and unique")
			}
			seen[id] = true
			resource, _, err := s.service.Access.Resource(ctx, s.scope.Principal, id, false, "runtime.list")
			if err != nil {
				return nil, err
			}
			if resource.OwnerID != s.scope.OwnerID {
				return nil, metadata.ErrNotFound
			}
			resources = append(resources, resource)
		}
		return resources, nil
	}
	var resources []authorization.Resource
	query := runner.Query{Limit: 100}
	for {
		page, err := s.service.Access.DiscoverTenant(ctx, s.scope.Principal, s.scope.OwnerID, query)
		if err != nil {
			return nil, err
		}
		for _, resource := range page.Items {
			if resource.OwnerID != s.scope.OwnerID {
				return nil, metadata.ErrNotFound
			}
			_, err := s.service.Access.Check(ctx, s.scope.Principal, resource, "runtime.list", resource.Runner.Kind)
			if errors.Is(err, access.ErrDenied) && !errors.Is(err, access.ErrUnavailable) {
				continue
			}
			if err != nil {
				return nil, err
			}
			resources = append(resources, resource)
		}
		if len(resources) > maxDirectoryRunners {
			return nil, &api.Error{Code: "RESOURCE_EXHAUSTED", Detail: "directory subscription exceeds 128 Runners; select a smaller range"}
		}
		if page.NextCursor == "" {
			return resources, nil
		}
		query.Cursor = page.NextCursor
	}
}

func (s *directorySubscription) run(resources []authorization.Resource) {
	defer close(s.done)
	defer s.workers.Wait()
	defer s.cancel(nil)
	bindings := make(map[string]runner.Binding)
	var initial sync.WaitGroup
	start := func(resource authorization.Resource) {
		var binding runner.Binding
		if resource.Runner.Binding != nil {
			binding = *resource.Runner.Binding
		}
		bindings[resource.Runner.ID] = binding
		if binding.Valid() {
			initial.Add(1)
			s.workers.Go(func() { s.observeRunner(resource, initial.Done) })
		}
	}
	for _, resource := range resources {
		start(resource)
	}
	initial.Wait()
	close(s.ready)
	ticker := time.NewTicker(time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-s.ctx.Done():
			return
		case <-ticker.C:
		}
		// This scans authorization/Runner membership only. Runtime metadata comes
		// exclusively from the Runner streams, never a periodic list/get sweep.
		current, err := s.resources(s.ctx)
		if err != nil {
			s.fail("directory authorization or membership could not be confirmed")
			return
		}
		present := make(map[string]bool, len(current))
		for _, resource := range current {
			present[resource.Runner.ID] = true
			var binding runner.Binding
			if resource.Runner.Binding != nil {
				binding = *resource.Runner.Binding
			}
			if previous, exists := bindings[resource.Runner.ID]; exists {
				if previous != binding {
					s.fail("Runner binding changed; reauthorize discovery")
					return
				}
			} else {
				s.fail("Runner joined the directory; resubscribe before discovery")
				return
			}
		}
		for id := range bindings {
			if !present[id] {
				s.fail("Runner left the authorized directory range")
				return
			}
		}
	}
}

func (s *directorySubscription) observeRunner(resource authorization.Resource, ready func()) {
	binding := *resource.Runner.Binding
	var announce sync.Once
	defer announce.Do(ready)
	initial := true
	for {
		connection, closeConnection, err := s.service.Dial(s.ctx, s.scope, binding, "runtime.watch")
		if err == nil {
			watch, watchErr := connection.SubscribeRuntimes(s.ctx)
			if watchErr == nil {
				if !initial {
					watch.Close()
					closeConnection()
					s.fail("previously unavailable Runner reconnected; rediscover current membership")
					return
				}
				announce.Do(ready)
				for {
					change, readErr := watch.Next()
					if readErr != nil || change.Removed {
						watch.Close()
						closeConnection()
						s.fail("Runner stream or directory membership changed")
						return
					}
					agent := describe(resource.Runner, change.Runtime)
					// A member event explicitly confirms membership within this
					// subscription. Its title is not independently authoritative.
					s.mailbox.Offer(agent.Target, agents.DirectoryEvent{SubscriptionID: s.id, Kind: agents.DirectoryMember, Agent: &agent})
				}
			}
			closeConnection()
			err = watchErr
		}
		if s.ctx.Err() != nil {
			return
		}
		var failure *api.Error
		if errors.As(err, &failure) {
			switch failure.Code {
			case "RESOURCE_EXHAUSTED", "RESYNC_REQUIRED", "ACCESS_DENIED", "UNSUPPORTED", "BINDING_CHANGED":
				// Admission failure is not an offline source. Preserve its cause
				// before releasing the setup barrier so Subscribe cannot report ready.
				s.failWith(failure)
				return
			}
		}
		initial = false
		announce.Do(ready)
		// An initially offline Runner can later join. Its first stream snapshot
		// covers all changes before that connection, even with no later events.
		timer := time.NewTimer(time.Second)
		select {
		case <-s.ctx.Done():
			timer.Stop()
			return
		case <-timer.C:
		}
	}
}
