package metadata

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/aiomni/dune/pkg/workbench"
)

const projectColumns = `id,owner_id,revision,spec,created_at,updated_at`

func scanProject(row interface{ Scan(...any) error }) (workbench.Project, error) {
	var project workbench.Project
	var payload string
	var created, updated int64
	err := row.Scan(&project.ID, &project.OwnerID, &project.Revision, &payload, &created, &updated)
	if errors.Is(err, sql.ErrNoRows) {
		return project, ErrNotFound
	}
	if err != nil {
		return project, err
	}
	project.CreatedAt, project.UpdatedAt = time.UnixMicro(created).UTC(), time.UnixMicro(updated).UTC()
	err = json.Unmarshal([]byte(payload), &project.ProjectSpec)
	return project, err
}

// Projects returns a bounded, owner-scoped page ordered by ID. The caller
// authorizes the Owner (a personal account or Tenant) before entering the store.
func (s *Store) Projects(ctx context.Context, owner, after string, limit int) ([]workbench.Project, error) {
	if owner == "" || len(after) > 256 || limit < 1 || limit > 101 {
		return nil, ErrInvalidArgument
	}
	rows, err := s.db.QueryContext(ctx, `SELECT `+projectColumns+` FROM dune_projects WHERE owner_id=$1 AND id>$2 ORDER BY id LIMIT $3`, owner, after, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	projects := []workbench.Project{}
	for rows.Next() {
		project, err := scanProject(rows)
		if err != nil {
			return nil, err
		}
		projects = append(projects, project)
	}
	return projects, rows.Err()
}

func (s *Store) Project(ctx context.Context, owner, id string) (workbench.Project, error) {
	return scanProject(s.db.QueryRowContext(ctx, `SELECT `+projectColumns+` FROM dune_projects WHERE owner_id=$1 AND id=$2`, owner, id))
}

// SaveProject creates when id is empty, otherwise updates exactly the supplied
// revision. Unknown commit outcomes are not retried.
func (s *Store) SaveProject(ctx context.Context, owner, id string, revision int64, spec workbench.ProjectSpec) (workbench.Project, error) {
	if owner == "" || len(owner) > 256 || (id == "" && revision != 0) || (id != "" && revision < 1) {
		return workbench.Project{}, ErrInvalidArgument
	}
	if err := spec.Validate(); err != nil {
		return workbench.Project{}, fmt.Errorf("%w: %s", ErrInvalidArgument, err)
	}
	if spec.Directories == nil {
		spec.Directories = []workbench.Directory{}
	}
	payload, err := json.Marshal(spec)
	if err != nil {
		return workbench.Project{}, err
	}
	now := time.Now().UTC().Truncate(time.Microsecond)
	project := workbench.Project{ID: id, OwnerID: owner, Revision: revision + 1, ProjectSpec: spec, CreatedAt: now, UpdatedAt: now}
	err = s.transaction(ctx, func(tx *sql.Tx) error {
		if id == "" {
			project.ID = "project_" + rand.Text()
			_, err := tx.ExecContext(ctx, `INSERT INTO dune_projects(id,owner_id,revision,spec,created_at,updated_at) VALUES($1,$2,1,$3,$4,$4)`, project.ID, owner, string(payload), now.UnixMicro())
			return err
		}
		previous, err := scanProject(tx.QueryRowContext(ctx, `SELECT `+projectColumns+` FROM dune_projects WHERE owner_id=$1 AND id=$2`, owner, id))
		if err != nil {
			return err
		}
		project.CreatedAt = previous.CreatedAt
		result, err := tx.ExecContext(ctx, `UPDATE dune_projects SET revision=revision+1,spec=$1,updated_at=$2 WHERE owner_id=$3 AND id=$4 AND revision=$5`, string(payload), now.UnixMicro(), owner, id, revision)
		return changedRow(result, err)
	})
	return project, err
}

// DeleteProject removes organization metadata only, never files or Runtimes.
func (s *Store) DeleteProject(ctx context.Context, owner, id string, revision int64) error {
	if owner == "" || id == "" || revision < 1 {
		return ErrInvalidArgument
	}
	return s.transaction(ctx, func(tx *sql.Tx) error {
		if _, err := scanProject(tx.QueryRowContext(ctx, `SELECT `+projectColumns+` FROM dune_projects WHERE owner_id=$1 AND id=$2`, owner, id)); err != nil {
			return err
		}
		result, err := tx.ExecContext(ctx, `DELETE FROM dune_projects WHERE owner_id=$1 AND id=$2 AND revision=$3`, owner, id, revision)
		return changedRow(result, err)
	})
}

func changedRow(result sql.Result, err error) error {
	if err != nil {
		return err
	}
	count, err := result.RowsAffected()
	if err == nil && count != 1 {
		return ErrConflict
	}
	return err
}
