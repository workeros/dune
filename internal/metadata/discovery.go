package metadata

import (
	"context"
	"database/sql"
	"errors"
	"strings"
	"time"

	"github.com/aiomni/dune/internal/authorization"
	"github.com/aiomni/dune/internal/identity"
	"github.com/aiomni/dune/internal/wire"
	"github.com/aiomni/dune/pkg/runner"
)

const resourceSelect = `SELECT r.id,r.name,r.kind,r.created_at,r.owner_id,r.fabric_id,r.binding_revision,m.id,m.os,m.arch FROM dune_runners r LEFT JOIN dune_machines m ON m.runner_id=r.id`

func scanResource(row interface{ Scan(...any) error }) (authorization.Resource, error) {
	var resource authorization.Resource
	var binding runner.Binding
	var machine, osName, arch sql.NullString
	err := row.Scan(&resource.Runner.ID, &resource.Runner.Name, &resource.Runner.Kind, &resource.Runner.CreatedAt, &resource.OwnerID, &binding.FabricID, &binding.Revision, &machine, &osName, &arch)
	if err != nil {
		return resource, err
	}
	if machine.Valid {
		binding.RunnerID, binding.MachineID = resource.Runner.ID, machine.String
		resource.Runner.Binding = &binding
	}
	resource.OS, resource.Arch = osName.String, arch.String
	return resource, nil
}
func (s *Store) RunnerResource(ctx context.Context, id string) (authorization.Resource, error) {
	resource, err := scanResource(s.db.QueryRowContext(ctx, resourceSelect+` WHERE r.id=$1`, id))
	if errors.Is(err, sql.ErrNoRows) {
		err = authorization.ErrNotFound
	}
	return resource, err
}
func (s *Store) MachineResource(ctx context.Context, id string) (authorization.Resource, error) {
	resource, err := scanResource(s.db.QueryRowContext(ctx, resourceSelect+` WHERE m.id=$1`, id))
	if errors.Is(err, sql.ErrNoRows) {
		err = authorization.ErrNotFound
	}
	return resource, err
}
func (s *Store) Candidates(ctx context.Context, owner, after string, limit int) ([]authorization.Resource, error) {
	if limit < 1 || limit > 128 {
		return nil, ErrInvalidArgument
	}
	query := resourceSelect + ` WHERE r.id>$1`
	args := []any{after, limit}
	if owner != "" {
		query += ` AND r.owner_id=$3`
		args = append(args, owner)
	}
	rows, err := s.db.QueryContext(ctx, query+` ORDER BY r.id LIMIT $2`, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []authorization.Resource{}
	for rows.Next() {
		r, err := scanResource(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// Cursors contain positions only. They confer no access, are bound to a user and
// discovery operation, and never disclose the IDs of denied candidates.
func (s *Store) ReadCursor(ctx context.Context, principal, namespace, operation, cursor string) (string, error) {
	if !strings.HasPrefix(cursor, "dune_page_") || len(cursor) != 42 {
		return "", ErrInvalidArgument
	}
	var after string
	err := s.db.QueryRowContext(ctx, `SELECT after_id FROM dune_discovery_cursors WHERE id=$1 AND principal_id=$2 AND identity_namespace=$3 AND operation=$4 AND expires_at>$5`, cursor, principal, namespace, operation, time.Now().Unix()).Scan(&after)
	if errors.Is(err, sql.ErrNoRows) {
		err = ErrInvalidArgument
	}
	return after, err
}
func (s *Store) SaveCursor(ctx context.Context, principal, namespace, operation, after string) (string, error) {
	var id string
	err := s.transaction(ctx, func(tx *sql.Tx) error {
		if err := s.lockPrincipal(ctx, tx, principal); err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, `DELETE FROM dune_discovery_cursors WHERE principal_id=$1 AND expires_at<=$2`, principal, time.Now().Unix()); err != nil {
			return err
		}
		err := tx.QueryRowContext(ctx, `SELECT id FROM dune_discovery_cursors WHERE principal_id=$1 AND identity_namespace=$2 AND operation=$3 AND after_id=$4`, principal, namespace, operation, after).Scan(&id)
		if err == nil {
			return nil
		}
		if !errors.Is(err, sql.ErrNoRows) {
			return err
		}
		var count int
		if err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM dune_discovery_cursors WHERE principal_id=$1`, principal).Scan(&count); err != nil {
			return err
		}
		if count >= 64 {
			return identity.ErrLoginLimit
		}
		id = "dune_page_" + wire.ID()
		_, err = tx.ExecContext(ctx, `INSERT INTO dune_discovery_cursors(id,principal_id,identity_namespace,operation,after_id,expires_at) VALUES($1,$2,$3,$4,$5,$6)`, id, principal, namespace, operation, after, time.Now().Add(10*time.Minute).Unix())
		return err
	})
	if err != nil {
		return "", err
	}
	return id, nil
}
