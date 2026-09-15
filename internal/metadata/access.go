package metadata

import (
	"context"
	"database/sql"
	"errors"

	"github.com/aiomni/dune/internal/authorization"
)

// CheckRunnerAccess validates only Dune-owned binding state. Browser session
// validity is checked separately through identity.Service; trusted background
// calls instead rely on the host-supplied principal at admission.
func (s *Store) CheckRunnerAccess(ctx context.Context, record authorization.ConnectionAccess) (bool, error) {
	var found int
	err := s.db.QueryRowContext(ctx, `SELECT 1 FROM dune_runners WHERE machine_id=$1 AND owner_id=$2 AND id=$3 AND fabric_id=$4 AND binding_revision=$5 AND enabled=TRUE AND suspended=FALSE`, record.Target, record.OwnerID, record.RunnerID, record.FabricID, record.BindingRevision).Scan(&found)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	return err == nil, err
}
