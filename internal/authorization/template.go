package authorization

import (
	"context"

	"github.com/aiomni/dune/internal/identity"
	"github.com/aiomni/dune/internal/wire"
	"github.com/aiomni/dune/pkg/access"
	"github.com/aiomni/dune/pkg/fabric"
)

// TemplateDecision receives a selection already resolved against host
// configuration. User parameter values and private adapter settings are excluded.
func (l *Service) TemplateDecision(ctx context.Context, user identity.User, template fabric.Template, operation string) (access.Decision, error) {
	if operation != "template.list" && operation != "template.get" && operation != "runner.create" {
		return access.Decision{}, access.ErrDenied
	}
	return l.evaluate(ctx, access.Request{
		Scope:     access.Scope{PrincipalID: user.ID, Namespace: user.Namespace, Subject: user.Subject, OwnerID: user.ID},
		RequestID: wire.ID(), Operation: operation, Suboperation: "managed",
		Resource: access.Resource{FabricID: template.FabricID, TemplateID: template.ID, TemplateVersion: template.Version},
	})
}
