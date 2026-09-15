package authorization

import (
	"context"

	"github.com/aiomni/dune/internal/identity"
	"github.com/aiomni/dune/internal/wire"
	"github.com/aiomni/dune/pkg/access"
	"github.com/aiomni/dune/pkg/gateway"
	"github.com/aiomni/dune/pkg/runner"
)

// BackgroundRunner creates an in-process host grant for one exact Runner
// binding. The caller is trusted server code; the supplied principal still
// traverses the configured operation AccessChecker. This grant does not depend
// on a browser session and does not add a principal-revocation contract.
func (l *Service) BackgroundRunner(ctx context.Context, user identity.User, ownerID string, binding runner.Binding) (gateway.BindingContext, gateway.ConnectionHandler, error) {
	if err := l.ctx.Err(); err != nil {
		return gateway.BindingContext{}, nil, err
	}
	if err := ctx.Err(); err != nil {
		return gateway.BindingContext{}, nil, err
	}
	if user.ID == "" || ownerID == "" || user.Namespace != l.sessions.Namespace() || !binding.Valid() {
		return gateway.BindingContext{}, nil, identity.ErrUnauthorized
	}
	resource, err := l.bindings.RunnerResource(ctx, binding.RunnerID)
	if err != nil {
		return gateway.BindingContext{}, nil, err
	}
	if resource.OwnerID != ownerID {
		return gateway.BindingContext{}, nil, ErrNotFound
	}
	if resource.Runner.Binding == nil || *resource.Runner.Binding != binding {
		return gateway.BindingContext{}, nil, runner.ErrBindingChanged
	}
	request := access.Request{Scope: scope(user, resource), RequestID: wire.ID(), Operation: "runner.connect", Suboperation: resource.Runner.Kind}
	if _, err := l.evaluate(ctx, request); err != nil {
		return gateway.BindingContext{}, nil, err
	}
	record := ConnectionAccess{Background: true, PrincipalID: user.ID, PrincipalKind: user.Kind, Namespace: user.Namespace, Subject: user.Subject, Target: binding.MachineID, RunnerID: binding.RunnerID, FabricID: binding.FabricID, BindingRevision: binding.Revision, OwnerID: ownerID}
	valid := l.validAccess(record)
	if !valid() {
		return gateway.BindingContext{}, nil, ErrNotFound
	}
	policy := &access.Policy{Scope: record.Scope(), Checker: l.checker, Observer: l.observer}
	if l.bootID != "" {
		policy.Delegate = l.delegate(record)
	}
	return (access.Grant{Target: binding.MachineID, Role: gateway.RoleSDK, Valid: valid, Policy: policy}).Bind()
}
