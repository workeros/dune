package host

import (
	"context"
	"testing"

	"github.com/aiomni/dune/pkg/fabric"
)

type exactProvider struct {
	creates int
	reads   int
	pauses  int
}

func (*exactProvider) Availability(context.Context) (fabric.Availability, error) {
	return fabric.Availability{Available: true}, nil
}
func (p *exactProvider) Create(context.Context, fabric.CreateCall) (fabric.Observation, error) {
	p.creates++
	return fabric.Observation{Outcome: fabric.OutcomeUnknown}, nil
}
func (*exactProvider) ReconcileCreate(context.Context, fabric.ReconcileCall) (fabric.Observation, error) {
	return fabric.Observation{Outcome: fabric.OutcomeUnknown}, nil
}
func (*exactProvider) Bootstrap(context.Context, fabric.BootstrapCall) (fabric.Observation, error) {
	return fabric.Observation{Outcome: fabric.OutcomeUnknown}, nil
}
func (*exactProvider) ReconcileBootstrap(context.Context, fabric.BootstrapReconcileCall) (fabric.Observation, error) {
	return fabric.Observation{Outcome: fabric.OutcomeUnknown}, nil
}
func (p *exactProvider) Inspect(context.Context, fabric.InspectCall) (fabric.Inspection, error) {
	p.reads++
	return fabric.Inspection{Status: fabric.InspectionUnknown}, nil
}
func (*exactProvider) Renew(context.Context, fabric.RenewCall) (fabric.Observation, error) {
	return fabric.Observation{Outcome: fabric.OutcomeUnknown}, nil
}
func (*exactProvider) ReconcileRenew(context.Context, fabric.RenewReconcileCall) (fabric.Observation, error) {
	return fabric.Observation{Outcome: fabric.OutcomeUnknown}, nil
}
func (*exactProvider) Destroy(context.Context, fabric.DestroyCall) (fabric.Observation, error) {
	return fabric.Observation{Outcome: fabric.OutcomeUnknown}, nil
}
func (*exactProvider) ReconcileDestroy(context.Context, fabric.DestroyReconcileCall) (fabric.Observation, error) {
	return fabric.Observation{Outcome: fabric.OutcomeUnknown}, nil
}
func (*exactProvider) VerifyCandidate(context.Context, fabric.CandidateCall) (fabric.Observation, error) {
	return fabric.Observation{Outcome: fabric.OutcomeUnknown}, nil
}
func (p *exactProvider) Pause(context.Context, fabric.PauseResumeCall) error { p.pauses++; return nil }
func (*exactProvider) Resume(context.Context, fabric.PauseResumeCall) error  { return nil }

type exactResolver struct {
	binding fabric.ProviderBinding
	calls   []fabric.ProviderBindingRef
}

func (*exactResolver) Current(context.Context, string, string) (fabric.ProviderBinding, error) {
	return fabric.ProviderBinding{}, fabric.ErrProviderUnavailable
}
func (r *exactResolver) Exact(_ context.Context, id string, revision int64) (fabric.ProviderBinding, error) {
	r.calls = append(r.calls, fabric.ProviderBindingRef{ID: id, Revision: revision})
	return r.binding, nil
}

func TestResolvedManagedProviderUsesDurableExactBinding(t *testing.T) {
	provider := &exactProvider{}
	binding := fabric.ProviderBinding{
		ProviderBindingRef: fabric.ProviderBindingRef{ID: "binding-a", Revision: 7}, FabricID: "sandbox",
		Availability: provider, Create: provider, Bootstrap: provider, Inspect: provider, Renew: provider, Destroy: provider, Candidate: provider, PauseResume: provider,
	}
	resolver := &exactResolver{binding: binding}
	proxy := resolvedManagedProvider{resolver: resolver, fabricID: "sandbox"}
	action := fabric.Action{ProviderBindingID: "binding-a", ProviderBindingRevision: 7}
	if _, err := proxy.Create(context.Background(), fabric.CreateCall{Action: action}); err != nil {
		t.Fatal(err)
	}
	if _, err := proxy.Inspect(context.Background(), fabric.InspectCall{ProviderBindingID: "binding-a", ProviderBindingRevision: 7}); err != nil {
		t.Fatal(err)
	}
	if err := proxy.Pause(context.Background(), fabric.PauseResumeCall{ProviderBindingID: "binding-a", ProviderBindingRevision: 7}); err != nil {
		t.Fatal(err)
	}
	if provider.creates != 1 || provider.reads != 1 || provider.pauses != 1 || len(resolver.calls) != 3 {
		t.Fatal("durable calls did not resolve the exact provider revision", provider, resolver.calls)
	}
	resolver.binding.Revision = 8
	if _, err := proxy.Create(context.Background(), fabric.CreateCall{Action: action}); err == nil {
		t.Fatal("resolver response for a different revision was accepted")
	}
}
