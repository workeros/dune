package access

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"
	"unicode"

	"github.com/aiomni/dune/internal/wire"
	"github.com/aiomni/dune/pkg/runner"
)

const CheckTimeout = time.Second
const MaxLease = 30 * time.Second

var ErrDenied = errors.New("access denied")
var ErrUnavailable = fmt.Errorf("access checker unavailable: %w", ErrDenied)

// Scope is supplied from authenticated session and authoritative binding data.
// None of these fields may be taken from an execution request payload.
type Scope struct {
	PrincipalID string
	Namespace   string
	// Subject is the verified external ID within Namespace for this login.
	// Both are empty for local login; email and upstream tokens are excluded.
	Subject string
	OwnerID string
	Binding runner.Binding
}

// RuntimeIdentity identifies a selected runtime; the protocol and fabricd still
// verify its incarnation/generation. Adapter is filled from a fabricd response.
type RuntimeIdentity struct {
	ID          string
	Incarnation string
	Generation  uint64
	Adapter     string
}

// Resource contains only bounded routing/selection attributes. Paths are not
// sandbox boundaries; permission to execute a shell permits its system calls.
type Resource struct {
	Path        string
	Destination string
	Directory   string
	UploadID    string
	ConfigID    string
	Port        int
	Adapter     string
	ManagedACP  bool
	Observe     bool
}

// Request deliberately excludes commands, environment, file contents, prompts,
// terminal bytes, browser bodies and user credentials. Continuation decisions
// belong to the original stream RequestID and fixed operation scope.
type Request struct {
	Scope
	Runtime      RuntimeIdentity
	RequestID    string
	Operation    string
	Suboperation string
	Mode         string
	Resource     Resource
}

type Decision struct {
	Allowed    bool
	Reason     string
	ID         string
	ValidUntil time.Time
}

// Checker must honor context cancellation and may be called concurrently.
// Each decision describes its input request only; it cannot replace identity or
// binding, add protocol permissions, or relax machine/session validity.
type Checker interface {
	Check(context.Context, Request) (Decision, error)
}

// Policy selects one checker. There is no allow-by-either composition or owner
// fallback when the selected checker rejects, times out, or fails.
type Policy struct {
	Scope   Scope
	Checker Checker
}

// Owner implements the default personal policy with the same decision contract.
type Owner struct{}

func (Owner) Check(ctx context.Context, r Request) (Decision, error) {
	if err := ctx.Err(); err != nil {
		return Decision{}, err
	}
	return Decision{Allowed: r.PrincipalID != "" && r.PrincipalID == r.OwnerID, Reason: "OWNER", ID: wire.ID(), ValidUntil: time.Now().Add(MaxLease)}, nil
}

func (p Policy) check(ctx context.Context, r Request) (Decision, error) {
	return Evaluate(ctx, p.Checker, r)
}

// Evaluate applies the same bounded decision contract to product API requests.
// Callers construct operation and scope from authenticated, authoritative data.
func Evaluate(ctx context.Context, checker Checker, r Request) (Decision, error) {
	ctx, cancel := context.WithTimeout(ctx, CheckTimeout)
	defer cancel()
	if checker == nil {
		return Decision{}, ErrUnavailable
	}
	decision, err := checker.Check(ctx, r)
	if err != nil || ctx.Err() != nil || !validCode(decision.Reason) || decision.ID == "" || len(decision.ID) > 128 || strings.ContainsFunc(decision.ID, unicode.IsControl) {
		return Decision{}, ErrUnavailable
	}
	if !decision.Allowed {
		return Decision{}, ErrDenied
	}
	now := time.Now()
	if !decision.ValidUntil.After(now) {
		return Decision{}, ErrUnavailable
	}
	// Convert an external wall-clock expiry into a local monotonic deadline.
	decision.ValidUntil = now.Add(min(MaxLease, decision.ValidUntil.Sub(now)))
	return decision, nil
}
func validCode(s string) bool {
	return len(s) > 0 && len(s) <= 64 && !strings.ContainsFunc(s, func(r rune) bool { return r != '_' && (r < 'A' || r > 'Z') && (r < '0' || r > '9') })
}
