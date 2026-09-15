// Package fake provides a deterministic in-memory Managed Provider for local
// development and contract tests. Its resource facts are process-local, so it
// must not be used as a production Provider.
package fake

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"sync"
	"time"

	"github.com/aiomni/dune/pkg/fabric"
)

type Config struct {
	TTL time.Duration
	// Now is optional and primarily useful for deterministic contract tests.
	Now func() time.Time
}

type resource struct {
	createActionID string
	expires        time.Time
	state          fabric.ResourceState
	gone           bool
}

type action struct {
	kind, ref, digest string
}

type Provider struct {
	mu        sync.Mutex
	ttl       time.Duration
	now       func() time.Time
	resources map[string]*resource
	actions   map[string]action
}

func New(config Config) *Provider {
	if config.TTL <= 0 {
		config.TTL = 24 * time.Hour
	}
	if config.Now == nil {
		config.Now = time.Now
	}
	return &Provider{ttl: config.TTL, now: config.Now, resources: map[string]*resource{}, actions: map[string]action{}}
}

func (*Provider) Availability(ctx context.Context) (fabric.Availability, error) {
	if err := ctx.Err(); err != nil {
		return fabric.Availability{}, err
	}
	return fabric.Availability{Available: true}, nil
}

func (p *Provider) Create(ctx context.Context, call fabric.CreateCall) (fabric.Observation, error) {
	if err := ctx.Err(); err != nil {
		return fabric.Observation{}, err
	}
	if call.Action.ID == "" || call.Action.RunnerID == "" {
		return fabric.Observation{}, fmt.Errorf("fake create requires action and runner identities")
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	digest := call.Action.RunnerID + "\x00" + call.Action.RequestDigest
	if existing, ok := p.actions[call.Action.ID]; ok {
		if existing.kind != "create" || existing.digest != digest {
			return fabric.Observation{}, fmt.Errorf("fake action ID conflicts with an earlier request")
		}
		return p.observation(existing.ref), nil
	}
	ref := "fake:" + call.Action.RunnerID
	if existing := p.resources[ref]; existing != nil {
		return fabric.Observation{}, fmt.Errorf("fake Runner already has a resource")
	}
	p.resources[ref] = &resource{createActionID: call.Action.ID, expires: p.now().UTC().Add(p.ttl), state: fabric.ResourceReady}
	p.actions[call.Action.ID] = action{kind: "create", ref: ref, digest: digest}
	return p.observation(ref), nil
}

func (p *Provider) ReconcileCreate(ctx context.Context, call fabric.ReconcileCall) (fabric.Observation, error) {
	if err := ctx.Err(); err != nil {
		return fabric.Observation{}, err
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	existing, ok := p.actions[call.Action.ID]
	if !ok || existing.kind != "create" || existing.digest != createDigest(call.Action) || (call.KnownResourceRef != "" && call.KnownResourceRef != existing.ref) {
		return fabric.Observation{Outcome: fabric.OutcomeUnknown}, nil
	}
	return p.observation(existing.ref), nil
}

func (p *Provider) Bootstrap(ctx context.Context, call fabric.BootstrapCall) (fabric.Observation, error) {
	if err := ctx.Err(); err != nil {
		return fabric.Observation{}, err
	}
	if call.Action.ID == "" || call.Action.ResourceRef == "" || call.EnrollmentToken == "" {
		return fabric.Observation{}, fmt.Errorf("fake bootstrap requires action, resource and enrollment identities")
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	digest := bootstrapDigest(call)
	if existing, ok := p.actions[call.Action.ID]; ok {
		if existing.kind != "bootstrap" || existing.ref != call.Action.ResourceRef || existing.digest != digest {
			return fabric.Observation{}, fmt.Errorf("fake action ID conflicts with an earlier request")
		}
		return p.observation(existing.ref), nil
	}
	r := p.resources[call.Action.ResourceRef]
	if r == nil || r.gone {
		return fabric.Observation{Outcome: fabric.OutcomeUnknown}, nil
	}
	p.actions[call.Action.ID] = action{kind: "bootstrap", ref: call.Action.ResourceRef, digest: digest}
	return p.observation(call.Action.ResourceRef), nil
}

func (p *Provider) ReconcileBootstrap(ctx context.Context, call fabric.BootstrapReconcileCall) (fabric.Observation, error) {
	if err := ctx.Err(); err != nil {
		return fabric.Observation{}, err
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	existing, ok := p.actions[call.Action.ID]
	if !ok || existing.kind != "bootstrap" || existing.ref != call.Action.ResourceRef {
		return fabric.Observation{Outcome: fabric.OutcomeUnknown}, nil
	}
	return p.observation(existing.ref), nil
}

func (p *Provider) Inspect(ctx context.Context, call fabric.InspectCall) (fabric.Inspection, error) {
	if err := ctx.Err(); err != nil {
		return fabric.Inspection{}, err
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	r := p.resources[call.ResourceRef]
	if r == nil {
		return fabric.Inspection{Status: fabric.InspectionUnknown}, nil
	}
	if r.gone {
		return fabric.Inspection{Status: fabric.InspectionConfirmed, ResourceRef: call.ResourceRef, Gone: true}, nil
	}
	return fabric.Inspection{Status: fabric.InspectionConfirmed, ResourceRef: call.ResourceRef, ExpiresAt: r.expires, State: r.state, Gone: r.gone, Capabilities: capabilities()}, nil
}

func (p *Provider) Renew(ctx context.Context, call fabric.RenewCall) (fabric.Observation, error) {
	if err := ctx.Err(); err != nil {
		return fabric.Observation{}, err
	}
	if call.Action.ID == "" || call.Action.ResourceRef == "" || call.Action.RenewUntil.IsZero() {
		return fabric.Observation{}, fmt.Errorf("fake renew requires action, resource and target expiry")
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	if existing, ok := p.actions[call.Action.ID]; ok {
		if existing.kind != "renew" || existing.ref != call.Action.ResourceRef || existing.digest != call.Action.RenewUntil.UTC().Format(time.RFC3339Nano) {
			return fabric.Observation{}, fmt.Errorf("fake action ID conflicts with an earlier request")
		}
		return p.observation(existing.ref), nil
	}
	r := p.resources[call.Action.ResourceRef]
	if r == nil || r.gone {
		return fabric.Observation{Outcome: fabric.OutcomeUnknown}, nil
	}
	if call.Action.RenewUntil.After(r.expires) {
		r.expires = call.Action.RenewUntil
	}
	p.actions[call.Action.ID] = action{kind: "renew", ref: call.Action.ResourceRef, digest: call.Action.RenewUntil.UTC().Format(time.RFC3339Nano)}
	return p.observation(call.Action.ResourceRef), nil
}

func (p *Provider) ReconcileRenew(ctx context.Context, call fabric.RenewReconcileCall) (fabric.Observation, error) {
	if err := ctx.Err(); err != nil {
		return fabric.Observation{}, err
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	existing, ok := p.actions[call.Action.ID]
	if !ok || existing.kind != "renew" || existing.ref != call.Action.ResourceRef || existing.digest != renewDigest(call.Action) {
		return fabric.Observation{Outcome: fabric.OutcomeUnknown}, nil
	}
	return p.observation(existing.ref), nil
}

func (p *Provider) Destroy(ctx context.Context, call fabric.DestroyCall) (fabric.Observation, error) {
	if err := ctx.Err(); err != nil {
		return fabric.Observation{}, err
	}
	if call.Action.ID == "" || call.Action.ResourceRef == "" {
		return fabric.Observation{}, fmt.Errorf("fake destroy requires action and resource identities")
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	if existing, ok := p.actions[call.Action.ID]; ok {
		if existing.kind != "destroy" || existing.ref != call.Action.ResourceRef {
			return fabric.Observation{}, fmt.Errorf("fake action ID conflicts with an earlier request")
		}
		return p.observation(existing.ref), nil
	}
	r := p.resources[call.Action.ResourceRef]
	if r == nil {
		return fabric.Observation{Outcome: fabric.OutcomeUnknown}, nil
	}
	r.gone = true
	r.state = fabric.ResourceUnknown
	p.actions[call.Action.ID] = action{kind: "destroy", ref: call.Action.ResourceRef}
	return p.observation(call.Action.ResourceRef), nil
}

func (p *Provider) ReconcileDestroy(ctx context.Context, call fabric.DestroyReconcileCall) (fabric.Observation, error) {
	if err := ctx.Err(); err != nil {
		return fabric.Observation{}, err
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	existing, ok := p.actions[call.Action.ID]
	if !ok || existing.kind != "destroy" || existing.ref != call.Action.ResourceRef {
		return fabric.Observation{Outcome: fabric.OutcomeUnknown}, nil
	}
	return p.observation(existing.ref), nil
}

func (p *Provider) VerifyCandidate(ctx context.Context, call fabric.CandidateCall) (fabric.Observation, error) {
	if err := ctx.Err(); err != nil {
		return fabric.Observation{}, err
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	r := p.resources[call.CandidateResourceRef]
	if r == nil || r.gone || r.createActionID != call.Action.ID {
		return fabric.Observation{Outcome: fabric.OutcomeUnknown}, nil
	}
	existing, ok := p.actions[call.Action.ID]
	if !ok || existing.kind != "create" || existing.ref != call.CandidateResourceRef || existing.digest != createDigest(call.Action) {
		return fabric.Observation{Outcome: fabric.OutcomeUnknown}, nil
	}
	return p.observation(call.CandidateResourceRef), nil
}

func (p *Provider) Pause(ctx context.Context, call fabric.PauseResumeCall) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	return p.setState(call.ResourceRef, fabric.ResourcePaused)
}

func (p *Provider) Resume(ctx context.Context, call fabric.PauseResumeCall) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	return p.setState(call.ResourceRef, fabric.ResourceReady)
}

func (p *Provider) setState(ref string, state fabric.ResourceState) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	r := p.resources[ref]
	if r == nil || r.gone {
		return fmt.Errorf("fake resource is unavailable")
	}
	r.state = state
	return nil
}

func (p *Provider) observation(ref string) fabric.Observation {
	r := p.resources[ref]
	if r == nil {
		return fabric.Observation{Outcome: fabric.OutcomeUnknown}
	}
	if r.gone {
		return fabric.Observation{Outcome: fabric.OutcomeSucceeded, ResourceRef: ref, Gone: true}
	}
	return fabric.Observation{Outcome: fabric.OutcomeSucceeded, ResourceRef: ref, ExpiresAt: r.expires, State: r.state, Capabilities: capabilities()}
}

func capabilities() *fabric.ResourceCapabilities {
	return &fabric.ResourceCapabilities{PauseResume: true}
}

func createDigest(action fabric.Action) string {
	return action.RunnerID + "\x00" + action.RequestDigest
}

func renewDigest(action fabric.Action) string {
	return action.RenewUntil.UTC().Format(time.RFC3339Nano)
}

func bootstrapDigest(call fabric.BootstrapCall) string {
	sum := sha256.Sum256([]byte(call.EnrollmentToken))
	return call.Action.ResourceRef + "\x00" + hex.EncodeToString(sum[:]) + "\x00" +
		call.EnrollmentExpiresAt.UTC().Format(time.RFC3339Nano) + "\x00" + call.Endpoint + "\x00" +
		call.GatewayURL + "\x00" + call.Version
}

var _ interface {
	fabric.AvailabilityProvider
	fabric.CreateProvider
	fabric.BootstrapProvider
	fabric.InspectProvider
	fabric.RenewProvider
	fabric.DestroyProvider
	fabric.CandidateProvider
	fabric.PauseResumeProvider
} = (*Provider)(nil)
