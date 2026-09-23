package upgradejob

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	"github.com/aiomni/dune/pkg/api"
	"github.com/aiomni/dune/pkg/runner"
	"github.com/aiomni/dune/pkg/upgrade"
)

func (s *Store) Active(ctx context.Context) (*Record, error) {
	record, _, err := scan(s.db.QueryRowContext(ctx, `SELECT data,digest FROM upgrades WHERE active=1`))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &record, nil
}

func (s *Store) List(ctx context.Context, binding runner.Binding, installationID, cursor string, limit int) (upgrade.Page, error) {
	page := upgrade.Page{Items: []upgrade.Operation{}}
	if !binding.Valid() || api.ValidateSubmissionID(installationID) != nil {
		return page, fmt.Errorf("complete upgrade history scope required")
	}

	rows, err := s.db.QueryContext(ctx, `SELECT data,digest,sequence FROM upgrades WHERE expired=0 ORDER BY sequence DESC LIMIT ?`, MaxKeys)
	if err != nil {
		return page, err
	}
	defer rows.Close()
	var operations []upgrade.Operation
	for rows.Next() {
		var body []byte
		var digest string
		var sequence int64
		if err := rows.Scan(&body, &digest, &sequence); err != nil {
			return page, err
		}
		var record Record
		if err := decodeRecord(body, &record); err != nil {
			return page, err
		}
		if !sameScope(record.Operation, binding, installationID) {
			continue
		}
		operations = append(operations, record.Operation)
	}
	if err := rows.Err(); err != nil {
		return page, err
	}
	return upgrade.Paginate(operations, cursor, limit)
}

func sameScope(operation upgrade.Operation, binding runner.Binding, installationID string) bool {
	return operation.Request.Binding == binding && operation.Request.InstallationID == installationID
}

// Prune keeps the newest 128 terminal records and at least 24 hours of details.
// It leaves permanent bounded tombstones for all original keys, and never frees
// key capacity for reuse. Returned IDs are the only eligible recovery materials;
// active/blocked operations never appear here.
func (s *Store) Prune(ctx context.Context, now time.Time) ([]string, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	rows, err := tx.QueryContext(ctx, `SELECT data,digest FROM upgrades WHERE active=0 AND expired=0 ORDER BY sequence DESC LIMIT ? OFFSET 128`, MaxKeys)
	if err != nil {
		return nil, err
	}
	var expired []Record
	for rows.Next() {
		record, _, err := scan(rows)
		if err != nil {
			rows.Close()
			return nil, err
		}
		if record.Operation.UpdatedAt.Before(now.Add(-24 * time.Hour)) {
			expired = append(expired, record)
		}
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return nil, err
	}
	ids := make([]string, 0, len(expired))
	for _, record := range expired {
		original := record.Operation
		revision, err := nextRevision(original.Revision)
		if err != nil {
			return nil, err
		}
		tombstone := Record{Operation: upgrade.Operation{ID: original.ID, Request: original.Request, Revision: revision, Admission: api.SubmissionExpired, Phase: original.Phase, Confirmed: true, StartedAt: original.StartedAt, UpdatedAt: now.UTC()}}
		body, err := encode(tombstone)
		if err != nil {
			return nil, err
		}
		if _, err := tx.ExecContext(ctx, `UPDATE upgrades SET data=?,expired=1,owner='',revision=revision+1 WHERE operation_id=? AND active=0`, body, original.ID); err != nil {
			return nil, err
		}
		ids = append(ids, original.ID)
	}
	return ids, tx.Commit()
}
