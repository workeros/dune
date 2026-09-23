package runnerupgrade

import (
	"context"
	"path/filepath"

	"github.com/aiomni/dune/internal/installation"
	"github.com/aiomni/dune/internal/upgradejob"
	"github.com/aiomni/dune/pkg/upgrade"
)

// Status reads the active or selected local operation even when the host,
// connector or config is unavailable. Filesystem ownership authorizes this CLI
// diagnostic; network users must use the host's authorized public API.
func Status(ctx context.Context, root, operationID string) (upgrade.Operation, error) {
	metadata, err := installation.MetadataAt(root)
	if err != nil {
		return upgrade.Operation{}, err
	}
	jobs, err := upgradejob.OpenReadOnly(ctx, filepath.Join(root, "upgrades"))
	if err != nil {
		return upgrade.Operation{}, err
	}
	defer jobs.Close()
	return jobs.LocalStatus(ctx, metadata.ID, operationID)
}
