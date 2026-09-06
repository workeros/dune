package metadata

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/binary"
	"errors"
	"time"

	"github.com/aiomni/dune/internal/identity"
	public "github.com/aiomni/dune/pkg/identity"
)

func (s *Store) lockExternalIdentity(ctx context.Context, tx *sql.Tx, namespace, subject string) error {
	if !s.postgres {
		return nil
	}
	// Use the same lock for first login and explicit linking, before a row exists.
	key := sha256.Sum256([]byte("dune-identity\x00" + namespace + "\x00" + subject))
	_, err := tx.ExecContext(ctx, `SELECT pg_advisory_xact_lock($1)`, int64(binary.BigEndian.Uint64(key[:8])))
	return err
}

func (s *Store) LinkIdentity(ctx context.Context, request public.LinkRequest) (public.LinkRecord, error) {
	if err := identity.ValidateLink(request); err != nil {
		return public.LinkRecord{}, err
	}
	var result public.LinkRecord
	err := s.transaction(ctx, func(tx *sql.Tx) error {
		if err := s.lockExternalIdentity(ctx, tx, request.Namespace, request.Subject); err != nil {
			return err
		}
		previous, err := readIdentityLink(ctx, tx, request.RequestID)
		if err == nil {
			if previous.LinkRequest != request {
				return public.ErrLinkConflict
			}
			result = previous
			return nil
		}
		if !errors.Is(err, public.ErrLinkNotFound) {
			return err
		}
		if err := s.lockPrincipal(ctx, tx, request.PrincipalID); err != nil {
			return err
		}
		var owner string
		err = tx.QueryRowContext(ctx, `SELECT principal_id FROM dune_external_identities WHERE namespace=$1 AND subject=$2`, request.Namespace, request.Subject).Scan(&owner)
		if err == nil && owner != request.PrincipalID {
			return public.ErrLinkConflict
		}
		if err != nil && !errors.Is(err, sql.ErrNoRows) {
			return err
		}
		if errors.Is(err, sql.ErrNoRows) {
			if _, err := tx.ExecContext(ctx, `INSERT INTO dune_external_identities(namespace,subject,principal_id) VALUES($1,$2,$3)`, request.Namespace, request.Subject, request.PrincipalID); err != nil {
				return err
			}
		}
		if _, err := tx.ExecContext(ctx, `UPDATE dune_principals SET auth_version=auth_version+1 WHERE id=$1`, request.PrincipalID); err != nil {
			return err
		}
		for _, table := range []string{"dune_sessions", "dune_enrollments"} {
			if _, err := tx.ExecContext(ctx, "DELETE FROM "+table+" WHERE principal_id=$1", request.PrincipalID); err != nil {
				return err
			}
		}
		now := time.Now().Unix()
		_, err = tx.ExecContext(ctx, `INSERT INTO dune_identity_links(request_id,actor,principal_id,namespace,subject,reason,created_at) VALUES($1,$2,$3,$4,$5,$6,$7)`, request.RequestID, request.Actor, request.PrincipalID, request.Namespace, request.Subject, request.Reason, now)
		result = public.LinkRecord{LinkRequest: request, CreatedAt: time.Unix(now, 0).UTC()}
		return err
	})
	if err != nil {
		if errors.Is(err, ErrConflict) {
			err = public.ErrLinkConflict
		}
		return public.LinkRecord{}, err
	}
	return result, nil
}

type linkReader interface {
	QueryRowContext(context.Context, string, ...any) *sql.Row
}

func readIdentityLink(ctx context.Context, db linkReader, requestID string) (public.LinkRecord, error) {
	var record public.LinkRecord
	var created int64
	err := db.QueryRowContext(ctx, `SELECT request_id,actor,principal_id,namespace,subject,reason,created_at FROM dune_identity_links WHERE request_id=$1`, requestID).Scan(&record.RequestID, &record.Actor, &record.PrincipalID, &record.Namespace, &record.Subject, &record.Reason, &created)
	if errors.Is(err, sql.ErrNoRows) {
		err = public.ErrLinkNotFound
	}
	if err != nil {
		return public.LinkRecord{}, err
	}
	record.CreatedAt = time.Unix(created, 0).UTC()
	return record, nil
}

func (s *Store) IdentityLink(ctx context.Context, requestID string) (public.LinkRecord, error) {
	return readIdentityLink(ctx, s.db, requestID)
}
