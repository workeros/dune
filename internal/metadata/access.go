package metadata

import (
	"context"
	"database/sql"
	"errors"
	"time"

	"github.com/aiomni/dune/internal/authorization"
	"github.com/aiomni/dune/internal/identity"
)

func (s *Store) CreateAccess(ctx context.Context, hash, sessionHash, principal, namespace, target string, expires int64) (authorization.ConnectionAccess, error) {
	record := authorization.ConnectionAccess{SessionHash: sessionHash, PrincipalID: principal, Namespace: namespace, Target: target, ExpiresAt: expires}
	err := s.transaction(ctx, func(tx *sql.Tx) error {
		if err := s.lockPrincipal(ctx, tx, principal); err != nil {
			return err
		}
		now := time.Now().Unix()
		sessionQuery := `SELECT s.auth_version FROM dune_sessions s JOIN dune_principals p ON p.id=s.principal_id WHERE s.hash=$1 AND s.principal_id=$2 AND s.identity_namespace=$3 AND s.expires_at>$4 AND p.enabled=TRUE AND p.auth_version=s.auth_version` + liveSessionParent("$4")
		if s.postgres {
			// Lock parents before touching tickets, consistently with cascading
			// logout/revocation. Otherwise cleanup followed by FK insertion can
			// deadlock with a concurrent parent deletion waiting for that cleanup.
			sessionQuery += ` FOR KEY SHARE OF s`
		}
		if err := tx.QueryRowContext(ctx, sessionQuery, sessionHash, principal, namespace, now).Scan(&record.AuthVersion); err != nil {
			if errors.Is(err, sql.ErrNoRows) {
				return identity.ErrUnauthorized
			}
			return err
		}
		bindingQuery := `SELECT r.id,r.fabric_id,r.binding_revision FROM dune_machines m JOIN dune_runners r ON r.id=m.runner_id WHERE m.id=$1 AND r.owner_id=$2`
		if s.postgres {
			bindingQuery += ` FOR KEY SHARE OF r`
		}
		if err := tx.QueryRowContext(ctx, bindingQuery, target, principal).Scan(&record.RunnerID, &record.FabricID, &record.BindingRevision); err != nil {
			if errors.Is(err, sql.ErrNoRows) {
				return authorization.ErrNotFound
			}
			return err
		}
		if s.postgres {
			var machine string
			if err := tx.QueryRowContext(ctx, `SELECT id FROM dune_machines WHERE id=$1 AND runner_id=$2 FOR KEY SHARE`, target, record.RunnerID).Scan(&machine); err != nil {
				if errors.Is(err, sql.ErrNoRows) {
					return authorization.ErrNotFound
				}
				return err
			}
		}
		if _, err := tx.ExecContext(ctx, `DELETE FROM dune_access_tickets WHERE session_hash=$1 AND expires_at<=$2`, sessionHash, now); err != nil {
			return err
		}
		var count int
		if err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM dune_access_tickets WHERE session_hash=$1`, sessionHash).Scan(&count); err != nil {
			return err
		}
		if count >= 64 {
			return identity.ErrLoginLimit
		}
		_, err := tx.ExecContext(ctx, `INSERT INTO dune_access_tickets(hash,session_hash,principal_id,identity_namespace,machine_id,runner_id,fabric_id,binding_revision,auth_version,expires_at) VALUES($1,$2,$3,$4,$5,$6,$7,$8,$9,$10)`, hash, sessionHash, principal, namespace, target, record.RunnerID, record.FabricID, record.BindingRevision, record.AuthVersion, expires)
		return err
	})
	if err != nil {
		return authorization.ConnectionAccess{}, err
	}
	return record, nil
}

func (s *Store) ConsumeAccess(ctx context.Context, hash, namespace string, now int64) (authorization.ConnectionAccess, error) {
	var record authorization.ConnectionAccess
	err := s.transaction(ctx, func(tx *sql.Tx) error {
		err := tx.QueryRowContext(ctx, `DELETE FROM dune_access_tickets WHERE hash=$1 AND identity_namespace=$2 AND expires_at>$3 RETURNING session_hash,principal_id,identity_namespace,machine_id,runner_id,fabric_id,binding_revision,auth_version,expires_at`, hash, namespace, now).Scan(&record.SessionHash, &record.PrincipalID, &record.Namespace, &record.Target, &record.RunnerID, &record.FabricID, &record.BindingRevision, &record.AuthVersion, &record.ExpiresAt)
		if errors.Is(err, sql.ErrNoRows) {
			return identity.ErrUnauthorized
		}
		return err
	})
	if err != nil {
		return authorization.ConnectionAccess{}, err
	}
	return record, nil
}

func (s *Store) CheckAccess(ctx context.Context, record authorization.ConnectionAccess, now int64) (bool, error) {
	var found int
	err := s.db.QueryRowContext(ctx, `SELECT 1 FROM dune_sessions s JOIN dune_principals p ON p.id=s.principal_id JOIN dune_machines m ON m.id=$1 JOIN dune_runners r ON r.id=m.runner_id WHERE s.hash=$2 AND s.principal_id=$3 AND s.identity_namespace=$4 AND s.expires_at>$5 AND s.auth_version=$6 AND p.enabled=TRUE AND p.auth_version=$6 AND r.owner_id=p.id AND r.id=$7 AND r.fabric_id=$8 AND r.binding_revision=$9`+liveSessionParent("$5"), record.Target, record.SessionHash, record.PrincipalID, record.Namespace, now, record.AuthVersion, record.RunnerID, record.FabricID, record.BindingRevision).Scan(&found)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	return err == nil, err
}

func (s *Store) DeleteAccess(ctx context.Context, hash string) error {
	return s.transaction(ctx, func(tx *sql.Tx) error {
		_, err := tx.ExecContext(ctx, `DELETE FROM dune_access_tickets WHERE hash=$1`, hash)
		return err
	})
}
