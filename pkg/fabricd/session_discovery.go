package fabricd

import (
	"context"
	"encoding/json"
	"math/rand/v2"
	"path/filepath"
	"time"

	"github.com/aiomni/dune/internal/tmux"
	"github.com/aiomni/dune/pkg/api"
)

// Scanning never advances launch or cleanup. A host that finishes registration
// after connector replacement is picked up by its original identity.
func (d *Engine) discoverSessions() error {
	d.discoveryScanMu.Lock()
	defer d.discoveryScanMu.Unlock()
	ctx, cancel := context.WithTimeout(d.ctx, time.Second)
	defer cancel()
	return d.scanSessionRegistrations(ctx)
}

func (d *Engine) refreshSessionRegistrations(ctx context.Context) {
	if d.acpTmux == nil || !d.discoveryScanMu.TryLock() {
		return
	}
	defer d.discoveryScanMu.Unlock()
	if time.Now().Before(d.discoveryNextScan) {
		return
	}
	scanCtx, cancel := context.WithTimeout(ctx, time.Second)
	defer cancel()
	if err := d.scanSessionRegistrations(scanCtx); err != nil {
		d.mu.Lock()
		// Preserve prior targets when the registry itself is temporarily unreadable.
		issues := d.discoveryIssues[:0]
		for _, issue := range d.discoveryIssues {
			if issue.Code != "REGISTRY_UNAVAILABLE" {
				issues = append(issues, issue)
			}
		}
		d.discoveryIssues = append(issues, api.RuntimeDiscoveryIssue{Code: "REGISTRY_UNAVAILABLE"})
		d.mu.Unlock()
	}
	d.discoveryNextScan = time.Now().Add(time.Second)
}

func (d *Engine) scanSessionRegistrations(ctx context.Context) error {
	root := filepath.Join(d.stateDir, "acp", "runtimes")
	if err := tmux.PrivateDir(root); err != nil {
		return err
	}
	hosts, err := d.registry.Hosts(ctx)
	if err != nil {
		return err
	}
	issues := append([]api.RuntimeDiscoveryIssue{}, hosts.Issues...)
	registrations := make([]sessionRegistration, 0, len(hosts.Hosts))
	for _, host := range hosts.Hosts {
		var reg sessionRegistration
		code := ""
		if json.Unmarshal(host.Registration, &reg) != nil || reg.Installation != installationID(d.stateDir) || reg.Target != host.Target || reg.Instance != host.Instance {
			code = "REGISTRATION_INVALID"
		} else if _, err := sessionSocketPath(reg); err != nil {
			code = "REGISTRATION_INVALID"
			if reg.Version != sessionProtocol {
				code = "SESSION_PROTOCOL_UNSUPPORTED"
			}
		}
		if code != "" {
			runtime := host.Runtime
			runtime.Availability = "unavailable"
			issues = append(issues, api.RuntimeDiscoveryIssue{Runtime: &runtime, Code: code})
			continue
		}
		reg.Runtime = host.Runtime
		registrations = append(registrations, reg)
	}
	issues = append(issues, d.sessionArtifactIssues(ctx, hosts)...)
	d.mu.Lock()
	defer d.mu.Unlock()
	for _, reg := range registrations {
		if d.launching[reg.Runtime.ID] != "" {
			observed := reg.Runtime
			issues = append(issues, api.RuntimeDiscoveryIssue{Runtime: &observed, Code: "HOST_REGISTRATION_PENDING"})
			continue
		}
		if existing := d.runtimes[reg.Runtime.ID]; existing != nil {
			// A repaired record can reveal an original host; it cannot replace an
			// already-known identity or steal an in-progress launch's connection.
			if existing.inc != reg.Runtime.Incarnation || existing.host == nil || existing.host.registration.Instance != reg.Instance || existing.host.registration.Target != reg.Target || existing.host.registration.Program != reg.Program || existing.host.registration.Version != reg.Version {
				issues = append(issues, api.RuntimeDiscoveryIssue{Runtime: &reg.Runtime, Code: "REGISTRATION_CONFLICT"})
			}
			continue
		}
		directory := filepath.Join(root, reg.Runtime.ID)
		d.runtimes[reg.Runtime.ID] = &runtime{id: reg.Runtime.ID, inc: reg.Runtime.Incarnation, adapter: "acp", host: &sessionProxy{registration: reg, directory: directory, term: d.sessionTerm, connector: d.inc, registry: d.registry}}
	}
	d.discoveryIssues = issues
	return nil
}

// Restore existing IPC connections within a bounded startup window. Per-host
// backoff and the Engine-wide probe pool also apply to concurrent list readers.
// Subsequent ordinary reads can retry a still-unavailable original endpoint.
func (d *Engine) restoreSessionConnections() {
	defer d.active.Done()
	ctx, cancel := context.WithTimeout(d.ctx, 30*time.Second)
	defer cancel()
	for {
		page := d.list(ctx)
		pending := false
		for _, runtime := range page.Items {
			if runtime.Availability == "unavailable" {
				pending = true
				break
			}
		}
		for _, issue := range page.Issues {
			if issue.Code == "HOST_REGISTRATION_PENDING" || issue.Code == "REGISTRY_UNAVAILABLE" {
				pending = true
			}
		}
		if !pending || ctx.Err() != nil {
			return
		}
		timer := time.NewTimer(250*time.Millisecond + time.Duration(rand.Int64N(int64(250*time.Millisecond))))
		select {
		case <-ctx.Done():
			timer.Stop()
			return
		case <-timer.C:
		}
	}
}
