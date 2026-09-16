package channel

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
)

// Registry resolves providers by their own Kind without a platform enum.
// Registration is intended during host assembly; lookups may run concurrently.
type Registry struct {
	mu        sync.RWMutex
	providers map[string]Provider
}

func (r *Registry) Register(provider Provider) error {
	if provider == nil {
		return errors.New("IM provider is required")
	}
	kind := provider.Kind()
	if kind == "" || strings.TrimSpace(kind) != kind || strings.ContainsAny(kind, " \t\r\n/") {
		return fmt.Errorf("invalid IM provider kind %q", kind)
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.providers == nil {
		r.providers = make(map[string]Provider)
	}
	if _, exists := r.providers[kind]; exists {
		return fmt.Errorf("IM provider kind %q is already registered", kind)
	}
	r.providers[kind] = provider
	return nil
}

func (r *Registry) Provider(kind string) (Provider, bool) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	provider, ok := r.providers[kind]
	return provider, ok
}

func (r *Registry) Open(ctx context.Context, binding BotBinding, credentials []byte, sink EventSink) (Channel, error) {
	provider, found := r.Provider(binding.Provider)
	if !found {
		return nil, fmt.Errorf("IM provider %q is not registered", binding.Provider)
	}
	if err := provider.Validate(binding.Config, credentials); err != nil {
		return nil, fmt.Errorf("validate IM provider %q: %w", binding.Provider, err)
	}
	return provider.Open(ctx, binding, credentials, sink)
}
