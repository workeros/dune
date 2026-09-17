// Package profiles stores reusable, versioned execution Profiles on the server.
// The embedding application authorizes access to each owner before calling this
// package. Owner IDs can represent personal accounts or enterprise spaces.
// Runners receive complete api.Profile payloads and never access this store.
package profiles

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"
	"unicode"

	"github.com/aiomni/dune/pkg/api"
)

var (
	ErrInvalid       = errors.New("invalid Profile")
	ErrNotFound      = errors.New("Profile not found")
	ErrConflict      = errors.New("Profile revision conflict")
	ErrCommitUnknown = errors.New("Profile commit outcome unknown; do not automatically replay")
)

type Selection struct {
	ID       string `json:"id"`
	Revision int64  `json:"revision"`
}

// Actor records authorship, not an authorization grant or a login credential.
type Actor struct {
	Type    string `json:"type"`
	Subject string `json:"subject"`
}

type Record struct {
	ID          string      `json:"id"`
	OwnerID     string      `json:"owner_id"`
	Name        string      `json:"name"`
	Description string      `json:"description"`
	Revision    int64       `json:"revision"`
	Profile     api.Profile `json:"profile"`
	CreatedBy   Actor       `json:"created_by"`
	CreatedAt   time.Time   `json:"created_at"`
	UpdatedAt   time.Time   `json:"updated_at"`
}

type queries interface {
	QueryContext(context.Context, string, ...any) (*sql.Rows, error)
	QueryRowContext(context.Context, string, ...any) *sql.Row
}

// Store borrows an application-owned SQLite or PostgreSQL pool. The application
// initializes the schema before use and retains responsibility for closing it.
type Store struct {
	db      *sql.DB
	tx      *sql.Tx
	queries queries
}

func NewStore(db *sql.DB) *Store { return &Store{db: db, queries: db} }

// NewTransaction joins a host-owned transaction, for example to atomically
// authorize a space write and create its Profile. The host commits or rolls back.
func NewTransaction(tx *sql.Tx) *Store { return &Store{tx: tx, queries: tx} }

// CreateSchema participates in the host's initialization transaction. It creates
// the current format only; there is no schema migration or legacy import.
func CreateSchema(ctx context.Context, tx *sql.Tx) error {
	for _, statement := range []string{
		`CREATE TABLE IF NOT EXISTS dune_profiles (id TEXT PRIMARY KEY,owner_id TEXT NOT NULL,kind TEXT NOT NULL CHECK(kind IN ('environment','agent')),revision BIGINT NOT NULL CHECK(revision>0),created_by_type TEXT NOT NULL,created_by_subject TEXT NOT NULL,created_at BIGINT NOT NULL,updated_at BIGINT NOT NULL)`,
		`CREATE INDEX IF NOT EXISTS dune_profiles_owner ON dune_profiles(owner_id,id)`,
		`CREATE TABLE IF NOT EXISTS dune_profile_revisions (profile_id TEXT NOT NULL REFERENCES dune_profiles(id) ON DELETE CASCADE,revision BIGINT NOT NULL CHECK(revision>0),name TEXT NOT NULL,description TEXT NOT NULL,profile TEXT NOT NULL,created_at BIGINT NOT NULL,PRIMARY KEY(profile_id,revision))`,
	} {
		if _, err := tx.ExecContext(ctx, statement); err != nil {
			return err
		}
	}
	return nil
}

func validText(value string, limit int) bool {
	return strings.TrimSpace(value) != "" && len(value) <= limit && !strings.ContainsFunc(value, unicode.IsControl)
}

func validate(record *Record) error {
	if record.Profile.Setup.Steps == nil {
		record.Profile.Setup.Steps = []api.Command{}
	}
	if record.Profile.Env == nil {
		record.Profile.Env = map[string]string{}
	}
	if !validText(record.OwnerID, 256) {
		return fmt.Errorf("%w: owner is required", ErrInvalid)
	}
	record.Name = strings.TrimSpace(record.Name)
	record.Description = strings.TrimSpace(record.Description)
	if !validText(record.Name, 120) || len(record.Description) > 1024 {
		return fmt.Errorf("%w: name or description", ErrInvalid)
	}
	if err := record.Profile.Validate(); err != nil {
		return fmt.Errorf("%w: %s", ErrInvalid, err)
	}
	return nil
}

const selectRecord = `SELECT p.id,p.owner_id,r.name,r.description,r.revision,r.profile,p.created_by_type,p.created_by_subject,p.created_at,r.created_at FROM dune_profiles p JOIN dune_profile_revisions r ON r.profile_id=p.id`

func scanRecord(row interface{ Scan(...any) error }) (Record, error) {
	var record Record
	var payload string
	var created, updated int64
	err := row.Scan(&record.ID, &record.OwnerID, &record.Name, &record.Description, &record.Revision, &payload, &record.CreatedBy.Type, &record.CreatedBy.Subject, &created, &updated)
	if errors.Is(err, sql.ErrNoRows) {
		return record, ErrNotFound
	}
	if err != nil {
		return Record{}, err
	}
	record.CreatedAt, record.UpdatedAt = time.UnixMicro(created).UTC(), time.UnixMicro(updated).UTC()
	if err := json.Unmarshal([]byte(payload), &record.Profile); err != nil {
		return Record{}, errors.New("cannot read saved Profile")
	}
	return record, nil
}

func (s *Store) List(ctx context.Context, ownerID string) ([]Record, error) {
	if !validText(ownerID, 256) {
		return nil, ErrInvalid
	}
	rows, err := s.queries.QueryContext(ctx, selectRecord+` WHERE p.owner_id=$1 AND r.revision=p.revision ORDER BY r.name,p.id`, ownerID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	items := []Record{}
	for rows.Next() {
		record, err := scanRecord(rows)
		if err != nil {
			return nil, err
		}
		items = append(items, record)
	}
	return items, rows.Err()
}

// ListAgentPage bounds catalog discovery for tools without reading all saved
// Profile revisions. The caller projects the private records to safe summaries.
func (s *Store) ListAgentPage(ctx context.Context, ownerID, after string, limit int) ([]Record, error) {
	if !validText(ownerID, 256) || len(after) > 256 || limit < 1 || limit > 101 {
		return nil, ErrInvalid
	}
	rows, err := s.queries.QueryContext(ctx, selectRecord+` WHERE p.owner_id=$1 AND p.kind='agent' AND p.id>$2 AND r.revision=p.revision ORDER BY p.id LIMIT $3`, ownerID, after, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	items := []Record{}
	for rows.Next() {
		record, err := scanRecord(rows)
		if err != nil {
			return nil, err
		}
		items = append(items, record)
	}
	return items, rows.Err()
}

// Get accepts revision zero for editing the latest version. Executions should
// resolve a positive revision to keep a concurrent edit from changing input.
func (s *Store) Get(ctx context.Context, ownerID string, selection Selection) (Record, error) {
	if !validText(ownerID, 256) || !validText(selection.ID, 256) || selection.Revision < 0 {
		return Record{}, ErrInvalid
	}
	return scanRecord(s.queries.QueryRowContext(ctx, selectRecord+` WHERE p.owner_id=$1 AND p.id=$2 AND r.revision=CASE WHEN $3=0 THEN p.revision ELSE $3 END`, ownerID, selection.ID, selection.Revision))
}

func (s *Store) Create(ctx context.Context, record Record) (Record, error) {
	if err := validate(&record); err != nil {
		return Record{}, err
	}
	if !validText(record.CreatedBy.Type, 256) || !validText(record.CreatedBy.Subject, 256) {
		return Record{}, fmt.Errorf("%w: creator is required", ErrInvalid)
	}
	record.ID, record.Revision = "profile_"+rand.Text(), 1
	record.CreatedAt = time.Now().UTC().Truncate(time.Microsecond)
	record.UpdatedAt = record.CreatedAt
	err := s.transaction(ctx, func(tx *sql.Tx) error {
		_, err := tx.ExecContext(ctx, `INSERT INTO dune_profiles(id,owner_id,kind,revision,created_by_type,created_by_subject,created_at,updated_at) VALUES($1,$2,$3,1,$4,$5,$6,$6)`, record.ID, record.OwnerID, record.Profile.Kind, record.CreatedBy.Type, record.CreatedBy.Subject, record.CreatedAt.UnixMicro())
		if err != nil {
			return err
		}
		return insertRevision(ctx, tx, record)
	})
	if err != nil {
		return Record{}, err
	}
	return record, nil
}

func insertRevision(ctx context.Context, tx *sql.Tx, record Record) error {
	payload, err := json.Marshal(record.Profile)
	if err != nil {
		return err
	}
	_, err = tx.ExecContext(ctx, `INSERT INTO dune_profile_revisions(profile_id,revision,name,description,profile,created_at) VALUES($1,$2,$3,$4,$5,$6)`, record.ID, record.Revision, record.Name, record.Description, string(payload), record.UpdatedAt.UnixMicro())
	return err
}

func (s *Store) Update(ctx context.Context, record Record) (Record, error) {
	if err := validate(&record); err != nil {
		return Record{}, err
	}
	if record.Revision < 1 {
		return Record{}, ErrInvalid
	}
	current, err := s.Get(ctx, record.OwnerID, Selection{ID: record.ID})
	if err != nil {
		return Record{}, err
	}
	if record.Profile.Kind != current.Profile.Kind {
		return Record{}, fmt.Errorf("%w: Profile kind cannot change", ErrInvalid)
	}
	record.CreatedBy, record.CreatedAt = current.CreatedBy, current.CreatedAt
	record.UpdatedAt = time.Now().UTC().Truncate(time.Microsecond)
	err = s.transaction(ctx, func(tx *sql.Tx) error {
		result, err := tx.ExecContext(ctx, `UPDATE dune_profiles SET revision=revision+1,updated_at=$4 WHERE owner_id=$1 AND id=$2 AND revision=$3`, record.OwnerID, record.ID, record.Revision, record.UpdatedAt.UnixMicro())
		if err != nil {
			return err
		}
		if err := requireChange(result); err != nil {
			return err
		}
		record.Revision++
		return insertRevision(ctx, tx, record)
	})
	if err != nil {
		return Record{}, err
	}
	return record, nil
}

func requireChange(result sql.Result) error {
	count, err := result.RowsAffected()
	if err != nil {
		return err
	}
	if count != 1 {
		return ErrConflict
	}
	return nil
}

func (s *Store) Delete(ctx context.Context, ownerID string, selection Selection) error {
	if selection.Revision < 1 {
		return ErrInvalid
	}
	if _, err := s.Get(ctx, ownerID, Selection{ID: selection.ID}); err != nil {
		return err
	}
	return s.transaction(ctx, func(tx *sql.Tx) error {
		result, err := tx.ExecContext(ctx, `DELETE FROM dune_profiles WHERE owner_id=$1 AND id=$2 AND revision=$3`, ownerID, selection.ID, selection.Revision)
		if err != nil {
			return err
		}
		return requireChange(result)
	})
}

// DeleteOwner lets an authorized host remove a space and its Profiles in the
// same SQL transaction. The host must serialize this with new space writes.
func DeleteOwner(ctx context.Context, tx *sql.Tx, ownerID string) error {
	if !validText(ownerID, 256) {
		return ErrInvalid
	}
	_, err := tx.ExecContext(ctx, `DELETE FROM dune_profiles WHERE owner_id=$1`, ownerID)
	return err
}

func (s *Store) transaction(ctx context.Context, change func(*sql.Tx) error) error {
	if s.tx != nil {
		return change(s.tx)
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if err := change(tx); err != nil {
		return err
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("%w: %v", ErrCommitUnknown, err)
	}
	return nil
}
