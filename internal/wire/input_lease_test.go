package wire

import (
	"context"
	"testing"
	"time"
)

func TestInputLeaseDelayOverlapAndNoRevival(t *testing.T) {
	now := time.Now()
	window := NewInputWindow()
	window.now = func() time.Time { return now }
	first := ID()
	if err := window.Begin(first); err != nil {
		t.Fatal(err)
	}
	// Delivery takes four seconds. The receiver must not add those four seconds
	// to the fifteen seconds measured from its own original challenge.
	now = now.Add(4 * time.Second)
	if err := window.Confirm(first, InputLeaseDuration); err != nil {
		t.Fatal(err)
	}
	if _, remaining := window.Current(); remaining != 11*time.Second {
		t.Fatal("transport delay extended grant", remaining)
	}
	now = now.Add(time.Second)
	second := ID()
	if err := window.Begin(second); err != nil {
		t.Fatal(err)
	}
	if err := window.Confirm(second, InputLeaseDuration); err != nil {
		t.Fatal(err)
	}
	if !window.Valid(first) || !window.Valid(second) {
		t.Fatal("unexpired in-flight message lost its original grant")
	}
	now = now.Add(10 * time.Second)
	if window.Valid(first) || !window.Valid(second) {
		t.Fatal("new grant revived buffered input at the old deadline")
	}
	third := ID()
	if err := window.Begin(third); err != nil {
		t.Fatal(err)
	}
	// Both existing grants expire while the last acknowledgement is buffered.
	now = now.Add(5 * time.Second)
	if err := window.Confirm(third, InputLeaseDuration); err == nil {
		t.Fatal("late acknowledgement resurrected expired connection")
	}
	if id, _ := window.Current(); id != "" {
		t.Fatal("expired connection remained current")
	}
}

func TestInputLeaseValidationAndBoundedHistory(t *testing.T) {
	now := time.Now()
	window := NewInputWindow()
	window.now = func() time.Time { return now }
	for _, id := range []string{"", "not-random", "ABCDEF0123456789ABCDEF0123456789"} {
		if window.Begin(id) == nil {
			t.Fatal("accepted malformed challenge", id)
		}
	}
	first := ID()
	if err := window.Begin(first); err != nil {
		t.Fatal(err)
	}
	for _, duration := range []time.Duration{0, -1, InputLeaseDuration + 1} {
		if window.Confirm(first, duration) == nil {
			t.Fatal("accepted invalid duration", duration)
		}
	}
	if window.Confirm(ID(), InputLeaseDuration) == nil || window.Begin(ID()) == nil {
		t.Fatal("accepted unsolicited or concurrent grant")
	}
	if err := window.Confirm(first, InputLeaseDuration); err != nil {
		t.Fatal(err)
	}
	if window.Begin(ID()) == nil {
		t.Fatal("accepted unbounded challenge frequency")
	}
	now = now.Add(InputLeaseInterval)
	if window.Begin(first) == nil {
		t.Fatal("accepted reused grant")
	}
	for range 1000 {
		id := ID()
		if err := window.Begin(id); err != nil {
			t.Fatal(err)
		}
		if err := window.Confirm(id, InputLeaseDuration); err != nil {
			t.Fatal(err)
		}
		if len(window.grants) > 3 {
			t.Fatal("grant history grew without bound", len(window.grants))
		}
		now = now.Add(InputLeaseInterval)
	}
}

func TestInputLeaseRejectsDelayedInitialGrant(t *testing.T) {
	now := time.Now()
	window := NewInputWindow()
	window.now = func() time.Time { return now }
	id := ID()
	if err := window.Begin(id); err != nil {
		t.Fatal(err)
	}
	now = now.Add(InputLeaseDuration)
	if window.Confirm(id, InputLeaseDuration) == nil || window.Valid(id) {
		t.Fatal("accepted initial grant delayed past its original deadline")
	}
}

func TestInputLeaseWatchClosesIdleConnection(t *testing.T) {
	window := NewInputWindow()
	id := ID()
	if err := window.Begin(id); err != nil {
		t.Fatal(err)
	}
	if err := window.Confirm(id, 30*time.Millisecond); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	closed := make(chan struct{})
	go window.Watch(ctx, func() { close(closed) })
	select {
	case <-closed:
	case <-ctx.Done():
		t.Fatal("idle connection outlived its lease")
	}
	if window.Valid(id) {
		t.Fatal("expired connection still admits input")
	}
}
