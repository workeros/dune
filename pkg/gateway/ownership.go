package gateway

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"strings"
	"sync"
	"time"
	"unicode"

	"github.com/aiomni/dune/internal/wire"
	"github.com/aiomni/dune/pkg/api"
	"github.com/aiomni/dune/pkg/observe"
)

const directoryTimeout = time.Second
const ownerLeaseLimit = 15 * time.Second

// NewWithDirectory coordinates this instance's reverse connections through a
// shared directory. Each Gateway receives a fresh process identity. Address must
// reach this instance directly; the application validates its transport-specific
// address and supplies a trusted directory and
// The caller owns the directory and its storage lifecycle.
// This constructor does not provide a peer transport for remote SDK routing.
func NewWithDirectory(directory Directory, address string) (*Gateway, error) {
	if directory == nil || address == "" || len(address) > 2048 || strings.ContainsFunc(address, unicode.IsControl) {
		return nil, fmt.Errorf("directory and direct instance address required")
	}
	g := New()
	g.directory, g.ownerAddress, g.bootID = directory, address, wire.ID()
	return g, nil
}

// OwnerAddress is the immutable directory advertisement, empty in local mode.
func (g *Gateway) OwnerAddress() string { return g.ownerAddress }

type ownership struct {
	directory Directory
	route     Route
	mu        sync.Mutex
	until     time.Time
	changed   chan struct{}
	emit      func(observe.Event)
}

func (g *Gateway) acquireOwner(ctx context.Context, binding api.Binding) (*ownership, error) {
	lookup, cancel := context.WithTimeout(ctx, directoryTimeout)
	previous, err := g.directory.Resolve(lookup, binding.Target)
	lookupErr := lookup.Err()
	cancel()
	if lookupErr != nil {
		return nil, lookupErr
	}
	if err != nil && !errors.Is(err, ErrRouteNotFound) {
		return nil, err
	}
	var expected uint64
	if err == nil {
		if previous.Target != binding.Target {
			return nil, ErrRouteStale
		}
		expected = previous.Epoch
	}
	claim := RouteClaim{Target: binding.Target, OwnerBootID: g.bootID, OwnerAddress: g.ownerAddress, Binding: binding}
	started := time.Now()
	bounded, cancel := context.WithTimeout(ctx, directoryTimeout)
	defer cancel()
	lease, err := g.directory.Acquire(bounded, claim, expected)
	if err != nil {
		return nil, err
	}
	if !reflect.DeepEqual(lease.RouteClaim, claim) || lease.Epoch == 0 || lease.Epoch <= expected || lease.Published || lease.ValidFor <= 0 || lease.ValidFor > ownerLeaseLimit {
		return nil, ErrRouteStale
	}
	owner := &ownership{directory: g.directory, route: lease.Route, until: started.Add(lease.ValidFor), changed: make(chan struct{}, 1), emit: g.emit}
	if bounded.Err() != nil || owner.remaining() <= 0 {
		return nil, ErrRouteStale
	}
	return owner, nil
}

func (o *ownership) remaining() time.Duration {
	o.mu.Lock()
	defer o.mu.Unlock()
	return time.Until(o.until)
}

func (o *ownership) publish(ctx context.Context) error {
	remaining := o.remaining()
	if remaining <= 0 {
		return ErrRouteStale
	}
	bounded, cancel := context.WithTimeout(ctx, min(remaining, directoryTimeout))
	defer cancel()
	if err := o.directory.Publish(bounded, o.route); err != nil {
		return err
	}
	if bounded.Err() != nil || o.remaining() <= 0 {
		return ErrRouteStale
	}
	return nil
}

func (o *ownership) renew(ctx context.Context) (result error) {
	started := time.Now()
	defer func() {
		if result != nil && ctx.Err() == nil && o.emit != nil {
			o.emit(observe.Event{
				Name: observe.GatewayRouteRenewal, Outcome: "failed", DurationMicros: elapsedMicros(started),
				Target: o.route.Target, OwnerID: o.route.OwnerBootID, Incarnation: o.route.Binding.Incarnation,
				Generation: o.route.Binding.Generation, Epoch: o.route.Epoch,
			})
		}
	}()
	remaining := o.remaining()
	if remaining <= 0 {
		return ErrRouteStale
	}
	bounded, cancel := context.WithTimeout(ctx, min(remaining, directoryTimeout))
	defer cancel()
	lease, err := o.directory.Renew(bounded, o.route)
	if err != nil {
		return err
	}
	if bounded.Err() != nil || lease.Epoch != o.route.Epoch || !reflect.DeepEqual(lease.RouteClaim, o.route.RouteClaim) || lease.ValidFor <= 0 || lease.ValidFor > ownerLeaseLimit {
		return ErrRouteStale
	}
	o.mu.Lock()
	defer o.mu.Unlock()
	until := started.Add(lease.ValidFor)
	if !time.Now().Before(o.until) || !time.Now().Before(until) {
		return ErrRouteStale
	}
	o.until = until
	select {
	case o.changed <- struct{}{}:
	default:
	}
	return nil
}

func (o *ownership) watch(ctx context.Context, close func()) {
	for {
		remaining := o.remaining()
		if remaining <= 0 {
			close()
			return
		}
		timer := time.NewTimer(remaining)
		select {
		case <-ctx.Done():
			timer.Stop()
			return
		case <-o.changed:
			timer.Stop()
		case <-timer.C:
		}
	}
}

func (o *ownership) release(ctx context.Context) {
	bounded, cancel := context.WithTimeout(context.WithoutCancel(ctx), directoryTimeout)
	defer cancel()
	// Release compares the original owner/epoch, so a late cleanup cannot erase
	// a replacement. Unknown results are left for expiry, never retried here.
	_ = o.directory.Release(bounded, o.route)
}
