package metadata

import (
	"context"
	"database/sql"
	"errors"

	"github.com/aiomni/dune/pkg/runner"
)

const runnerSelect = `SELECT r.id,r.name,r.kind,r.created_at,r.fabric_id,r.binding_revision,m.id FROM dune_runners r LEFT JOIN dune_machines m ON m.runner_id=r.id`

func scanRunner(row interface{ Scan(...any) error }) (runner.Runner, error) {
	var out runner.Runner
	var binding runner.Binding
	var machine sql.NullString
	if err := row.Scan(&out.ID, &out.Name, &out.Kind, &out.CreatedAt, &binding.FabricID, &binding.Revision, &machine); err != nil {
		return out, err
	}
	if machine.Valid {
		binding.RunnerID, binding.MachineID = out.ID, machine.String
		out.Binding = &binding
	}
	return out, nil
}

// Runners is the default owner discovery. The per-owner Runner limit bounds
// this result; enterprise candidate scanning is a separate authorization path.
func (s *Store) Runners(ctx context.Context, owner string) ([]runner.Runner, error) {
	rows, err := s.db.QueryContext(ctx, runnerSelect+` WHERE r.owner_id=$1 ORDER BY r.created_at,r.id`, owner)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []runner.Runner{}
	for rows.Next() {
		r, err := scanRunner(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

func (s *Store) Runner(ctx context.Context, owner, id string) (runner.Runner, error) {
	out, err := scanRunner(s.db.QueryRowContext(ctx, runnerSelect+` WHERE r.owner_id=$1 AND r.id=$2`, owner, id))
	if errors.Is(err, sql.ErrNoRows) {
		err = ErrNotFound
	}
	return out, err
}
