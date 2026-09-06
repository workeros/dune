package authorization

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"time"
)

const TicketLifetime = 30 * time.Second
const ticketPrefix = "dune_access_"

// ConnectionAccess fixes the authenticated session and binding at issuance.
// It contains only a session reference, never the original bearer credential.
// Ticket expiry limits connection establishment; a live connection continually
// rechecks the current session and binding instead of extending the ticket.
type ConnectionAccess struct {
	SessionHash, PrincipalID, Namespace, Target, RunnerID, FabricID string
	BindingRevision, AuthVersion, ExpiresAt                         int64
}

type Repository interface {
	MachineCredential(context.Context, string) (string, error)
	CreateAccess(context.Context, string, string, string, string, string, int64) (ConnectionAccess, error)
	ConsumeAccess(context.Context, string, string, int64) (ConnectionAccess, error)
	CheckAccess(context.Context, ConnectionAccess, int64) (bool, error)
	DeleteAccess(context.Context, string) error
}

func credentialHash(value string) string {
	sum := sha256.Sum256([]byte(value))
	return hex.EncodeToString(sum[:])
}
