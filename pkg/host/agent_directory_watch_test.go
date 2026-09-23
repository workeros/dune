package host

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"github.com/aiomni/dune/internal/authorization"
	"github.com/aiomni/dune/internal/identity"
	"github.com/aiomni/dune/pkg/access"
	"github.com/aiomni/dune/pkg/agents"
	"github.com/aiomni/dune/pkg/api"
	"github.com/aiomni/dune/pkg/client"
	"github.com/aiomni/dune/pkg/runner"
)

func TestAgentDirectorySubscriptionDiscoversTitlesWithoutNativeControls(t *testing.T) {
	f := openExecutorFixture(t)
	ctx, cancel := context.WithTimeout(t.Context(), 20*time.Second)
	defer cancel()
	subscription, err := f.app.AgentDirectoryObserver().Subscribe(ctx, f.agentScope(), agents.DirectoryWatch{RunnerIDs: []string{f.binding.RunnerID}})
	if err != nil {
		t.Fatal(err)
	}
	defer subscription.Close()
	journal := filepath.Join(f.workspace, "watch-rpc.log")
	_, _ = directoryACPWithEnvironment(t, f, map[string]string{"DUNE_HOST_FAKE_ACP_TITLE": "directory title", "DUNE_HOST_FAKE_ACP_METHODS": journal})
	var cache agents.DirectoryCache
	cache.Begin(subscription.ID())
	for {
		event, err := subscription.Next(ctx)
		if err != nil {
			t.Fatal(err)
		}
		if !cache.Apply(event) {
			t.Fatal("SDK rejected an authorized event", event)
		}
		if event.Agent.Runtime.SessionMetadata != nil && event.Agent.Runtime.SessionMetadata.Title != nil {
			if *event.Agent.Runtime.SessionMetadata.Title != "directory title" {
				t.Fatal(event)
			}
			break
		}
	}
	before, err := os.ReadFile(journal)
	if err != nil {
		t.Fatal(err)
	}
	after, err := os.ReadFile(journal)
	if err != nil || string(before) != string(after) {
		t.Fatal("directory subscription dispatched native controls", err)
	}
}

type directoryReadPolicy struct{}

func (directoryReadPolicy) Check(_ context.Context, request access.Request) (access.Decision, error) {
	return access.Decision{Allowed: request.Operation != "runtime.list", Reason: "DIRECTORY_READ", ID: request.RequestID, ValidUntil: time.Now().Add(access.MaxLease)}, nil
}
func TestAgentDirectorySubscriptionFiltersUnauthorizedRunnersBeforeDial(t *testing.T) {
	f := openExecutorFixture(t)
	service := f.app.agentService()
	service.Access = authorization.New(t.Context(), identity.NewLocal(f.app.store, false), f.app.store, directoryReadPolicy{}, nil)
	service.Dial = func(context.Context, agents.Scope, runner.Binding, string) (*client.Client, func(), error) {
		t.Error("denied Runner reached the transport")
		return nil, nil, errors.New("unexpected transport")
	}
	subscription, err := service.Subscribe(t.Context(), f.agentScope(), agents.DirectoryWatch{})
	if err != nil {
		t.Fatal("denied Runner broke the remaining authorized range", err)
	}
	subscription.Close()
	if _, err := service.Subscribe(t.Context(), f.agentScope(), agents.DirectoryWatch{RunnerIDs: []string{f.binding.RunnerID}}); err == nil {
		t.Fatal("explicit denied Runner accepted")
	}
}

// The marker reaches only the delivery-time authorization check, after an event
// has been dequeued. Pausing there proves the test exercises buffered data.
type directoryDeliveryBarrier struct {
	entered chan struct{}
	release chan struct{}
}
type revocableDirectoryPolicy struct{ denied atomic.Bool }

func (p *revocableDirectoryPolicy) Check(ctx context.Context, request access.Request) (access.Decision, error) {
	if barrier, ok := ctx.Value(directoryDeliveryBarrier{}).(*directoryDeliveryBarrier); ok && request.Operation == "runtime.list" {
		close(barrier.entered)
		select {
		case <-barrier.release:
		case <-ctx.Done():
			return access.Decision{}, ctx.Err()
		}
	}
	return access.Decision{Allowed: !p.denied.Load(), Reason: "DIRECTORY_POLICY", ID: request.RequestID, ValidUntil: time.Now().Add(access.MaxLease)}, nil
}

func TestAgentDirectorySubscriptionRechecksBufferedDelivery(t *testing.T) {
	for _, revoked := range []bool{false, true} {
		name := "still_authorized"
		if revoked {
			name = "revoked"
		}
		t.Run(name, func(t *testing.T) {
			f := openExecutorFixtureFor(t, 45*time.Second)
			launched, _ := directoryACP(t, f)
			policy := &revocableDirectoryPolicy{}
			service := f.app.agentService()
			service.Access = authorization.New(f.app.ctx, identity.NewLocal(f.app.store, false), f.app.store, policy, nil)
			ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
			defer cancel()
			sub, err := service.Subscribe(ctx, f.agentScope(), agents.DirectoryWatch{RunnerIDs: []string{f.binding.RunnerID}})
			if err != nil {
				t.Fatal(err)
			}
			defer sub.Close()
			barrier := &directoryDeliveryBarrier{entered: make(chan struct{}), release: make(chan struct{})}
			type delivery struct {
				event agents.DirectoryEvent
				err   error
			}
			result := make(chan delivery, 1)
			go func() {
				event, err := sub.Next(context.WithValue(ctx, directoryDeliveryBarrier{}, barrier))
				result <- delivery{event, err}
			}()
			select {
			case <-barrier.entered:
			case <-ctx.Done():
				t.Fatal("event never reached the delivery barrier", ctx.Err())
			}
			policy.denied.Store(revoked)
			close(barrier.release)
			var got delivery
			select {
			case got = <-result:
			case <-ctx.Done():
				t.Fatal("buffered delivery did not finish", ctx.Err())
			}
			if ctx.Err() != nil || f.app.ctx.Err() != nil || !f.app.core.Online(f.binding.MachineID) {
				t.Fatal("fixture shutdown cannot prove revocation", ctx.Err(), f.app.ctx.Err())
			}
			if revoked {
				var failure *api.Error
				if !errors.As(got.err, &failure) || failure.Code != "RESYNC_REQUIRED" || got.event.Agent != nil {
					t.Fatal("revoked buffered data was delivered", got)
				}
			} else if got.err != nil || got.event.Agent == nil || got.event.Agent.Runtime.ID != launched.Runtime.ID {
				t.Fatal("authorized buffered data was not delivered", got)
			}
		})
	}
}

func TestAgentDirectorySubscriptionRejectsWatchCapacityExhaustion(t *testing.T) {
	f := openExecutorFixtureFor(t, 45*time.Second)
	ctx, cancel := context.WithTimeout(t.Context(), 20*time.Second)
	defer cancel()
	for i := 0; i < 16; i++ {
		connection, closeConnection, err := (&runnerExecutor{app: f.app}).connect(ctx, f.principal, f.owner, f.binding, "runtime.watch")
		if err != nil {
			t.Fatal(i, err)
		}
		defer closeConnection()
		watch, err := connection.SubscribeRuntimes(ctx)
		if err != nil {
			t.Fatal(i, err)
		}
		defer watch.Close()
	}
	sub, err := f.app.AgentDirectoryObserver().Subscribe(ctx, f.agentScope(), agents.DirectoryWatch{RunnerIDs: []string{f.binding.RunnerID}})
	if sub != nil {
		defer sub.Close()
		t.Fatal("exhausted watch reported ready")
	}
	var failure *api.Error
	if !errors.As(err, &failure) || failure.Code != "RESOURCE_EXHAUSTED" {
		t.Fatal("watch admission lost its capacity error", err)
	}
	if ctx.Err() != nil || f.app.ctx.Err() != nil {
		t.Fatal("admission waited for fixture shutdown")
	}
}
