package metadata

import (
	"context"
	"database/sql"
	"errors"
	"time"

	"github.com/aiomni/dune/internal/identity"
)

var _ identity.Repository = (*Store)(nil)

func (s *Store) lockUser(ctx context.Context, tx *sql.Tx, id string) error {
	if !s.localIdentity {
		return identity.ErrUnauthorized
	}
	query := `SELECT id FROM dune_users WHERE id=$1 AND enabled=TRUE`
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
	if !s.localIdentity {
		return identity.ErrRegistrationDisabled
	}
	return s.transaction(ctx, func(tx *sql.Tx) error {
		if _, err := tx.ExecContext(ctx, `INSERT INTO dune_users(id,email,salt,password_hash) VALUES($1,$2,$3,$4)`, account.ID, account.Email, account.Salt, account.PasswordHash); err != nil {
			return err
		}
		_, err := tx.ExecContext(ctx, `INSERT INTO dune_sessions(hash,user_id,expires_at,auth_version) VALUES($1,$2,$3,1)`, hash, account.ID, expires)
		return err
	})
}

func (s *Store) ReadAccount(ctx context.Context, email string) (identity.Account, error) {
	var account identity.Account
	if !s.localIdentity {
		return account, identity.ErrUnauthorized
	}
	err := s.db.QueryRowContext(ctx, `SELECT id,email,salt,password_hash FROM dune_users WHERE email=$1 AND enabled=TRUE`, email).Scan(&account.ID, &account.Email, &account.Salt, &account.PasswordHash)
	if errors.Is(err, sql.ErrNoRows) {
		err = identity.ErrUnauthorized
	}
	return account, err
}

func (s *Store) CreateSession(ctx context.Context, id, hash string, expires int64, limit int) error {
	return s.transaction(ctx, func(tx *sql.Tx) error {
		if err := s.lockUser(ctx, tx, id); err != nil {
			return err
		}
		now := time.Now().Unix()
		if _, err := tx.ExecContext(ctx, `DELETE FROM dune_sessions WHERE user_id=$1 AND expires_at<=$2`, id, now); err != nil {
			return err
		}
		var count int
		if err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM dune_sessions WHERE user_id=$1`, id).Scan(&count); err != nil {
			return err
		}
		if count >= limit {
			return identity.ErrSessionLimit
		}
		_, err := tx.ExecContext(ctx, `INSERT INTO dune_sessions(hash,user_id,expires_at,auth_version) SELECT $1,id,$3,auth_version FROM dune_users WHERE id=$2 AND enabled=TRUE`, hash, id, expires)
		return err
	})
}

func (s *Store) ReadSession(ctx context.Context, hash string, now int64) (identity.User, error) {
	var user identity.User
	if !s.localIdentity {
		return user, identity.ErrUnauthorized
	}
	err := s.db.QueryRowContext(ctx, `SELECT u.id,u.email FROM dune_sessions s JOIN dune_users u ON u.id=s.user_id WHERE s.hash=$1 AND s.expires_at>$2 AND u.enabled=TRUE AND s.auth_version=u.auth_version`, hash, now).Scan(&user.ID, &user.Email)
	if errors.Is(err, sql.ErrNoRows) {
		err = identity.ErrUnauthorized
	}
	return user, err
}

func (s *Store) DeleteSession(ctx context.Context, hash string) error {
	if !s.localIdentity {
		return nil
	}
	return s.transaction(ctx, func(tx *sql.Tx) error {
		_, err := tx.ExecContext(ctx, `DELETE FROM dune_sessions WHERE hash=$1`, hash)
		return err
	})
}

// SetUserEnabled is local-login administration. Enterprise deployments persist
// this state in their identity service instead of a Dune shadow user table.
func (s *Store) SetUserEnabled(ctx context.Context, id string, enabled bool) error {
	if !s.localIdentity {
		return ErrInvalidArgument
	}
	return s.transaction(ctx, func(tx *sql.Tx) error {
		query := `SELECT enabled FROM dune_users WHERE id=$1`
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
		if _, err := tx.ExecContext(ctx, `UPDATE dune_users SET enabled=$2,auth_version=auth_version+1 WHERE id=$1`, id, enabled); err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, `DELETE FROM dune_sessions WHERE user_id=$1`, id); err != nil {
			return err
		}
		_, err := tx.ExecContext(ctx, `DELETE FROM dune_enrollments WHERE owner_id=$1`, id)
		return err
	})
}

func (s *Store) checkLocalSession(ctx context.Context, tx *sql.Tx, userID, hash string) error {
	if !s.localIdentity {
		return nil
	}
	query := `SELECT s.hash FROM dune_sessions s JOIN dune_users u ON u.id=s.user_id WHERE s.hash=$1 AND s.user_id=$2 AND s.expires_at>$3 AND s.auth_version=u.auth_version AND u.enabled=TRUE`
	if s.postgres {
		query += ` FOR KEY SHARE OF s`
	}
	var found string
	err := tx.QueryRowContext(ctx, query, hash, userID, time.Now().Unix()).Scan(&found)
	if errors.Is(err, sql.ErrNoRows) {
		return identity.ErrUnauthorized
	}
	return err
}
