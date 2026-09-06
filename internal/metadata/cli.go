package metadata

import (
	"context"
	"database/sql"
	"errors"
	"strings"
	"time"

	"github.com/aiomni/dune/internal/identity"
	"github.com/aiomni/dune/internal/wire"
	"github.com/aiomni/dune/pkg/login"
)

func (s *Store) BeginCLI(ctx context.Context, challenge, site, namespace string) (login.Request, error) {
	if !identity.ValidProof(challenge) {
		return login.Request{}, identity.ErrInvalidArgument
	}
	id := wire.ID()
	request := login.Request{ID: id, Code: strings.ToUpper(id[:8]), VerificationURL: site + "?cli_login=" + id, ExpiresAt: time.Now().Add(identity.LoginLifetime).Unix()}
	err := s.transaction(ctx, func(tx *sql.Tx) error {
		if s.postgres {
			if _, err := tx.ExecContext(ctx, `SELECT pg_advisory_xact_lock(1146441287)`); err != nil {
				return err
			}
		}
		if _, err := tx.ExecContext(ctx, `DELETE FROM dune_cli_logins WHERE expires_at<=$1`, time.Now().Unix()); err != nil {
			return err
		}
		var count int
		if err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM dune_cli_logins`).Scan(&count); err != nil {
			return err
		}
		if count >= 10000 {
			return identity.ErrLoginLimit
		}
		_, err := tx.ExecContext(ctx, `INSERT INTO dune_cli_logins(id,challenge,site,identity_namespace,expires_at) VALUES($1,$2,$3,$4,$5)`, id, challenge, site, namespace, request.ExpiresAt)
		return err
	})
	if err != nil {
		return login.Request{}, err
	}
	return request, nil
}

func (s *Store) ReviewCLI(ctx context.Context, id, site, namespace string) (login.Review, error) {
	var review login.Review
	var parent sql.NullString
	err := s.db.QueryRowContext(ctx, `SELECT id,expires_at,session_hash FROM dune_cli_logins WHERE id=$1 AND site=$2 AND identity_namespace=$3 AND expires_at>$4`, id, site, namespace, time.Now().Unix()).Scan(&review.ID, &review.ExpiresAt, &parent)
	if errors.Is(err, sql.ErrNoRows) {
		err = identity.ErrUnauthorized
	}
	if err != nil {
		return login.Review{}, err
	}
	review.Site, review.Code, review.Confirmed = site, strings.ToUpper(review.ID[:8]), parent.Valid
	return review, nil
}

func (s *Store) ConfirmCLI(ctx context.Context, id, site, namespace, code, cookie, principal string, approve bool) error {
	if len(id) != 32 || strings.ToUpper(id[:8]) != code {
		return identity.ErrInvalidArgument
	}
	return s.transaction(ctx, func(tx *sql.Tx) error {
		if err := s.lockPrincipal(ctx, tx, principal); err != nil {
			return err
		}
		parent := tokenHash(cookie)
		query := `SELECT s.hash FROM dune_sessions s JOIN dune_principals p ON p.id=s.principal_id WHERE s.hash=$1 AND s.principal_id=$2 AND s.identity_namespace=$3 AND s.kind='browser' AND s.parent_hash IS NULL AND s.expires_at>$4 AND s.auth_version=p.auth_version AND p.enabled=TRUE`
		if s.postgres {
			query += ` FOR KEY SHARE OF s`
		}
		var found string
		if err := tx.QueryRowContext(ctx, query, parent, principal, namespace, time.Now().Unix()).Scan(&found); err != nil {
			if errors.Is(err, sql.ErrNoRows) {
				return identity.ErrUnauthorized
			}
			return err
		}
		var current sql.NullString
		query = `SELECT session_hash FROM dune_cli_logins WHERE id=$1 AND site=$2 AND identity_namespace=$3 AND expires_at>$4`
		if s.postgres {
			query += ` FOR UPDATE`
		}
		if err := tx.QueryRowContext(ctx, query, id, site, namespace, time.Now().Unix()).Scan(&current); err != nil {
			if errors.Is(err, sql.ErrNoRows) {
				return identity.ErrUnauthorized
			}
			return err
		}
		if current.Valid {
			if approve && current.String == parent {
				return nil
			}
			return ErrConflict
		}
		if !approve {
			_, err := tx.ExecContext(ctx, `DELETE FROM dune_cli_logins WHERE id=$1`, id)
			return err
		}
		_, err := tx.ExecContext(ctx, `UPDATE dune_cli_logins SET session_hash=$2 WHERE id=$1`, id, parent)
		return err
	})
}

func (s *Store) ConsumeCLI(ctx context.Context, id, site, namespace, verifier string) (login.Session, error) {
	if len(id) != 32 || !identity.ValidProof(verifier) {
		return login.Session{}, identity.ErrUnauthorized
	}
	var session login.Session
	err := s.transaction(ctx, func(tx *sql.Tx) error {
		var parent sql.NullString
		if err := tx.QueryRowContext(ctx, `SELECT session_hash FROM dune_cli_logins WHERE id=$1 AND site=$2 AND identity_namespace=$3 AND challenge=$4 AND expires_at>$5`, id, site, namespace, tokenHash(verifier), time.Now().Unix()).Scan(&parent); err != nil {
			if errors.Is(err, sql.ErrNoRows) {
				return identity.ErrUnauthorized
			}
			return err
		}
		if !parent.Valid {
			return identity.ErrPending
		}
		if err := tx.QueryRowContext(ctx, `SELECT principal_id FROM dune_sessions WHERE hash=$1`, parent.String).Scan(&session.PrincipalID); err != nil {
			if errors.Is(err, sql.ErrNoRows) {
				return identity.ErrUnauthorized
			}
			return err
		}
		if err := s.lockPrincipal(ctx, tx, session.PrincipalID); err != nil {
			return err
		}
		query := `SELECT s.expires_at FROM dune_sessions s JOIN dune_principals p ON p.id=s.principal_id WHERE s.hash=$1 AND s.kind='browser' AND s.parent_hash IS NULL AND s.identity_namespace=$2 AND s.expires_at>$3 AND s.auth_version=p.auth_version AND p.enabled=TRUE`
		if s.postgres {
			query += ` FOR KEY SHARE OF s`
		}
		if err := tx.QueryRowContext(ctx, query, parent.String, namespace, time.Now().Unix()).Scan(&session.ExpiresAt); err != nil {
			if errors.Is(err, sql.ErrNoRows) {
				return identity.ErrUnauthorized
			}
			return err
		}
		var consumed string
		if err := tx.QueryRowContext(ctx, `DELETE FROM dune_cli_logins WHERE id=$1 AND site=$2 AND identity_namespace=$3 AND challenge=$4 AND session_hash=$5 AND expires_at>$6 RETURNING id`, id, site, namespace, tokenHash(verifier), parent.String, time.Now().Unix()).Scan(&consumed); err != nil {
			if errors.Is(err, sql.ErrNoRows) {
				return identity.ErrUnauthorized
			}
			return err
		}
		session.Site, session.Token = site, identity.CLIPrefix+wire.ID()+wire.ID()
		session.ExpiresAt = min(session.ExpiresAt, time.Now().Add(identity.CLISessionLifetime).Unix())
		if err := s.createSession(ctx, tx, session.PrincipalID, tokenHash(session.Token), session.ExpiresAt, 32, namespace); err != nil {
			return err
		}
		_, err := tx.ExecContext(ctx, `UPDATE dune_sessions SET kind='cli',parent_hash=$2 WHERE hash=$1`, tokenHash(session.Token), parent.String)
		return err
	})
	if err != nil {
		return login.Session{}, err
	}
	return session, nil
}
