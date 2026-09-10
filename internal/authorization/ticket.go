package authorization

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"time"

	"github.com/aiomni/dune/internal/identity"
	"github.com/aiomni/dune/pkg/access"
	"github.com/aiomni/dune/pkg/api"
	"github.com/aiomni/dune/pkg/runner"
)

func credentialHash(value string) string {
	sum := sha256.Sum256([]byte(value))
	return hex.EncodeToString(sum[:])
}

const TicketLifetime = 30 * time.Second
const ticketPrefix = "dune_access_"
const maxPendingTickets = 4096
const maxSeenPeerNonces = 4096

// ConnectionAccess exists only in process memory or on an authenticated peer
// connection. Dune never persists browser credentials or access tickets.
type ConnectionAccess struct {
	Session, PrincipalID, Namespace, Subject, Target, RunnerID, FabricID, OwnerID string
	BindingRevision, ExpiresAt                                                    int64
}

func (r ConnectionAccess) Scope() access.Scope {
	return access.Scope{PrincipalID: r.PrincipalID, Namespace: r.Namespace, Subject: r.Subject, OwnerID: r.OwnerID,
		Binding: runner.Binding{RunnerID: r.RunnerID, FabricID: r.FabricID, MachineID: r.Target, Revision: r.BindingRevision}}
}

type Repository interface {
	IssueEnrollmentForSession(context.Context, identity.User, string, string) (string, int64, error)
	RevokeAuthorized(context.Context, identity.User, string, Resource) error
	MachineResource(context.Context, string) (Resource, error)
	RunnerResource(context.Context, string) (Resource, error)
	Candidates(context.Context, string, string, int) ([]Resource, error)
	ReadCursor(context.Context, string, string, string, string) (string, error)
	SaveCursor(context.Context, string, string, string, string) (string, error)
	EnrollmentIdentity(context.Context, string) (identity.User, string, error)
	MachineCredential(context.Context, string) (string, error)
	ConfirmMachineOnline(context.Context, api.Binding) error
	CheckRunnerAccess(context.Context, ConnectionAccess) (bool, error)
}
