// Package migrate provides offline, explicit metadata migration operations.
// JSON is an import format only; it is never a selectable runtime backend.
package migrate

import (
	"context"

	"github.com/aiomni/dune/internal/metadata"
	"github.com/aiomni/dune/pkg/storage"
)

type Report struct {
	Accounts    int `json:"accounts"`
	Sessions    int `json:"sessions"`
	Enrollments int `json:"enrollments"`
	Machines    int `json:"machines"`
}

// JSON imports sourceDir/accounts.json into an empty SQL target. The source
// must be offline: Dune holds its original account-directory lock throughout
// validation and import. Source files and original identities are preserved.
// SQLite targets must use a separate private directory. On error no records are
// committed unless the error explicitly reports an unknown commit outcome.
func JSON(ctx context.Context, sourceDir string, target storage.Config) (Report, error) {
	source, err := metadata.ReadLegacy(ctx, sourceDir)
	if err != nil {
		return Report{}, err
	}
	defer source.Close()
	destination, err := metadata.Open(ctx, target)
	if err != nil {
		return Report{}, err
	}
	defer destination.Close()
	counts, err := destination.ImportLegacy(ctx, source)
	if err != nil {
		return Report{}, err
	}
	return Report{Accounts: counts.Accounts, Sessions: counts.Sessions, Enrollments: counts.Enrollments, Machines: counts.Machines}, nil
}
