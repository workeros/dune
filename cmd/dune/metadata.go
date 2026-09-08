package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"os"

	"github.com/aiomni/dune/internal/config"
	"github.com/aiomni/dune/internal/metadata"
	"github.com/aiomni/dune/pkg/storage"
)

func runMetadata(ctx context.Context, args []string) error {
	if len(args) > 0 && args[0] == "cluster-recovery" {
		return runClusterRecovery(ctx, args[1:])
	}
	return fmt.Errorf("metadata cluster-recovery --database-config FILE [--rotate-from GENERATION]")
}

func runClusterRecovery(ctx context.Context, args []string) error {
	flags := flag.NewFlagSet("metadata cluster-recovery", flag.ContinueOnError)
	file := flags.String("database-config", "", "private PostgreSQL configuration file")
	expected := flags.String("rotate-from", "", "rotate this exact generation after stopping replicas and restoring the database")
	if err := flags.Parse(args); err != nil {
		return err
	}
	if flags.NArg() != 0 || *file == "" {
		return fmt.Errorf("metadata cluster-recovery --database-config FILE [--rotate-from GENERATION]")
	}
	database, err := config.Database(*file)
	if err != nil {
		return err
	}
	report, err := clusterRecovery(ctx, database, *expected)
	if err == nil || report.Outcome == "unknown" {
		if outputErr := json.NewEncoder(os.Stdout).Encode(report); outputErr != nil {
			return fmt.Errorf("recovery report unavailable; inspect current generation before any retry: %w", outputErr)
		}
	}
	return err
}

type recoveryReport struct {
	Generation string `json:"generation"`
	Outcome    string `json:"outcome"`
}

// clusterRecovery reads the current recovery generation when expected is empty.
// Otherwise it rotates only that exact generation to a fresh random identity.
// Before rotating, stop all cluster replicas and restore/verify the database.
// Deploy the returned generation before restarting. An unknown outcome returns
// the candidate with its error: read again to reconcile; never blindly rotate.
func clusterRecovery(ctx context.Context, database storage.Config, expected string) (recoveryReport, error) {
	if database.Postgres == nil || database.SQLiteDir != "" {
		return recoveryReport{}, fmt.Errorf("cluster recovery requires PostgreSQL")
	}
	store, err := metadata.OpenExistingPostgres(ctx, database.Postgres)
	if err != nil {
		return recoveryReport{}, err
	}
	defer store.Close()
	if expected == "" {
		current, err := store.ConnectionRecovery(ctx)
		return recoveryReport{Generation: current, Outcome: "current"}, err
	}
	generation, err := store.RotateConnectionRecovery(ctx, expected)
	if errors.Is(err, metadata.ErrCommitUnknown) {
		return recoveryReport{Generation: generation, Outcome: "unknown"}, err
	}
	if err != nil {
		return recoveryReport{}, err
	}
	return recoveryReport{Generation: generation, Outcome: "changed"}, nil
}
