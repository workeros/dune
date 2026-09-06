package migrate

import (
	"context"

	"github.com/aiomni/dune/internal/metadata"
	"github.com/aiomni/dune/pkg/storage"
)

// SQLite copies all current metadata into an empty SQLite or PostgreSQL target.
// It also supports offline SQLite backups/restores using separate directories.
// Stop the source application first. Its directory stays locked throughout the
// operation. No source records, identifiers or expiration times are changed.
// The result gives the copied row count for each domain table. A reported unknown
// commit outcome requires inspecting the target before taking any further action.
func SQLite(ctx context.Context, sourceDir string, target storage.Config) (map[string]int64, error) {
	source, err := metadata.OpenSQLiteSource(ctx, sourceDir)
	if err != nil {
		return nil, err
	}
	defer source.Close()
	destination, err := metadata.Open(ctx, target)
	if err != nil {
		return nil, err
	}
	defer destination.Close()
	return source.CopySQLiteTo(ctx, destination)
}
