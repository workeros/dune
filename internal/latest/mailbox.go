// Package latest provides bounded coalescing for observation streams. It keeps
// the most recently offered value per key, not a history of changes.
package latest

import (
	"context"
	"errors"
	"sync"
)

var ErrOverflow = errors.New("observation capacity exceeded; resynchronize")

type Mailbox[K comparable, V any] struct {
	mu     sync.Mutex
	values map[K]V
	limit  int
	err    error
	wake   chan struct{}
}

func New[K comparable, V any](limit int) *Mailbox[K, V] {
	return &Mailbox[K, V]{values: make(map[K]V), limit: limit, wake: make(chan struct{}, 1)}
}

func (m *Mailbox[K, V]) Offer(key K, value V) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.err != nil {
		return
	}
	if _, exists := m.values[key]; !exists && len(m.values) >= m.limit {
		m.err, m.values = ErrOverflow, nil
	} else {
		m.values[key] = value
	}
	m.notify()
}

// Fail discards even buffered values, so revocation/invalidations take priority
// over previously queued observations.
func (m *Mailbox[K, V]) Fail(err error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.err == nil {
		m.err, m.values = err, nil
	}
	m.notify()
}

func (m *Mailbox[K, V]) notify() {
	select {
	case m.wake <- struct{}{}:
	default:
	}
}

func (m *Mailbox[K, V]) Next(ctx context.Context) (V, error) {
	for {
		var zero V
		if err := ctx.Err(); err != nil {
			return zero, err
		}
		m.mu.Lock()
		if m.err != nil {
			err := m.err
			m.mu.Unlock()
			return zero, err
		}
		for key, value := range m.values {
			delete(m.values, key)
			m.mu.Unlock()
			return value, nil
		}
		m.mu.Unlock()
		select {
		case <-ctx.Done():
			return zero, ctx.Err()
		case <-m.wake:
		}
	}
}
