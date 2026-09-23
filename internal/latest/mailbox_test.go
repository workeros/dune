package latest

import (
	"context"
	"errors"
	"testing"
)

func TestMailboxCoalescesAndFailsBeforeBufferedValues(t *testing.T) {
	m := New[string, int](2)
	for i := range 10000 {
		m.Offer("same", i)
	}
	if value, err := m.Next(t.Context()); value != 9999 || err != nil {
		t.Fatal("final value lost without subsequent update", value, err)
	}
	m.Offer("one", 1)
	m.Offer("two", 2)
	m.Offer("three", 3)
	if _, err := m.Next(t.Context()); !errors.Is(err, ErrOverflow) {
		t.Fatal("overflow was not terminal", err)
	}
	m = New[string, int](2)
	m.Offer("buffered", 1)
	m.Fail(context.Canceled)
	m.Offer("late", 2)
	if _, err := m.Next(t.Context()); !errors.Is(err, context.Canceled) {
		t.Fatal("failed subscription returned buffered or late data", err)
	}
}
