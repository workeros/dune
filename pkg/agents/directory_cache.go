package agents

import (
	"encoding/json"
	"sort"
	"sync"

	"github.com/aiomni/dune/pkg/workbench"
)

// DiscoveryBatch binds asynchronous pages to one valid subscription lifetime.
// Its fields are private so an old result cannot be relabeled after reconnect.
type DiscoveryBatch struct {
	subscription string
	generation   uint64
}

// DirectoryCache is the SDK's merge boundary. Membership and subscription
// validity are checked before complete observation versions. Returned values
// are owned copies, not mutable references into the cache.
type DirectoryCache struct {
	mu           sync.Mutex
	subscription string
	generation   uint64
	valid        bool
	members      map[workbench.AgentTarget]Agent
}

func (c *DirectoryCache) Begin(subscriptionID string) DiscoveryBatch {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.generation++
	c.subscription, c.valid = subscriptionID, subscriptionID != ""
	c.members = make(map[workbench.AgentTarget]Agent)
	return DiscoveryBatch{subscription: subscriptionID, generation: c.generation}
}

func (c *DirectoryCache) Invalidate(subscriptionID string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.subscription == subscriptionID {
		c.invalidateLocked()
	}
}

func (c *DirectoryCache) invalidateLocked() {
	c.valid = false
	c.generation++
	c.members = nil
}

func (c *DirectoryCache) MergePage(batch DiscoveryBatch, page DirectoryPage) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	if !c.valid || batch.subscription != c.subscription || batch.generation != c.generation {
		return false
	}
	for _, agent := range page.Items {
		if !c.mergeLocked(agent, true) {
			c.invalidateLocked()
			return false
		}
	}
	// Missing items, especially in an incomplete page, are not removals.
	return true
}

func (c *DirectoryCache) Apply(event DirectoryEvent) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	if !c.valid || event.SubscriptionID != c.subscription {
		return false
	}
	if event.Kind == DirectoryInvalidated {
		c.invalidateLocked()
		return true
	}
	if event.Agent == nil || (event.Kind != DirectoryMember && event.Kind != DirectoryMetadata) {
		return false
	}
	if !c.mergeLocked(*event.Agent, event.Kind == DirectoryMember) {
		return false
	}
	return true
}

func (c *DirectoryCache) mergeLocked(next Agent, establishesMembership bool) bool {
	if next.Target.Validate() != nil || next.Runtime.ID != next.Target.Runtime.ID || next.Runtime.Incarnation != next.Target.Runtime.Incarnation || next.Runtime.Generation != next.Target.Runtime.Generation || next.Runtime.Adapter != next.Target.Runtime.Adapter || next.Runner.ID != next.Target.Binding.RunnerID || next.Runner.Binding == nil || *next.Runner.Binding != next.Target.Binding {
		return false
	}
	previous, exists := c.members[next.Target]
	if !exists && (!establishesMembership || len(c.members) >= MaxDirectoryMembers) {
		return false
	}
	if !next.Runtime.Observation.Valid() || (exists && next.Runtime.Observation.Epoch != previous.Runtime.Observation.Epoch) {
		c.invalidateLocked()
		return false
	}
	if exists && next.Runtime.Observation.Revision <= previous.Runtime.Observation.Revision {
		return true
	}
	if exists && previous.Runtime.SessionMetadata != nil && next.Runtime.SessionMetadata == nil {
		// Missing metadata is not an explicit title clear.
		next.Runtime.SessionMetadata = previous.Runtime.SessionMetadata
	}
	c.members[next.Target] = copyAgent(next)
	return true
}

func (c *DirectoryCache) Snapshot() []Agent {
	c.mu.Lock()
	defer c.mu.Unlock()
	items := make([]Agent, 0, len(c.members))
	for _, agent := range c.members {
		items = append(items, copyAgent(agent))
	}
	sort.Slice(items, func(i, j int) bool { return items[i].Target.Key() < items[j].Target.Key() })
	return items
}

func copyAgent(value Agent) Agent {
	data, _ := json.Marshal(value)
	var copied Agent
	_ = json.Unmarshal(data, &copied)
	return copied
}
