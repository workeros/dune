package runnerupgrade

import (
	"context"
	"os"
	"path/filepath"
	"time"

	"github.com/aiomni/dune/internal/installation"
	"github.com/aiomni/dune/internal/launchgate"
	"github.com/aiomni/dune/internal/upgradejob"
	"github.com/aiomni/dune/internal/wire"
)

func cleanupMaterials(root string, installed *installation.Store, record upgradejob.Record) error {
	if !record.Operation.Confirmed || record.Operation.LaunchSealed {
		return issue("UPGRADE_RECOVERY_REQUIRED")
	}
	return removeMaterials(root, installed, upgradejob.Materials{InstallationID: record.Operation.Request.InstallationID, Directories: []string{record.Original.Directory, record.Candidate.Directory}})
}

func removeMaterials(root string, installed *installation.Store, materials upgradejob.Materials) error {
	current, err := installed.Selected()
	if err != nil {
		return err
	}
	if current.Metadata.ID != materials.InstallationID {
		return issue("INSTALLATION_CHANGED")
	}
	for _, directory := range materials.Directories {
		if directory == "" || directory == current.Current.Directory {
			continue
		}
		if filepath.Dir(directory) != "releases" || !wire.ValidID(filepath.Base(directory)) {
			return issue("UPGRADE_MATERIALS_UNVERIFIABLE")
		}
		path := filepath.Join(root, directory)
		if _, err := os.Lstat(path); err == nil {
			if err := launchgate.CheckDirectory(path); err != nil {
				return err
			}
			if err := os.RemoveAll(path); err != nil {
				return err
			}
		} else if !os.IsNotExist(err) {
			return err
		}
		if err := os.Remove(path + ".archive"); err != nil && !os.IsNotExist(err) {
			return err
		}
	}
	return nil
}

// Maintain runs after recovery, under the same installation lock. Only durable
// terminal records authorize release cleanup; active/blocked work is untouched.
func Maintain(ctx context.Context, root string) error {
	installed, err := installation.Lock(root)
	if err != nil {
		return err
	}
	defer installed.Close()
	jobs, err := upgradejob.Open(ctx, filepath.Join(root, "upgrades"))
	if err != nil {
		return err
	}
	defer jobs.Close()
	active, err := jobs.Active(ctx)
	if err != nil || active != nil {
		return err
	}
	records, err := jobs.Cleanable(ctx)
	if err != nil {
		return err
	}
	for _, record := range records {
		if err := ctx.Err(); err != nil {
			return err
		}
		if err := removeMaterials(root, installed, record); err != nil {
			return err
		}
	}
	_, err = jobs.Prune(ctx, time.Now().UTC())
	return err
}
