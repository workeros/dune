package host

import (
	"context"
	"time"

	"github.com/aiomni/dune/internal/metadata"
	"github.com/aiomni/dune/pkg/gateway"
	"github.com/aiomni/dune/pkg/identity"
	"github.com/aiomni/dune/pkg/managed"
	"github.com/aiomni/dune/pkg/runner"
)

type managedRunnerAccess struct {
	store  *metadata.Store
	core   *gateway.Gateway
	online func(context.Context, []string) (map[string]bool, error)
}

func (a managedRunnerAccess) IssueEnrollment(ctx context.Context, user identity.User, logical runner.Runner, fabricID string) (managed.Enrollment, error) {
	token, expires, err := a.store.IssueManagedEnrollment(ctx, user, logical, fabricID)
	return managed.Enrollment{Token: token, ExpiresAt: time.Unix(expires, 0).UTC()}, err
}

func (a managedRunnerAccess) State(ctx context.Context, user identity.User, id string) (managed.RunnerState, error) {
	logical, err := a.store.Runner(ctx, user.ID, id)
	if err != nil {
		return managed.RunnerState{}, err
	}
	state := managed.RunnerState{Runner: logical}
	if logical.Binding == nil {
		return state, nil
	}
	if a.online == nil {
		state.Online = a.core.Online(logical.Binding.MachineID)
		return state, nil
	}
	online, err := a.online(ctx, []string{logical.Binding.MachineID})
	if err != nil {
		return managed.RunnerState{}, err
	}
	state.Online = online[logical.Binding.MachineID]
	return state, nil
}

var _ managed.RunnerAccess = managedRunnerAccess{}
