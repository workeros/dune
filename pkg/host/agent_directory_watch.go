package host

import (
	"context"
	"sync"

	"github.com/aiomni/dune/pkg/agents"
)

func (a *App) AgentDirectoryObserver() agents.DirectoryObserver { return &agentDirectory{app: a} }

func (d *agentDirectory) Subscribe(ctx context.Context, scope agents.Scope, query agents.DirectoryWatch) (agents.DirectorySubscription, error) {
	ctx, finish, err := d.app.adminContext(ctx)
	if err != nil {
		return nil, err
	}
	subscription, err := d.app.agentService().Subscribe(ctx, scope, query)
	if err != nil {
		finish()
		return nil, err
	}
	owned := &hostDirectorySubscription{DirectorySubscription: subscription, finish: finish}
	context.AfterFunc(ctx, func() { _ = owned.Close() })
	return owned, nil
}

type hostDirectorySubscription struct {
	agents.DirectorySubscription
	once   sync.Once
	finish func()
}

func (s *hostDirectorySubscription) Close() error {
	s.once.Do(func() {
		_ = s.DirectorySubscription.Close()
		s.finish()
	})
	return nil
}
