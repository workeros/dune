package metadata

import (
	"context"
	"database/sql"
	"errors"
	"time"

	"github.com/aiomni/dune/internal/identity"
	public "github.com/aiomni/dune/pkg/identity"
)

func (s *Store) BeginLogin(ctx context.Context, login identity.LoginTransaction) error {
	return s.transaction(ctx, func(tx *sql.Tx) error {
		// Bound anonymous pending state across replicas as well as per HTTP peer.
		if s.postgres {
			if _, err := tx.ExecContext(ctx, `SELECT pg_advisory_xact_lock(1146441286)`); err != nil {
				return err
			}
		}
		if _, err := tx.ExecContext(ctx, `DELETE FROM dune_login_transactions WHERE expires_at<=$1`, time.Now().Unix()); err != nil {
			return err
		}
		var count int
		if err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM dune_login_transactions`).Scan(&count); err != nil {
			return err
		}
		if count >= 10000 {
			return identity.ErrLoginLimit
		}
		_, err := tx.ExecContext(ctx, `INSERT INTO dune_login_transactions(state_hash,browser_hash,namespace,redirect_url,nonce,verifier,expires_at) VALUES($1,$2,$3,$4,$5,$6,$7)`, login.StateHash, login.BrowserHash, login.Namespace, login.RedirectURL, login.Nonce, login.Verifier, login.ExpiresAt)
		return err
	})
}

func (s *Store) ConsumeLogin(ctx context.Context, stateHash, browserHash, namespace, redirect string, now int64) (identity.LoginTransaction, error) {
	var login identity.LoginTransaction
	err := s.transaction(ctx, func(tx *sql.Tx) error {
		err := tx.QueryRowContext(ctx, `DELETE FROM dune_login_transactions WHERE state_hash=$1 AND browser_hash=$2 AND namespace=$3 AND redirect_url=$4 AND expires_at>$5 RETURNING nonce,verifier`, stateHash, browserHash, namespace, redirect, now).Scan(&login.Nonce, &login.Verifier)
		if errors.Is(err, sql.ErrNoRows) {
			return identity.ErrUnauthorized
		}
		return err
	})
	return login, err
}

func (s *Store) ExternalLogin(ctx context.Context, namespace string, subject public.Subject, newID, hash string, expires int64, limit int) (identity.User, error) {
	var user identity.User
	err := s.transaction(ctx, func(tx *sql.Tx) error {
		if err := s.lockExternalIdentity(ctx, tx, namespace, subject.ID); err != nil {
			return err
		}
		err := tx.QueryRowContext(ctx, `SELECT principal_id FROM dune_external_identities WHERE namespace=$1 AND subject=$2`, namespace, subject.ID).Scan(&user.ID)
		if errors.Is(err, sql.ErrNoRows) {
			user.ID = newID
			if _, err := tx.ExecContext(ctx, `INSERT INTO dune_principals(id,email) VALUES($1,$2)`, user.ID, subject.Email); err != nil {
				return err
			}
			if _, err := tx.ExecContext(ctx, `INSERT INTO dune_external_identities(namespace,subject,principal_id) VALUES($1,$2,$3)`, namespace, subject.ID, user.ID); err != nil {
				return err
			}
		} else if err != nil {
			return err
		}
		if err := s.createSession(ctx, tx, user.ID, hash, expires, limit, namespace); err != nil {
			return err
		}
		user.Email = subject.Email
		_, err = tx.ExecContext(ctx, `UPDATE dune_principals SET email=$2 WHERE id=$1`, user.ID, user.Email)
		return err
	})
	if err != nil {
		return identity.User{}, err
	}
	return user, nil
}
