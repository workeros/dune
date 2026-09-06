package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"path/filepath"

	"github.com/aiomni/dune/internal/config"
	"github.com/aiomni/dune/pkg/migrate"
	"github.com/aiomni/dune/pkg/storage"
)

func runMetadata(ctx context.Context, args []string) error {
	if len(args) > 0 && args[0] == "cluster-recovery" {
		return runClusterRecovery(ctx, args[1:])
	}
	if len(args) == 0 || (args[0] != "import-json" && args[0] != "copy-sqlite") {
		return fmt.Errorf("metadata import-json|copy-sqlite --source DIR [--data SQLITE_DIR | --database-config FILE]")
	}
	flags := flag.NewFlagSet("metadata "+args[0], flag.ContinueOnError)
	source := flags.String("source", "", "offline source directory")
	data := flags.String("data", "", "empty target SQLite directory (separate from source)")
	database := flags.String("database-config", "", "private SQL target configuration file")
	if err := flags.Parse(args[1:]); err != nil {
		return err
	}
	if flags.NArg() != 0 || *source == "" || (*data == "") == (*database == "") {
		return fmt.Errorf("source and exactly one SQL target required")
	}
	dir, err := filepath.Abs(*source)
	if err != nil {
		return err
	}
	var target storage.Config
	if *database != "" {
		target, err = config.Database(*database)
	} else {
		target.SQLiteDir, err = filepath.Abs(*data)
	}
	if err != nil {
		return err
	}
	var report any
	if args[0] == "copy-sqlite" {
		report, err = migrate.SQLite(ctx, dir, target)
	} else {
		report, err = migrate.JSON(ctx, dir, target)
	}
	if err != nil {
		return err
	}
	return json.NewEncoder(os.Stdout).Encode(report)
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
	report, err := migrate.ClusterRecovery(ctx, database, *expected)
	if err == nil || report.Outcome == "unknown" {
		if outputErr := json.NewEncoder(os.Stdout).Encode(report); outputErr != nil {
			return fmt.Errorf("recovery report unavailable; inspect current generation before any retry: %w", outputErr)
		}
	}
	return err
}
