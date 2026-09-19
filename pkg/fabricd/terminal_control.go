package fabricd

import (
	"sync"

	"github.com/aiomni/dune/pkg/api"
)

// Output subscriptions and input ownership have separate lifetimes. Every PTY
// viewer keeps receiving output when control moves to another stream.
type terminalControl struct {
	mu      sync.Mutex
	viewers map[*subscription]bool
	owner   *subscription
	epoch   uint64
}

func (c *terminalControl) add(s *subscription) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.viewers == nil {
		c.viewers = make(map[*subscription]bool)
	}
	c.viewers[s] = true
	if s.canInput && c.owner == nil {
		c.owner = s
		c.epoch++
	}
	c.notifyLocked()
}

func (c *terminalControl) remove(s *subscription) {
	c.mu.Lock()
	defer c.mu.Unlock()
	delete(c.viewers, s)
	if c.owner == s {
		c.owner = nil
		c.epoch++
		c.notifyLocked()
	}
}

func (c *terminalControl) change(s *subscription, action string) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if !c.viewers[s] || !s.canInput {
		return &api.Error{Code: "READ_ONLY", Detail: "observe-only stream cannot control the terminal"}
	}
	switch action {
	case "acquire":
		if c.owner == nil {
			c.owner = s
			c.epoch++
		}
	case "take":
		if c.owner != s {
			c.owner = s
			c.epoch++
		}
	case "release":
		if c.owner == s {
			c.owner = nil
			c.epoch++
		}
	default:
		return &api.Error{Code: "INVALID_ARGUMENT", Detail: "control action must be acquire, take or release"}
	}
	c.notifyLocked()
	return nil
}

func (c *terminalControl) notifyLocked() {
	for s := range c.viewers {
		select {
		case s.controlChanged <- struct{}{}:
		default:
		}
	}
}

func (c *terminalControl) state(s *subscription) (api.TerminalControlState, uint64) {
	c.mu.Lock()
	defer c.mu.Unlock()
	writable := c.owner == s && c.viewers[s]
	epoch := uint64(0)
	if writable {
		epoch = c.epoch
	}
	return api.TerminalControlState{Writable: writable, Available: c.owner == nil}, epoch
}

// Check at the actual write, including after waiting in the shared PTY queue.
// The epoch also rejects old buffered input after this same viewer takes back
// control. Never hold this lock while sending network notifications.
func (c *terminalControl) write(s *subscription, epoch uint64, write func() error) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if epoch == 0 || epoch != c.epoch || c.owner != s || !c.viewers[s] {
		return &api.Error{Code: "READ_ONLY", Detail: "terminal input control has changed"}
	}
	return write()
}
