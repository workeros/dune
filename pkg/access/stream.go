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

func (c *connection) openChecked(ctx context.Context, m *pb.Message, flow *gateway.Stream) (gateway.StreamHandler, error) {
	r, err := describe(c.grant.Policy.Scope, m)
	if err != nil {
		return nil, err
	}
	d, err := c.grant.Policy.check(ctx, r)
	if err != nil {
		return nil, err
	}
	if err = flow.Forward(); err != nil {
		return nil, err
	}
	life, cancel := context.WithCancel(c.ctx)
	s := &checkedStream{grant: c.grant, base: r, flow: flow, ctx: life, cancel: cancel, leases: map[Request]*lease{r: newLease(d)}, changed: make(chan struct{}, 1)}
	go s.watch()
	return s, nil
}

// ensure reuses only this stream's exact action lease. New input actions obtain
// a distinct decision; terminal data never enters the cache or checker.
func (s *checkedStream) ensure(ctx context.Context, r Request) error {
	s.mu.Lock()
	cached := s.leases[r]
	s.mu.Unlock()
	if cached != nil {
		if !time.Now().Before(cached.decision.ValidUntil) {
			return ErrDenied
		}
		return nil
	}
	d, err := s.grant.Policy.check(ctx, r)
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
	if err := s.grant.check(); err != nil {
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
		next := now.Add(MaxLease)
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
	d, err := s.grant.Policy.check(ctx, key)
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
		s.leases[key] = newLease(d)
		s.notify()
	}
}
func (s *checkedStream) Closed(error) { s.cancel() }
