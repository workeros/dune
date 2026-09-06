package migrate

import (
	"context"
	"errors"
	"fmt"

	"github.com/aiomni/dune/internal/metadata"
	"github.com/aiomni/dune/pkg/storage"
)

type RecoveryReport struct {
	Generation string `json:"generation"`
	Outcome    string `json:"outcome"`
}

// ClusterRecovery reads the current recovery generation when expected is empty.
// Otherwise it rotates only that exact generation to a fresh random identity.
// Before rotating, stop all cluster replicas and restore/verify the database.
// Deploy the returned generation before restarting. An unknown outcome returns
// the candidate with its error: read again to reconcile; never blindly rotate.
func ClusterRecovery(ctx context.Context, database storage.Config, expected string) (RecoveryReport, error) {
	if database.Postgres == nil || database.SQLiteDir != "" {
		return RecoveryReport{}, fmt.Errorf("cluster recovery requires PostgreSQL")
	}
	store, err := metadata.OpenPostgresSource(ctx, database.Postgres)
	if err != nil {
		return RecoveryReport{}, err
	}
	defer store.Close()
	if expected == "" {
		current, err := store.ConnectionRecovery(ctx)
		return RecoveryReport{Generation: current, Outcome: "current"}, err
	}
	generation, err := store.RotateConnectionRecovery(ctx, expected)
	if errors.Is(err, metadata.ErrCommitUnknown) {
		return RecoveryReport{Generation: generation, Outcome: "unknown"}, err
	}
	if err != nil {
		return RecoveryReport{}, err
	}
	return RecoveryReport{Generation: generation, Outcome: "changed"}, nil
}
