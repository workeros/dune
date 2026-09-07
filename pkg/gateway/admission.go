package gateway

import (
	"fmt"
	"sync"
	"time"

	"github.com/aiomni/dune/internal/wire"
)

// AdmissionLease bounds a trusted application's right to admit protocol input.
// It carries no user, configuration or database identity. Callers translate a
// committed external lease to a conservative local monotonic deadline measured
// from before that external call. A lease cannot be revived after expiry.
// The application owns Close; multiple connections may share the same lease.
type AdmissionLease struct {
	mu     sync.Mutex
	until  time.Time
	done   chan struct{}
	timer  *time.Timer
	closed bool
}

func NewAdmissionLease(until time.Time) (*AdmissionLease, error) {
	remaining := time.Until(until)
	if remaining <= 0 || remaining > wire.InputLeaseDuration {
		return nil, fmt.Errorf("admission deadline must be within fifteen seconds")
	}
	lease := &AdmissionLease{until: until, done: make(chan struct{})}
	lease.timer = time.AfterFunc(remaining, lease.expire)
	return lease, nil
}

// Remaining is checked at input admission, independently of timer scheduling.
// Nil means the application did not impose an additional protocol lease.
func (l *AdmissionLease) Remaining() time.Duration {
	if l == nil {
		return wire.InputLeaseDuration
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	return max(0, time.Until(l.until))
}
func (l *AdmissionLease) Done() <-chan struct{} {
	if l == nil {
		return nil
	}
	return l.done
}
func (l *AdmissionLease) Renew(until time.Time) error {
	l.mu.Lock()
	defer l.mu.Unlock()
	remaining := time.Until(until)
	if l.closed || time.Until(l.until) <= 0 || remaining <= 0 || remaining > wire.InputLeaseDuration {
		return fmt.Errorf("admission lease expired or invalid")
	}
	if until.After(l.until) {
		l.until = until
		l.timer.Reset(remaining)
	}
	return nil
}
func (l *AdmissionLease) expire() {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.closed {
		return
	}
	if remaining := time.Until(l.until); remaining > 0 {
		l.timer.Reset(remaining)
		return
	}
	l.closed = true
	close(l.done)
}
func (l *AdmissionLease) Close() {
	if l == nil {
		return
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	if !l.closed {
		l.until = time.Time{}
		l.closed = true
		l.timer.Stop()
		close(l.done)
	}
}
