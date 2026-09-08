package metadata

import (
	"context"
	"database/sql"
	"time"

	"github.com/aiomni/dune/internal/lifecycle"
	"github.com/aiomni/dune/internal/wire"
)

const managedEnrollmentMaxLifetime = 10 * time.Minute

// BeginManagedBootstrap atomically reserves the provider action and a
// resource-bound, one-time enrollment. Dispatch is true only after both rows
// committed. A repeated call returns the original action without the secret and
// permits reconciliation only; an unknown commit returns no action or token.
func (s *Store) BeginManagedBootstrap(ctx context.Context, expected lifecycle.Operation, digest string, lifetime time.Duration) (grant lifecycle.BootstrapGrant, dispatch bool, err error) {
	if !peerHash(digest) || lifetime < time.Second || lifetime > managedEnrollmentMaxLifetime {
		return lifecycle.BootstrapGrant{}, false, ErrInvalidArgument
	}
	token := wire.ID() + wire.ID()
	request := lifecycle.ActionRequest{Kind: "bootstrap", Digest: digest}
	err = s.withOperation(ctx, expected.ID, func(tx *sql.Tx, current lifecycle.Operation, now int64) error {
		if !ownsOperation(current, expected, now) {
			return lifecycle.ErrLeaseLost
		}
		action, first, err := s.beginProviderAction(ctx, tx, current, request, now)
		if err != nil {
			return err
		}
		grant.Action = action
		if !first {
			return nil
		}
		expires := now + lifetime.Milliseconds()
		result, err := tx.ExecContext(ctx, `INSERT INTO dune_managed_enrollments(hash,action_id,operation_id,runner_id,fabric_id,binding_revision,resource_ref,expires_at)
			SELECT $1,$2,id,runner_id,fabric_id,binding_revision,$3,$4 FROM dune_operations
			WHERE id=$5 AND worker=$6 AND execution_revision=$7 AND lease_until>`+s.databaseClock(), tokenHash(token), action.ID, action.ResourceRef, expires, current.ID, current.Worker, current.Revision)
		if err != nil {
			return err
		}
		n, err := result.RowsAffected()
		if err != nil {
			return err
		}
		if n != 1 {
			return lifecycle.ErrLeaseLost
		}
		grant.Token = token
		grant.ExpiresAt = time.UnixMilli(expires).UTC()
		dispatch = true
		return nil
	})
	if err != nil {
		return lifecycle.BootstrapGrant{}, false, err
	}
	return grant, dispatch, nil
}
