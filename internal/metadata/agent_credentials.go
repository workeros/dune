package metadata

import (
	"context"
	"crypto/rand"
	"database/sql"
	"errors"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/aiomni/dune/pkg/agents"
	"github.com/aiomni/dune/pkg/identity"
	"github.com/aiomni/dune/pkg/workbench"
)

const agentCredentialPrefix = "dune_agent_"
const AgentCredentialLifetime = 30 * 24 * time.Hour

// AgentCredential identifies the calling launch, not a browser login or a
// target selected in tool arguments. Liveness and access still require Gateway.
type AgentCredential struct {
	Scope     agents.Scope
	SessionID string
	AttemptID string
	Target    workbench.AgentTarget
	ExpiresAt time.Time
}

func credentialIdentityValid(user identity.User) bool {
	if user.ID == "" {
		return false
	}
	for _, value := range []string{user.ID, user.Kind, user.Namespace, user.Subject} {
		if len(value) > 512 || !utf8.ValidString(value) || strings.ContainsFunc(value, unicode.IsControl) {
			return false
		}
	}
	return true
}

// IssueAgentCredential is called by trusted launch/injection code after scope
// authorization. It rotates this record's credential and returns the secret
// once, after a confirmed commit. Only its hash is stored, never in the Profile.
// Issuing before startup is allowed; use is denied until Runtime is recorded.
func (s *Store) IssueAgentCredential(ctx context.Context, scope agents.Scope, sessionID, attemptID string, expiresAt time.Time) (string, error) {
	if !credentialIdentityValid(scope.Principal) || scope.OwnerID == "" || !expiresAt.After(time.Now()) || expiresAt.After(time.Now().Add(AgentCredentialLifetime)) {
		return "", ErrInvalidArgument
	}
	token := agentCredentialPrefix + rand.Text() + rand.Text()
	err := s.transaction(ctx, func(tx *sql.Tx) error {
		query := `SELECT ` + agentSessionColumns + ` FROM dune_agent_sessions WHERE owner_id=$1 AND id=$2`
		if s.postgres {
			query += ` FOR UPDATE`
		}
		session, err := scanAgentSession(tx.QueryRowContext(ctx, query, scope.OwnerID, sessionID))
		if err != nil {
			return err
		}
		if attemptID == "" || session.Attempt.ID != attemptID || session.Attempt.State == "failed" || session.Attempt.State == "unknown" {
			return ErrConflict
		}
		if s.localIdentity {
			if scope.Principal.Namespace != "" {
				return identity.ErrUnauthorized
			}
			if err := s.lockUser(ctx, tx, scope.Principal.ID); err != nil {
				return err
			}
		}
		if _, err := tx.ExecContext(ctx, `DELETE FROM dune_agent_credentials WHERE expires_at<=$1 OR session_id=$2`, time.Now().Unix(), sessionID); err != nil {
			return err
		}
		_, err = tx.ExecContext(ctx, `INSERT INTO dune_agent_credentials(hash,session_id,attempt_id,principal_id,namespace,subject,kind,expires_at) VALUES($1,$2,$3,$4,$5,$6,$7,$8)`, tokenHash(token), sessionID, attemptID, scope.Principal.ID, scope.Principal.Namespace, scope.Principal.Subject, scope.Principal.Kind, expiresAt.Unix())
		return err
	})
	if err != nil {
		return "", err
	}
	return token, nil
}

// ReadAgentCredential rechecks expiry, identity namespace and launch attempt.
// A later resume invalidates the old token even before the new Runtime exists.
func (s *Store) ReadAgentCredential(ctx context.Context, token, namespace string) (AgentCredential, error) {
	var result AgentCredential
	if !strings.HasPrefix(token, agentCredentialPrefix) || len(token) != len(agentCredentialPrefix)+52 {
		return result, identity.ErrUnauthorized
	}
	var expires int64
	err := s.db.QueryRowContext(ctx, `SELECT session_id,attempt_id,principal_id,namespace,subject,kind,expires_at FROM dune_agent_credentials WHERE hash=$1 AND expires_at>$2`, tokenHash(token), time.Now().Unix()).Scan(&result.SessionID, &result.AttemptID, &result.Scope.Principal.ID, &result.Scope.Principal.Namespace, &result.Scope.Principal.Subject, &result.Scope.Principal.Kind, &expires)
	if errors.Is(err, sql.ErrNoRows) {
		return AgentCredential{}, identity.ErrUnauthorized
	}
	if err != nil {
		return AgentCredential{}, err
	}
	if result.Scope.Principal.Namespace != namespace || !credentialIdentityValid(result.Scope.Principal) {
		return AgentCredential{}, identity.ErrUnauthorized
	}
	// The session lookup takes no Owner supplied by the caller. Owner and target
	// are recovered exclusively from the record named by the credential hash.
	session, err := scanAgentSession(s.db.QueryRowContext(ctx, `SELECT `+agentSessionColumns+` FROM dune_agent_sessions WHERE id=$1`, result.SessionID))
	if errors.Is(err, ErrNotFound) {
		return AgentCredential{}, identity.ErrUnauthorized
	}
	if err != nil {
		return AgentCredential{}, err
	}
	if session.Attempt.ID != result.AttemptID || session.Attempt.Runtime == nil || session.Attempt.State == "failed" || session.Attempt.State == "unknown" {
		return AgentCredential{}, identity.ErrUnauthorized
	}
	if s.localIdentity {
		var enabled bool
		if err := s.db.QueryRowContext(ctx, `SELECT enabled FROM dune_users WHERE id=$1`, result.Scope.Principal.ID).Scan(&enabled); err != nil {
			if errors.Is(err, sql.ErrNoRows) {
				return AgentCredential{}, identity.ErrUnauthorized
			}
			return AgentCredential{}, err
		}
		if !enabled {
			return AgentCredential{}, identity.ErrUnauthorized
		}
	}
	result.Scope.OwnerID = session.OwnerID
	result.Target = workbench.AgentTarget{Binding: session.Launch.Binding, Runtime: *session.Attempt.Runtime}
	result.ExpiresAt = time.Unix(expires, 0)
	return result, nil
}

func (s *Store) RevokeAgentCredential(ctx context.Context, owner, sessionID string) error {
	_, err := s.db.ExecContext(ctx, `DELETE FROM dune_agent_credentials WHERE session_id IN (SELECT id FROM dune_agent_sessions WHERE id=$1 AND owner_id=$2)`, sessionID, owner)
	return err
}
