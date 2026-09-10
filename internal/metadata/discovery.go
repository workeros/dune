package metadata

import (
	"context"
	"database/sql"
	"encoding/base64"
	"errors"

	"github.com/aiomni/dune/internal/authorization"
	"github.com/aiomni/dune/internal/wire"
	"github.com/aiomni/dune/pkg/runner"
)

const resourceSelect = `SELECT id,name,kind,created_at,owner_id,fabric_id,binding_revision,machine_id,os,arch FROM dune_runners`

func scanResource(row interface{ Scan(...any) error }) (authorization.Resource, error) {
	var resource authorization.Resource
	var binding runner.Binding
	var machine sql.NullString
	if err := row.Scan(&resource.Runner.ID, &resource.Runner.Name, &resource.Runner.Kind, &resource.Runner.CreatedAt, &resource.OwnerID, &binding.FabricID, &binding.Revision, &machine, &resource.OS, &resource.Arch); err != nil {
		return resource, err
	}
	if machine.Valid {
		binding.RunnerID, binding.MachineID = resource.Runner.ID, machine.String
		resource.Runner.Binding = &binding
	}
	resource.FabricID, resource.BindingRevision = binding.FabricID, binding.Revision
	return resource, nil
}

func (s *Store) RunnerResource(ctx context.Context, id string) (authorization.Resource, error) {
	resource, err := scanResource(s.db.QueryRowContext(ctx, resourceSelect+` WHERE id=$1 AND enabled=TRUE`, id))
	if errors.Is(err, sql.ErrNoRows) {
		err = authorization.ErrNotFound
	}
	return resource, err
}

func (s *Store) MachineResource(ctx context.Context, id string) (authorization.Resource, error) {
	resource, err := scanResource(s.db.QueryRowContext(ctx, resourceSelect+` WHERE machine_id=$1 AND enabled=TRUE AND suspended=FALSE`, id))
	if errors.Is(err, sql.ErrNoRows) {
		err = authorization.ErrNotFound
	}
	return resource, err
}

func (s *Store) Candidates(ctx context.Context, owner, after string, limit int) ([]authorization.Resource, error) {
	if limit < 1 || limit > 128 {
		return nil, ErrInvalidArgument
	}
	query := resourceSelect + ` WHERE id>$1 AND enabled=TRUE`
	args := []any{after, limit}
	if owner != "" {
		query += ` AND owner_id=$3`
		args = append(args, owner)
	}
	rows, err := s.db.QueryContext(ctx, query+` ORDER BY id LIMIT $2`, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []authorization.Resource{}
	for rows.Next() {
		resource, err := scanResource(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, resource)
	}
	return out, rows.Err()
}

// Cursors are stateless positions. They carry no authority; every item after
// the position is still checked against the current user and policy.
func (s *Store) ReadCursor(_ context.Context, _, _, _, cursor string) (string, error) {
	const prefix = "dune_page_"
	if len(cursor) <= len(prefix) || cursor[:len(prefix)] != prefix {
		return "", ErrInvalidArgument
	}
	data, err := base64.RawURLEncoding.DecodeString(cursor[len(prefix):])
	if err != nil || !wire.ValidID(string(data)) {
		return "", ErrInvalidArgument
	}
	return string(data), nil
}

func (s *Store) SaveCursor(_ context.Context, _, _, _, after string) (string, error) {
	if !wire.ValidID(after) {
		return "", ErrInvalidArgument
	}
	return "dune_page_" + base64.RawURLEncoding.EncodeToString([]byte(after)), nil
}
