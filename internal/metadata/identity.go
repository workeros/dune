package metadata

import (
	"context"
	"database/sql"
	"errors"
	"time"

	"github.com/aiomni/dune/internal/identity"
)

var _ identity.Repository = (*Store)(nil)

func (s *Store) lockPrincipal(ctx context.Context, tx *sql.Tx, id string) error {
	query := `SELECT id FROM dune_principals WHERE id = $1 AND enabled=TRUE`
	if s.postgres {
		query += ` FOR UPDATE`
	}
	var found string
	err := tx.QueryRowContext(ctx, query, id).Scan(&found)
	if errors.Is(err, sql.ErrNoRows) {
		return identity.ErrUnauthorized
	}
	return err
}

func (s *Store) RegisterAccount(ctx context.Context, account identity.Account, hash string, expires int64) error {
	return s.transaction(ctx, func(tx *sql.Tx) error {
		if _, err := tx.ExecContext(ctx, `INSERT INTO dune_principals(id, email) VALUES($1,$2)`, account.ID, account.Email); err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, `INSERT INTO dune_local_accounts(principal_id,email,salt,password_hash) VALUES($1,$2,$3,$4)`, account.ID, account.Email, account.Salt, account.PasswordHash); err != nil {
			return err
		}
		_, err := tx.ExecContext(ctx, `INSERT INTO dune_sessions(hash,principal_id,expires_at) VALUES($1,$2,$3)`, hash, account.ID, expires)
		return err
	})
}

func (s *Store) ReadAccount(ctx context.Context, email string) (identity.Account, error) {
	var a identity.Account
	err := s.db.QueryRowContext(ctx, `SELECT a.principal_id,a.email,a.salt,a.password_hash FROM dune_local_accounts a JOIN dune_principals p ON p.id=a.principal_id WHERE a.email=$1 AND p.enabled=TRUE`, email).Scan(&a.ID, &a.Email, &a.Salt, &a.PasswordHash)
	if errors.Is(err, sql.ErrNoRows) {
		err = identity.ErrUnauthorized
	}
	return a, err
}

func (s *Store) CreateSession(ctx context.Context, id, hash string, expires int64, limit int) error {
	return s.transaction(ctx, func(tx *sql.Tx) error {
		if err := s.lockPrincipal(ctx, tx, id); err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, `DELETE FROM dune_sessions WHERE principal_id=$1 AND expires_at<=$2`, id, time.Now().Unix()); err != nil {
			return err
		}
		var count int
		if err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM dune_sessions WHERE principal_id=$1`, id).Scan(&count); err != nil {
			return err
		}
		if count >= limit {
			return identity.ErrSessionLimit
		}
		_, err := tx.ExecContext(ctx, `INSERT INTO dune_sessions(hash,principal_id,expires_at,auth_version) SELECT $1,id,$3,auth_version FROM dune_principals WHERE id=$2 AND enabled=TRUE`, hash, id, expires)
		return err
	})
}

func (s *Store) ReadSession(ctx context.Context, hash string, now int64) (identity.User, error) {
	var user identity.User
	err := s.db.QueryRowContext(ctx, `SELECT p.id,p.email FROM dune_sessions s JOIN dune_principals p ON p.id=s.principal_id WHERE s.hash=$1 AND s.expires_at>$2 AND p.enabled=TRUE AND s.auth_version=p.auth_version`, hash, now).Scan(&user.ID, &user.Email)
	if errors.Is(err, sql.ErrNoRows) {
		err = identity.ErrUnauthorized
	}
	return user, err
}

func (s *Store) DeleteSession(ctx context.Context, hash string) error {
	return s.transaction(ctx, func(tx *sql.Tx) error {
		_, err := tx.ExecContext(ctx, `DELETE FROM dune_sessions WHERE hash=$1`, hash)
		return err
	})
}

// SetPrincipalEnabled is a trusted administrative operation. Disabling and
// revoking every session commit together; re-enabling cannot revive old tokens.
// The row lock serializes this operation with login and enrollment creation.
func (s *Store) SetPrincipalEnabled(ctx context.Context, id string, enabled bool) error {
	return s.transaction(ctx, func(tx *sql.Tx) error {
		query := `SELECT enabled FROM dune_principals WHERE id=$1`
		if s.postgres {
			query += ` FOR UPDATE`
		}
		var current bool
		if err := tx.QueryRowContext(ctx, query, id).Scan(&current); err != nil {
			if errors.Is(err, sql.ErrNoRows) {
				return ErrNotFound
			}
			return err
		}
		if current == enabled {
			return nil
		}
		if _, err := tx.ExecContext(ctx, `UPDATE dune_principals SET enabled=$2,auth_version=auth_version+1 WHERE id=$1`, id, enabled); err != nil {
			return err
		}
		for _, table := range []string{"dune_sessions", "dune_enrollments"} {
			if _, err := tx.ExecContext(ctx, "DELETE FROM "+table+" WHERE principal_id=$1", id); err != nil {
				return err
			}
		}
		return nil
	})
}
