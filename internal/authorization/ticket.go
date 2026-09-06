package authorization

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"time"

	"github.com/aiomni/dune/internal/identity"
	"github.com/aiomni/dune/pkg/runner"
)

const TicketLifetime = 30 * time.Second
const ticketPrefix = "dune_access_"

// ConnectionAccess fixes the authenticated session and binding at issuance.
// It contains only a session reference, never the original bearer credential.
// Ticket expiry limits connection establishment; a live connection continually
// rechecks the current session and binding instead of extending the ticket.
type ConnectionAccess struct {
	SessionHash, PrincipalID, Namespace, Target, RunnerID, FabricID, OwnerID string
	BindingRevision, AuthVersion, ExpiresAt                                  int64
}

type Repository interface {
	IssueEnrollmentForSession(context.Context, identity.User, string, string) (string, int64, error)
	RevokeAuthorized(context.Context, identity.User, string, Resource) error
	MachineResource(context.Context, string) (Resource, error)
	RunnerResource(context.Context, string) (Resource, error)
	Candidates(context.Context, string, string, int) ([]Resource, error)
	ReadCursor(context.Context, string, string, string, string) (string, error)
	SaveCursor(context.Context, string, string, string, string) (string, error)
	EnrollmentUser(context.Context, string) (identity.User, error)
	MachineCredential(context.Context, string) (string, error)
	CreateRunnerAccess(context.Context, string, string, string, string, runner.Binding, int64) (ConnectionAccess, error)
	ConsumeAccess(context.Context, string, string, int64) (ConnectionAccess, error)
	CheckAccess(context.Context, ConnectionAccess, int64) (bool, error)
	DeleteAccess(context.Context, string) error
}

func credentialHash(value string) string {
	sum := sha256.Sum256([]byte(value))
	return hex.EncodeToString(sum[:])
}
