package access

import (
	"context"
	"sync"
	"time"

	"github.com/aiomni/dune/pkg/gateway"
	pb "github.com/aiomni/dune/proto/dune/dtp/v1"
)

type lease struct {
	decision   Decision
	refreshAt  time.Time
	refreshing bool
}
type checkedStream struct {
	changed chan struct{}
	grant   Grant
	base    Request
	flow    *gateway.Stream
	ctx     context.Context
	cancel  context.CancelFunc
	mu      sync.Mutex
	runtime RuntimeIdentity
	leases  map[Request]*lease
}

func newLease(d Decision) *lease {
	return &lease{decision: d, refreshAt: time.Now().Add(time.Until(d.ValidUntil) / 2)}
}

// checkAction refreshes all constraints before establishing a new action lease.
// No callback is evaluated per data frame; the stream watchdog and Message both
// enforce this same deadline, including when a callback returns too late.
func (g Grant) checkAction(ctx context.Context, request Request) (Decision, error) {
	started := time.Now()
	if err := g.check(); err != nil {
		return Decision{}, err
	}
	decision, err := g.Policy.check(ctx, request)
	if err != nil {
		return Decision{}, err
	}
	if deadline := started.Add(StreamLeaseLimit); deadline.Before(decision.ValidUntil) {
		decision.ValidUntil = deadline
	}
	if ctx.Err() != nil || !time.Now().Before(decision.ValidUntil) {
		return Decision{}, ErrDenied
	}
	return decision, nil
}

func (c *connection) openChecked(ctx context.Context, m *pb.Message, flow *gateway.Stream) (gateway.StreamHandler, error) {
	r, err := Describe(c.grant.Policy.Scope, m)
	if err != nil {
		return nil, err
	}
	d, err := c.grant.checkAction(ctx, r)
	if err != nil {
		return nil, err
	}
	leases := map[Request]*lease{r: newLease(d)}
	additional, err := launchWorktreeRequest(c.grant.Policy.Scope, m)
	if err != nil {
		return nil, err
	}
	if additional != nil {
		decision, err := c.grant.checkAction(ctx, *additional)
		if err != nil {
			return nil, err
		}
		leases[*additional] = newLease(decision)
		if decision.ValidUntil.Before(d.ValidUntil) {
			d.ValidUntil = decision.ValidUntil
		}
	}
	if route, remote := flow.PeerRoute(); remote {
		if c.grant.Policy.Delegate == nil {
			return nil, ErrDenied
		}
		bounded, cancel := context.WithDeadline(ctx, d.ValidUntil)
		var value []byte
		value, err = c.grant.Policy.Delegate(bounded, route, m, r, d)
		if err == nil {
			err = bounded.Err()
		}
		cancel()
		if err == nil {
			err = flow.ForwardPeer(value)
		}
	} else {
		err = flow.Forward()
	}
	if err != nil || !time.Now().Before(d.ValidUntil) {
		if err == nil {
			err = ErrDenied
		}
		return nil, err
	}
	life, cancel := context.WithCancel(flow.Context())
	s := &checkedStream{grant: c.grant, base: r, flow: flow, ctx: life, cancel: cancel, leases: leases, changed: make(chan struct{}, 1)}
	go s.watch()
	return s, nil
}

// ensure reuses only this stream's exact action lease. New input actions obtain
// a distinct decision; terminal data never enters the cache or checker.
func (s *checkedStream) ensure(ctx context.Context, r Request) error {
	s.mu.Lock()
	if s.ctx.Err() != nil {
		s.mu.Unlock()
		return ErrDenied
	}
	cached := s.leases[r]
	s.mu.Unlock()
	if cached != nil {
		if !time.Now().Before(cached.decision.ValidUntil) {
			return ErrDenied
		}
		return nil
	}
	d, err := s.grant.checkAction(ctx, r)
	if err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.ctx.Err() != nil || !time.Now().Before(d.ValidUntil) {
		return ErrDenied
	}
	s.leases[r] = newLease(d)
	s.notify()
	return nil
}

func (s *checkedStream) Message(ctx context.Context, direction gateway.Direction, m *pb.Message) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if err := s.ensure(ctx, s.base); err != nil {
		return err
	}
	if direction == gateway.ToClient {
		// Runtime identity comes from fabricd before any continuation may pass.
		// The shell/prompt contents of later messages are never decoded here.
		if (s.base.Operation == "runtime.attach" && m.Kind == "accepted") || (s.base.Operation == "profile.start" && m.Kind == "result") {
			runtime, err := runtimeResult(s.base, m)
			if err != nil {
				return err
			}
			s.mu.Lock()
			if s.runtime.ID != "" && s.runtime != runtime {
				s.mu.Unlock()
				return ErrDenied
			}
			s.runtime = runtime
			s.mu.Unlock()
		}
		return nil
	}
	s.mu.Lock()
	runtime := s.runtime
	s.mu.Unlock()
	r, err := continuation(s.base, runtime, m)
	if err != nil {
		return err
	}
	return s.ensure(ctx, r)
}

// An independent watchdog enforces deadlines even when a trusted checker is
// slow or ignores cancellation. Refresh capacity is one call per active lease;
// late responses cannot replace a newer decision or resurrect a closed stream.
func (s *checkedStream) notify() {
	select {
	case s.changed <- struct{}{}:
	default:
	}
}
func (s *checkedStream) watch() {
	for {
		s.mu.Lock()
		now := time.Now()
		next := now.Add(StreamLeaseLimit)
		expired := false
		for key, value := range s.leases {
			if !now.Before(value.decision.ValidUntil) {
				expired = true
				break
			}
			if !value.refreshing && !now.Before(value.refreshAt) {
				value.refreshing = true
				go s.refresh(key, value)
			}
			deadline := value.decision.ValidUntil
			if !value.refreshing && value.refreshAt.Before(deadline) {
				deadline = value.refreshAt
			}
			if deadline.Before(next) {
				next = deadline
			}
		}
		s.mu.Unlock()
		if expired {
			s.flow.Cancel(ErrDenied)
			s.cancel()
			return
		}
		timer := time.NewTimer(time.Until(next))
		select {
		case <-s.ctx.Done():
			timer.Stop()
			return
		case <-s.changed:
			timer.Stop()
		case <-timer.C:
		}
	}
}
func (s *checkedStream) refresh(key Request, previous *lease) {
	ctx, cancel := context.WithDeadline(s.ctx, previous.decision.ValidUntil)
	defer cancel()
	d, err := s.grant.checkAction(ctx, key)
	if err != nil {
		s.flow.Cancel(ErrDenied)
		s.cancel()
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.ctx.Err() != nil || !time.Now().Before(previous.decision.ValidUntil) {
		return
	}
	if s.leases[key] == previous {
		renewed := newLease(d)
		// A fixed credential/policy expiry can validate again without extending
		// the lease. Wait for that deadline instead of repeatedly halving the
		// remaining interval into a burst of authoritative calls.
		if !d.ValidUntil.After(previous.decision.ValidUntil) {
			renewed.refreshAt = d.ValidUntil
		}
		s.leases[key] = renewed
		s.notify()
	}
}
func (s *checkedStream) Closed(error) { s.cancel() }
