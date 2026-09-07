package metadata

import (
	"context"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"time"

	"github.com/aiomni/dune/internal/authorization"
	"github.com/aiomni/dune/internal/identity"
	"github.com/aiomni/dune/internal/wire"
)

func peerHash(value string) bool {
	decoded, err := hex.DecodeString(value)
	return err == nil && len(decoded) == 32 && hex.EncodeToString(decoded) == value
}

func validPeerReference(r authorization.PeerReference) bool {
	return wire.ValidID(r.SourceBootID) && wire.ValidID(r.OwnerBootID) && r.SourceBootID != r.OwnerBootID && r.Target != "" && peerHash(r.RequestDigest)
}

// CreatePeerAccess persists only a digest and bounded selection metadata. Its
// capacity is shared by all entry instances for the original human session.
func (s *Store) CreatePeerAccess(ctx context.Context, hash string, r authorization.PeerAccess, ttl time.Duration) error {
	started := time.Now()
	if !peerHash(hash) || !validPeerReference(r.PeerReference) || ttl <= 0 || ttl > authorization.TicketLifetime || r.Connection.Target != r.Target || r.Connection.Namespace != r.Namespace || r.Request.Scope != r.Connection.Scope() || !r.Request.Binding.Valid() || r.Request.RequestID == "" || r.Request.Operation == "" || r.DecisionID == "" || len(r.DecisionID) > 128 {
		return ErrInvalidArgument
	}
	data, err := json.Marshal(r)
	if err != nil || len(data) > 16384 {
		return ErrInvalidArgument
	}
	return s.transaction(ctx, func(tx *sql.Tx) error {
		if err := s.lockPrincipal(ctx, tx, r.Connection.PrincipalID); err != nil {
			return err
		}
		if s.postgres {
			// Match normal ticket issuance's parent-first lock order so logout's
			// or machine revocation's cascade cannot deadlock with cleanup/insert.
			var session string
			err := tx.QueryRowContext(ctx, `SELECT hash FROM dune_sessions WHERE hash=$1 AND principal_id=$2 FOR KEY SHARE`, r.Connection.SessionHash, r.Connection.PrincipalID).Scan(&session)
			if errors.Is(err, sql.ErrNoRows) {
				return identity.ErrUnauthorized
			}
			if err != nil {
				return err
			}
			var runner, machine string
			err = tx.QueryRowContext(ctx, `SELECT id FROM dune_runners WHERE id=$1 AND fabric_id=$2 AND binding_revision=$3 FOR SHARE`, r.Connection.RunnerID, r.Connection.FabricID, r.Connection.BindingRevision).Scan(&runner)
			if errors.Is(err, sql.ErrNoRows) {
				return identity.ErrUnauthorized
			}
			if err != nil {
				return err
			}
			err = tx.QueryRowContext(ctx, `SELECT id FROM dune_machines WHERE id=$1 AND runner_id=$2 FOR KEY SHARE`, r.Target, runner).Scan(&machine)
			if errors.Is(err, sql.ErrNoRows) {
				return identity.ErrUnauthorized
			}
			if err != nil {
				return err
			}
		}
		now, err := s.databaseNow(ctx, tx)
		if err != nil {
			return err
		}
		valid, err := checkAccess(ctx, tx, r.Connection, now/1000)
		if err != nil {
			return err
		}
		if !valid {
			return identity.ErrUnauthorized
		}
		if _, err := tx.ExecContext(ctx, `DELETE FROM dune_peer_access WHERE session_hash=$1 AND expires_at<=$2`, r.Connection.SessionHash, now); err != nil {
			return err
		}
		var count int
		if err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM dune_peer_access WHERE session_hash=$1`, r.Connection.SessionHash).Scan(&count); err != nil {
			return err
		}
		if count >= 64 {
			return identity.ErrLoginLimit
		}
		remaining := (ttl - time.Since(started)).Milliseconds()
		if remaining <= 0 {
			return identity.ErrUnauthorized
		}
		_, err = tx.ExecContext(ctx, `INSERT INTO dune_peer_access(hash,session_hash,machine_id,source_boot_id,owner_boot_id,identity_namespace,request_digest,expires_at,context) VALUES($1,$2,$3,$4,$5,$6,$7,$8,$9)`, hash, r.Connection.SessionHash, r.Target, r.SourceBootID, r.OwnerBootID, r.Namespace, r.RequestDigest, now+remaining, string(data))
		return err
	})
}

// Consumption binds the confidential transport's identities and original
// request. A mismatch does not consume someone else's capability. A committed
// deletion whose acknowledgement is lost is never retried or treated as success.
func (s *Store) ConsumePeerAccess(ctx context.Context, hash string, ref authorization.PeerReference) (authorization.PeerAccess, error) {
	var r authorization.PeerAccess
	if !peerHash(hash) || !validPeerReference(ref) {
		return r, identity.ErrUnauthorized
	}
	err := s.transaction(ctx, func(tx *sql.Tx) error {
		var data string
		err := tx.QueryRowContext(ctx, `DELETE FROM dune_peer_access WHERE hash=$1 AND source_boot_id=$2 AND owner_boot_id=$3 AND machine_id=$4 AND identity_namespace=$5 AND request_digest=$6 AND expires_at>`+s.databaseClock()+` RETURNING context`, hash, ref.SourceBootID, ref.OwnerBootID, ref.Target, ref.Namespace, ref.RequestDigest).Scan(&data)
		if errors.Is(err, sql.ErrNoRows) {
			return identity.ErrUnauthorized
		}
		if err != nil {
			return err
		}
		if len(data) > 16384 || json.Unmarshal([]byte(data), &r) != nil || r.PeerReference != ref || r.Request.Scope != r.Connection.Scope() || r.Connection.Target != ref.Target || r.Connection.Namespace != ref.Namespace {
			return identity.ErrUnauthorized
		}
		return nil
	})
	if err != nil {
		return authorization.PeerAccess{}, err
	}
	return r, nil
}
