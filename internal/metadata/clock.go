package metadata

import (
	"context"
	"database/sql"
)

func (s *Store) databaseClock() string {
	if s.postgres {
		return `CAST(EXTRACT(EPOCH FROM clock_timestamp())*1000 AS BIGINT)`
	}
	return `CAST((julianday('now')-2440587.5)*86400000 AS INTEGER)`
}

func (s *Store) databaseNow(ctx context.Context, tx *sql.Tx) (int64, error) {
	var now int64
	err := tx.QueryRowContext(ctx, "SELECT "+s.databaseClock()).Scan(&now)
	return now, err
}
