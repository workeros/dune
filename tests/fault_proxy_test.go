package tests

import (
	"context"
	"io"
	"net"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// faultProxy preserves the real byte protocols and can hold traffic in both
// directions without immediately reporting EOF. Healing releases queued bytes;
// callers must not mistake a timeout for proof that a write never arrived.
type faultProxy struct {
	listener net.Listener
	ctx      context.Context
	wg       sync.WaitGroup
	mu       sync.Mutex
	target   string
	gate     chan struct{}
	blocked  bool
	active   map[net.Conn]context.CancelFunc
	accepted atomic.Int64
	stalled  atomic.Int64
}

func newFaultProxy(t *testing.T, parent context.Context, target string) *faultProxy {
	t.Helper()
	host := "127.0.0.1"
	if targetHost, _, err := net.SplitHostPort(target); err == nil && net.ParseIP(targetHost).IsLoopback() {
		host = targetHost
	}
	listener, err := net.Listen("tcp", net.JoinHostPort(host, "0"))
	must(t, err)
	ctx, cancel := context.WithCancel(parent)
	p := &faultProxy{listener: listener, ctx: ctx, target: target, active: make(map[net.Conn]context.CancelFunc), gate: make(chan struct{})}
	close(p.gate)
	p.wg.Go(func() {
		for {
			conn, err := listener.Accept()
			if err != nil {
				return
			}
			p.accepted.Add(1)
			p.wg.Go(func() { p.forward(conn) })
		}
	})
	t.Cleanup(func() {
		cancel()
		listener.Close()
		p.wg.Wait()
	})
	return p
}

func (p *faultProxy) address() string { return p.listener.Addr().String() }

func (p *faultProxy) block(blocked bool) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.blocked == blocked {
		return
	}
	p.blocked = blocked
	if blocked {
		p.gate = make(chan struct{})
	} else {
		close(p.gate)
	}
}

// route models a changed machine ingress destination and drops old TCP sockets.
// It does not restart fabricd or move an existing socket between Gateways.
func (p *faultProxy) route(target string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.target = target
	for _, cancel := range p.active {
		cancel()
	}
}

func (p *faultProxy) forward(client net.Conn) {
	defer client.Close()
	ctx, cancel := context.WithCancel(p.ctx)
	defer cancel()
	p.mu.Lock()
	target := p.target
	p.active[client] = cancel
	p.mu.Unlock()
	defer func() { p.mu.Lock(); delete(p.active, client); p.mu.Unlock() }()
	upstream, err := (&net.Dialer{Timeout: time.Second}).DialContext(ctx, "tcp", target)
	if err != nil {
		return
	}
	defer upstream.Close()
	stop := context.AfterFunc(ctx, func() { client.Close(); upstream.Close() })
	defer stop()
	done := make(chan struct{}, 2)
	copy := func(dst, src net.Conn) {
		_, _ = io.Copy(dst, faultReader{Reader: src, proxy: p, ctx: ctx})
		done <- struct{}{}
	}
	go copy(client, upstream)
	go copy(upstream, client)
	<-done
	cancel()
	<-done
}

type faultReader struct {
	io.Reader
	proxy *faultProxy
	ctx   context.Context
}

func (r faultReader) Read(b []byte) (int, error) {
	n, err := r.Reader.Read(b)
	if n == 0 {
		return n, err
	}
	for {
		if err := r.ctx.Err(); err != nil {
			return 0, err
		}
		r.proxy.mu.Lock()
		blocked, gate := r.proxy.blocked, r.proxy.gate
		r.proxy.mu.Unlock()
		if !blocked {
			return n, err
		}
		r.proxy.stalled.Add(1)
		select {
		case <-r.ctx.Done():
		case <-gate:
		}
		r.proxy.stalled.Add(-1)
	}
}
