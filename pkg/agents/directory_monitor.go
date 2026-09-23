package agents

import (
	"context"
	"fmt"
	"sync"

	"github.com/aiomni/dune/pkg/runner"
)

// DirectoryMonitor owns subscription, discovery and cache invalidation for SDK
// consumers. Next returns the latest local projection; slow consumers coalesce
// changes. An error discards the projection before it becomes observable.
type DirectoryMonitor struct {
	cache        DirectoryCache
	subscription DirectorySubscription
	cancel       context.CancelFunc
	workers      sync.WaitGroup
	once         sync.Once
	mu           sync.Mutex
	err          error
	page         DirectoryPage
	changed      chan struct{}
}

func ObserveDirectory(ctx context.Context, directory Directory, observer DirectoryObserver, scope Scope, query DirectoryWatch) (*DirectoryMonitor, error) {
	ctx, cancel := context.WithCancel(ctx)
	subscription, err := observer.Subscribe(ctx, scope, query)
	if err != nil {
		cancel()
		return nil, err
	}
	m := &DirectoryMonitor{subscription: subscription, cancel: cancel, changed: make(chan struct{}, 1)}
	batch := m.cache.Begin(subscription.ID())
	m.workers.Add(2)
	go func() {
		defer m.workers.Done()
		for {
			event, err := subscription.Next(ctx)
			if err != nil {
				m.fail(err)
				return
			}
			if event.Kind == DirectoryInvalidated {
				m.fail(fmt.Errorf("directory invalidated: %s", event.Code))
				return
			}
			if !m.cache.Apply(event) {
				m.fail(fmt.Errorf("invalid directory observation; resynchronize"))
				return
			}
			m.notify()
		}
	}()
	go func() {
		defer m.workers.Done()
		m.discover(ctx, directory, scope, query, batch)
	}()
	return m, nil
}

func (m *DirectoryMonitor) fail(err error) {
	m.once.Do(func() {
		m.mu.Lock()
		m.err = err
		m.cache.Invalidate(m.subscription.ID())
		m.mu.Unlock()
		m.cancel()
		_ = m.subscription.Close()
		m.notify()
	})
}

func (m *DirectoryMonitor) Close() error {
	m.fail(context.Canceled)
	m.workers.Wait()
	return nil
}

func (m *DirectoryMonitor) notify() {
	select {
	case m.changed <- struct{}{}:
	default:
	}
}

func (m *DirectoryMonitor) Next(ctx context.Context) (DirectoryPage, error) {
	select {
	case <-ctx.Done():
		return DirectoryPage{}, ctx.Err()
	case <-m.changed:
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.err != nil {
		m.notify() // A terminated monitor remains observably terminal.
		return DirectoryPage{}, m.err
	}
	page := m.page
	page.Items = m.cache.Snapshot()
	page.Runners = append([]RunnerAvailability(nil), page.Runners...)
	page.Issues = append([]DiscoveryIssue(nil), page.Issues...)
	return page, nil
}

func (m *DirectoryMonitor) discover(ctx context.Context, directory Directory, scope Scope, watch DirectoryWatch, batch DiscoveryBatch) {
	selected := make(map[string]bool, len(watch.RunnerIDs))
	for _, id := range watch.RunnerIDs {
		selected[id] = true
	}
	query, complete := runner.Query{Limit: 100}, true
	seen := map[string]bool{}
	for {
		page, err := directory.List(ctx, scope, query)
		if err != nil {
			m.fail(err)
			return
		}
		if len(selected) > 0 {
			items := make([]Agent, 0, len(page.Items))
			for _, agent := range page.Items {
				if selected[agent.Target.Binding.RunnerID] {
					items = append(items, agent)
				}
			}
			page.Items = items
			page.Runners = filterRunners(page.Runners, selected)
			page.Issues = filterIssues(page.Issues, selected)
		}
		if !m.cache.MergePage(batch, page) {
			m.fail(fmt.Errorf("directory discovery batch is no longer valid"))
			return
		}
		complete = complete && page.Complete
		m.mu.Lock()
		m.page.Runners = append(m.page.Runners, page.Runners...)
		m.page.Issues = append(m.page.Issues, page.Issues...)
		m.page.Complete = page.NextCursor == "" && complete
		m.mu.Unlock()
		m.notify()
		if page.NextCursor == "" {
			return
		}
		if seen[page.NextCursor] {
			m.fail(fmt.Errorf("directory discovery cursor did not advance"))
			return
		}
		seen[page.NextCursor] = true
		query.Cursor = page.NextCursor
	}
}

func filterRunners(values []RunnerAvailability, selected map[string]bool) []RunnerAvailability {
	var result []RunnerAvailability
	for _, value := range values {
		if selected[value.Runner.ID] {
			result = append(result, value)
		}
	}
	return result
}

func filterIssues(values []DiscoveryIssue, selected map[string]bool) []DiscoveryIssue {
	var result []DiscoveryIssue
	for _, value := range values {
		if selected[value.RunnerID] {
			result = append(result, value)
		}
	}
	return result
}
