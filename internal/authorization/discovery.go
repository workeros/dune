package authorization

import (
	"context"
	"errors"
	"time"

	"github.com/aiomni/dune/internal/identity"
	"github.com/aiomni/dune/internal/wire"
	"github.com/aiomni/dune/pkg/access"
	"github.com/aiomni/dune/pkg/runner"
)

// Resource is read from authoritative metadata, never decoded from a browser.
type Resource struct {
	Runner            runner.Runner
	OwnerID, OS, Arch string
}
type Page struct {
	Items      []Resource
	NextCursor string
}

func scope(user identity.User, resource Resource) access.Scope {
	out := access.Scope{PrincipalID: user.ID, Namespace: user.Namespace, OwnerID: resource.OwnerID}
	if resource.Runner.Binding != nil {
		out.Binding = *resource.Runner.Binding
	} else {
		out.Binding.RunnerID = resource.Runner.ID
	}
	return out
}

func (l *Service) Check(ctx context.Context, user identity.User, resource Resource, operation, suboperation string) (access.Decision, error) {
	return access.Evaluate(ctx, l.checker, access.Request{Scope: scope(user, resource), RequestID: wire.ID(), Operation: operation, Suboperation: suboperation})
}

func (l *Service) Resource(ctx context.Context, user identity.User, id string, machine bool, operation string) (Resource, access.Decision, error) {
	var resource Resource
	var err error
	if machine {
		resource, err = l.bindings.MachineResource(ctx, id)
	} else {
		resource, err = l.bindings.RunnerResource(ctx, id)
	}
	if err != nil {
		return Resource{}, access.Decision{}, err
	}
	decision, err := l.Check(ctx, user, resource, operation, resource.Runner.Kind)
	if errors.Is(err, access.ErrDenied) && !errors.Is(err, access.ErrUnavailable) {
		err = ErrNotFound
	}
	return resource, decision, err
}

func (l *Service) Discover(ctx context.Context, user identity.User, query runner.Query, machines bool) (Page, error) {
	limit := query.Limit
	if limit == 0 {
		limit = 32
	}
	if limit < 1 || limit > 100 {
		return Page{}, identity.ErrInvalidArgument
	}
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	operation := "runner.list"
	if machines {
		operation = "machine.list"
	}
	after := ""
	var err error
	if query.Cursor != "" {
		after, err = l.bindings.ReadCursor(ctx, user.ID, user.Namespace, operation, query.Cursor)
		if err != nil {
			return Page{}, err
		}
	}
	owner := ""
	if l.ownerOnly {
		owner = user.ID
	}
	out := Page{Items: []Resource{}}
	rows, err := l.bindings.Candidates(ctx, owner, after, 128)
	if err != nil {
		return Page{}, err
	}
	more := len(rows) == 128
	var deadlines []time.Time
	for i, resource := range rows {
		after = resource.Runner.ID
		if machines && resource.Runner.Binding == nil {
			continue
		}
		decision, err := l.Check(ctx, user, resource, operation, resource.Runner.Kind)
		if errors.Is(err, access.ErrUnavailable) {
			return Page{}, err
		}
		if err != nil {
			continue
		}
		out.Items = append(out.Items, resource)
		deadlines = append(deadlines, decision.ValidUntil)
		if len(out.Items) == limit {
			more = more || i < len(rows)-1
			break
		}
	}
	if more {
		out.NextCursor, err = l.bindings.SaveCursor(ctx, user.ID, user.Namespace, operation, after)
		if err != nil {
			return Page{}, err
		}
	}
	for _, deadline := range deadlines {
		if !time.Now().Before(deadline) {
			return Page{}, access.ErrUnavailable
		}
	}
	return out, nil
}
