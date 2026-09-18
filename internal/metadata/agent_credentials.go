package metadata

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/json"
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

// AgentCredential binds a caller to one execution instance. Runtime liveness
// and current access are checked through Gateway on every MCP request.
type AgentCredential struct {
	Scope     agents.Scope
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

// IssueAgentCredential accepts only a confirmed Runtime returned by trusted
// startup code. It does not infer authority from a native session or recovery
// record. Reissuing for the same target atomically replaces its previous token.
func (s *Store) IssueAgentCredential(ctx context.Context, scope agents.Scope, target workbench.AgentTarget, expiresAt time.Time) (string, error) {
	if !credentialIdentityValid(scope.Principal) || scope.OwnerID == "" || len(scope.OwnerID) > 256 || target.Validate() != nil || !expiresAt.After(time.Now()) || expiresAt.After(time.Now().Add(AgentCredentialLifetime)) {
		return "", ErrInvalidArgument
	}
	payload, err := json.Marshal(target)
	if err != nil {
		return "", ErrInvalidArgument
	}
	token := agentCredentialPrefix + rand.Text() + rand.Text()
	err = s.transaction(ctx, func(tx *sql.Tx) error {
		if s.localIdentity {
			if scope.Principal.Namespace != "" {
				return identity.ErrUnauthorized
			}
			if err := s.lockUser(ctx, tx, scope.Principal.ID); err != nil {
				return err
			}
		}
		var found int
		b := target.Binding
		err := tx.QueryRowContext(ctx, `SELECT 1 FROM dune_runners WHERE owner_id=$1 AND id=$2 AND machine_id=$3 AND fabric_id=$4 AND binding_revision=$5 AND enabled=TRUE AND suspended=FALSE`, scope.OwnerID, b.RunnerID, b.MachineID, b.FabricID, b.Revision).Scan(&found)
		if errors.Is(err, sql.ErrNoRows) {
			return ErrNotFound
		}
		if err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, `DELETE FROM dune_agent_credentials WHERE expires_at<=$1`, time.Now().Unix()); err != nil {
			return err
		}
		_, err = tx.ExecContext(ctx, `INSERT INTO dune_agent_credentials(hash,owner_id,target_key,target,principal_id,namespace,subject,kind,expires_at) VALUES($1,$2,$3,$4,$5,$6,$7,$8,$9)
			ON CONFLICT(owner_id,target_key) DO UPDATE SET hash=excluded.hash,target=excluded.target,principal_id=excluded.principal_id,namespace=excluded.namespace,subject=excluded.subject,kind=excluded.kind,expires_at=excluded.expires_at`, tokenHash(token), scope.OwnerID, target.Key(), string(payload), scope.Principal.ID, scope.Principal.Namespace, scope.Principal.Subject, scope.Principal.Kind, expiresAt.Unix())
		return err
	})
	if err != nil {
		return "", err
	}
	return token, nil
}

// ReadAgentCredential reads the caller's identity and target from the token
// binding alone. It never accepts an Owner or Runtime supplied by a tool call.
func (s *Store) ReadAgentCredential(ctx context.Context, token, namespace string) (AgentCredential, error) {
	var result AgentCredential
	if !strings.HasPrefix(token, agentCredentialPrefix) || len(token) != len(agentCredentialPrefix)+52 {
		return result, identity.ErrUnauthorized
	}
	var expires int64
	var targetKey, payload string
	err := s.db.QueryRowContext(ctx, `SELECT owner_id,target_key,target,principal_id,namespace,subject,kind,expires_at FROM dune_agent_credentials WHERE hash=$1 AND expires_at>$2`, tokenHash(token), time.Now().Unix()).Scan(&result.Scope.OwnerID, &targetKey, &payload, &result.Scope.Principal.ID, &result.Scope.Principal.Namespace, &result.Scope.Principal.Subject, &result.Scope.Principal.Kind, &expires)
	if errors.Is(err, sql.ErrNoRows) {
		return AgentCredential{}, identity.ErrUnauthorized
	}
	if err != nil {
		return AgentCredential{}, err
	}
	if result.Scope.OwnerID == "" || result.Scope.Principal.Namespace != namespace || !credentialIdentityValid(result.Scope.Principal) || json.Unmarshal([]byte(payload), &result.Target) != nil || result.Target.Validate() != nil || result.Target.Key() != targetKey {
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
	result.ExpiresAt = time.Unix(expires, 0)
	return result, nil
}

func (s *Store) RevokeAgentCredential(ctx context.Context, owner string, target workbench.AgentTarget) error {
	if owner == "" || target.Validate() != nil {
		return ErrInvalidArgument
	}
	_, err := s.db.ExecContext(ctx, `DELETE FROM dune_agent_credentials WHERE owner_id=$1 AND target_key=$2`, owner, target.Key())
	return err
}
