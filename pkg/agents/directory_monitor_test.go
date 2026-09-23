package agents

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/aiomni/dune/pkg/runner"
)

type monitorFixture struct {
	events    chan DirectoryEvent
	pages     chan DirectoryPage
	requested chan struct{}
}

func (f *monitorFixture) ID() string   { return "subscription" }
func (f *monitorFixture) Close() error { return nil }
func (f *monitorFixture) Subscribe(context.Context, Scope, DirectoryWatch) (DirectorySubscription, error) {
	return f, nil
}
func (f *monitorFixture) Next(ctx context.Context) (DirectoryEvent, error) {
	select {
	case e := <-f.events:
		return e, nil
	case <-ctx.Done():
		return DirectoryEvent{}, ctx.Err()
	}
}
func (f *monitorFixture) List(context.Context, Scope, runner.Query) (DirectoryPage, error) {
	f.requested <- struct{}{}
	// Deliberately ignore cancellation: an already produced response can arrive
	// after revocation. The SDK must fence it, not rely on network cancellation.
	return <-f.pages, nil
}
func (f *monitorFixture) Get(context.Context, Scope, string) (Agent, error) {
	panic("monitor must never query one Agent")
}

func TestDirectoryMonitorDiscoveryWindowAndInvalidationBarrier(t *testing.T) {
	ctx, cancel := context.WithTimeout(t.Context(), 3*time.Second)
	defer cancel()
	f := &monitorFixture{events: make(chan DirectoryEvent), pages: make(chan DirectoryPage), requested: make(chan struct{}, 1)}
	m, err := ObserveDirectory(ctx, f, f, Scope{}, DirectoryWatch{RunnerIDs: []string{"runner"}})
	if err != nil {
		t.Fatal(err)
	}
	<-f.requested
	newer := directoryCacheAgent(1<<53+2, "last update")
	f.events <- DirectoryEvent{SubscriptionID: f.ID(), Kind: DirectoryMember, Agent: &newer}
	page, err := m.Next(ctx)
	if err != nil || len(page.Items) != 1 || *page.Items[0].Runtime.SessionMetadata.Title != "last update" {
		t.Fatal(page, err)
	}
	older := directoryCacheAgent(1<<53+1, "old page")
	f.pages <- DirectoryPage{Items: []Agent{older}, Runners: []RunnerAvailability{{Runner: newer.Runner}, {Runner: runner.Runner{ID: "foreign"}}}, Issues: []DiscoveryIssue{{RunnerID: "foreign"}}, NextCursor: "next", Complete: true}
	<-f.requested
	page, err = m.Next(ctx)
	if err != nil || *page.Items[0].Runtime.SessionMetadata.Title != "last update" || len(page.Runners) != 1 || len(page.Issues) != 0 {
		t.Fatal(page, err)
	}
	f.events <- DirectoryEvent{SubscriptionID: f.ID(), Kind: DirectoryInvalidated, Code: "removed"}
	if _, err = m.Next(ctx); err == nil {
		t.Fatal("invalidation returned live members")
	}
	late := directoryCacheAgent(1<<53+100, "late higher revision")
	f.pages <- DirectoryPage{Items: []Agent{late}, Complete: true}
	if err = m.Close(); err != nil {
		t.Fatal(err)
	}
	if len(m.cache.Snapshot()) != 0 {
		t.Fatal("held page revived an invalidated member")
	}
	if _, err = m.Next(ctx); err == nil || errors.Is(err, context.DeadlineExceeded) {
		t.Fatal("monitor lost terminal state", err)
	}
}

func TestDirectoryMonitorSlowConsumerGetsFinalSnapshot(t *testing.T) {
	ctx, cancel := context.WithTimeout(t.Context(), 3*time.Second)
	defer cancel()
	f := &monitorFixture{events: make(chan DirectoryEvent), pages: make(chan DirectoryPage, 1), requested: make(chan struct{}, 1)}
	f.pages <- DirectoryPage{Complete: true}
	m, err := ObserveDirectory(ctx, f, f, Scope{}, DirectoryWatch{})
	if err != nil {
		t.Fatal(err)
	}
	defer m.Close()
	for i := uint64(1); i <= 1000; i++ {
		agent := directoryCacheAgent(i, "final")
		f.events <- DirectoryEvent{SubscriptionID: f.ID(), Kind: DirectoryMember, Agent: &agent}
	}
	for {
		page, err := m.Next(ctx)
		if err != nil {
			t.Fatal(err)
		}
		if len(page.Items) == 1 && page.Items[0].Runtime.SessionMetadata.Revision == 1000 {
			break
		}
	}
}
