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
	if len(args) == 0 || args[0] != "import-json" {
		return fmt.Errorf("metadata import-json --source LEGACY_DIR [--data SQLITE_DIR | --database-config FILE]")
	}
	flags := flag.NewFlagSet("metadata import-json", flag.ContinueOnError)
	source := flags.String("source", "", "offline legacy account directory containing accounts.json")
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
	report, err := migrate.JSON(ctx, dir, target)
	if err != nil {
		return err
	}
	return json.NewEncoder(os.Stdout).Encode(report)
}
