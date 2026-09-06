package wire

import (
	"context"
	"encoding/hex"
	"fmt"
	"sync"
	"time"
)

const InputLeaseDuration = 15 * time.Second
const InputLeaseInterval = 5 * time.Second

// InputWindow keeps overlapping grants for one reverse connection. A grant is
// measured from the local challenge start, never from a delayed acknowledgement.
// Each business message names its original grant; a new grant cannot revive it.
// The requester creates fresh random IDs. The responder uses the same window to
// bound forwarding, confirming its offered grant only after the requester acks.
type InputWindow struct {
	mu      sync.Mutex
	now     func() time.Time
	pending string
	issued  time.Time
	current string
	grants  map[string]time.Time
	changed chan struct{}
}

func NewInputWindow() *InputWindow {
	return &InputWindow{now: time.Now, grants: make(map[string]time.Time), changed: make(chan struct{}, 1)}
}

func (w *InputWindow) Begin(id string) error {
	w.mu.Lock()
	defer w.mu.Unlock()
	now := w.now()
	decoded, err := hex.DecodeString(id)
	if err != nil || len(decoded) != 16 || hex.EncodeToString(decoded) != id || w.pending != "" {
		return fmt.Errorf("invalid input lease challenge")
	}
	if w.current != "" && (!now.Before(w.grants[w.current]) || now.Sub(w.issued) < InputLeaseInterval) {
		return fmt.Errorf("input lease expired or challenge too frequent")
	}
	if _, exists := w.grants[id]; exists {
		return fmt.Errorf("input lease challenge reused")
	}
	w.pending, w.issued = id, now
	return nil
}

func (w *InputWindow) Confirm(id string, duration time.Duration) error {
	w.mu.Lock()
	defer w.mu.Unlock()
	now := w.now()
	if id == "" || id != w.pending || duration <= 0 || duration > InputLeaseDuration {
		return fmt.Errorf("invalid input lease grant")
	}
	until := w.issued.Add(duration)
	if !now.Before(until) || (w.current != "" && !now.Before(w.grants[w.current])) {
		return fmt.Errorf("input lease expired")
	}
	for previous, expiry := range w.grants {
		if !now.Before(expiry) {
			delete(w.grants, previous)
		}
	}
	w.grants[id] = until
	w.current, w.pending = id, ""
	select {
	case w.changed <- struct{}{}:
	default:
	}
	return nil
}

func (w *InputWindow) Valid(id string) bool {
	w.mu.Lock()
	defer w.mu.Unlock()
	return id != "" && w.now().Before(w.grants[id])
}

func (w *InputWindow) Current() (string, time.Duration) {
	w.mu.Lock()
	defer w.mu.Unlock()
	remaining := w.grants[w.current].Sub(w.now())
	if remaining <= 0 {
		return "", 0
	}
	return w.current, remaining
}

// Watch closes even idle tunnels at the current deadline. Admission and sending
// separately check their grants, so delayed timer scheduling never permits I/O.
// Start only after the initial grant has been confirmed.
func (w *InputWindow) Watch(ctx context.Context, expired func()) {
	for {
		_, remaining := w.Current()
		if remaining <= 0 {
			expired()
			return
		}
		timer := time.NewTimer(remaining)
		select {
		case <-ctx.Done():
			timer.Stop()
			return
		case <-w.changed:
			timer.Stop()
		case <-timer.C:
		}
	}
}
