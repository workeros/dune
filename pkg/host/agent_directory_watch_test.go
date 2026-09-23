package host

import (
	"context"
	"errors"
	"os"
	"path/filepath"
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

func TestAgentDirectorySubscriptionDiscoversTitlesAndRevokesBufferedData(t *testing.T) {
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
	if err := f.app.SetUserEnabled(ctx, f.principal.ID, false); err != nil {
		t.Fatal(err)
	}
	_, err = subscription.Next(ctx)
	var failure *api.Error
	if !errors.As(err, &failure) || failure.Code != "RESYNC_REQUIRED" {
		t.Fatal("revocation did not invalidate the stream", err)
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
