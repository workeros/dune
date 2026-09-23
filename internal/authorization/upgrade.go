package authorization

import (
	"context"
	"time"

	"github.com/aiomni/dune/pkg/access"
	"github.com/aiomni/dune/pkg/gateway"
	"github.com/aiomni/dune/pkg/identity"
	"github.com/aiomni/dune/pkg/runner"
)

// MachineUpgradeBinding authenticates only the original machine credential.
// It never treats a browser session or a caller-supplied binding as authority.
func (l *Service) MachineUpgradeBinding(ctx context.Context, token string, expected runner.Binding) (runner.Binding, error) {
	if err := l.ctx.Err(); err != nil {
		return runner.Binding{}, err
	}
	target, err := l.bindings.MachineCredential(ctx, token)
	if err != nil {
		return runner.Binding{}, err
	}
	resource, err := l.bindings.MachineResource(ctx, target)
	if err != nil {
		return runner.Binding{}, err
	}
	if resource.Runner.Binding == nil || !resource.Runner.Binding.Valid() {
		return runner.Binding{}, identity.ErrUnauthorized
	}
	actual := *resource.Runner.Binding
	if expected != (runner.Binding{}) && actual != expected {
		return runner.Binding{}, runner.ErrBindingChanged
	}
	return actual, nil
}

// UpgradeVerification creates one narrowly scoped system read. A previously
// admitted worker must be able to verify its Runner without a browser session.
// Peer owners independently revalidate the binding and the single allowed RPC.
func (l *Service) UpgradeVerification(ctx context.Context, token string, binding runner.Binding) (gateway.BindingContext, gateway.ConnectionHandler, error) {
	actual, err := l.MachineUpgradeBinding(ctx, token, binding)
	if err != nil || !binding.Valid() {
		return gateway.BindingContext{}, nil, identity.ErrUnauthorized
	}
	resource, err := l.bindings.MachineResource(ctx, actual.MachineID)
	if err != nil {
		return gateway.BindingContext{}, nil, err
	}
	record := ConnectionAccess{Background: true, UpgradeVerification: true, PrincipalID: actual.MachineID, PrincipalKind: "runner_verifier", Namespace: l.sessions.Namespace(), Target: actual.MachineID, RunnerID: actual.RunnerID, FabricID: actual.FabricID, BindingRevision: actual.Revision, OwnerID: resource.OwnerID, ExpiresAt: time.Now().Add(TicketLifetime).Unix()}
	policy := l.streamPolicy(record)
	if l.bootID != "" {
		policy.Delegate = l.delegate(record)
	}
	return (access.Grant{Target: actual.MachineID, Role: gateway.RoleSDK, Policy: policy}).Bind()
}

func (l *Service) validateUpgradeVerification(ctx context.Context, record ConnectionAccess) (time.Time, error) {
	expires := time.Unix(record.ExpiresAt, 0)
	if !record.Background || record.Session != "" || record.PrincipalID != record.Target || record.PrincipalKind != "runner_verifier" || record.Subject != "" || !time.Now().Before(expires) || expires.After(time.Now().Add(TicketLifetime)) {
		return time.Time{}, identity.ErrUnauthorized
	}
	resource, err := l.bindings.MachineResource(ctx, record.Target)
	if err != nil {
		return time.Time{}, err
	}
	if resource.OwnerID != record.OwnerID || resource.Runner.Binding == nil || *resource.Runner.Binding != record.Scope().Binding {
		return time.Time{}, identity.ErrUnauthorized
	}
	return expires, nil
}
