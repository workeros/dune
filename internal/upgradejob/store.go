// Package upgradejob persists original upgrade admission and fenced execution
// evidence independently of connector and browser lifetimes.
package upgradejob

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"syscall"
	"time"

	"github.com/aiomni/dune/internal/installation"
	"github.com/aiomni/dune/internal/launchgate"
	"github.com/aiomni/dune/internal/statecontract"
	"github.com/aiomni/dune/internal/wire"
	"github.com/aiomni/dune/pkg/api"
	"github.com/aiomni/dune/pkg/upgrade"
	"modernc.org/sqlite"
)

const MaxKeys = 4096
const MaxRecordBytes = 256 << 10

// Record includes private recovery locations. Public queries return Operation
// only; paths and service/configuration bytes are never diagnostics.
type Record struct {
	Operation           upgrade.Operation     `json:"operation"`
	Owner               string                `json:"owner"`
	Original            installation.Location `json:"original"`
	Candidate           installation.Location `json:"candidate"`
	ConfigurationSHA256 string                `json:"configuration_sha256"`
	Switched            bool                  `json:"switched"`
	Deadline            time.Time             `json:"deadline"`
	RollbackDeadline    time.Time             `json:"rollback_deadline,omitempty"`
}

type Store struct {
	db        *sql.DB
	directory string
}

func Open(ctx context.Context, directory string) (*Store, error) {
	return open(ctx, directory, false)
}

// OpenReadOnly cannot create jobs, databases, schemas or recovery side effects.
func OpenReadOnly(ctx context.Context, directory string) (*Store, error) {
	return open(ctx, directory, true)
}

func open(ctx context.Context, directory string, readOnly bool) (*Store, error) {
	if !readOnly {
		if err := os.MkdirAll(directory, 0700); err != nil {
			return nil, err
		}
	}
	if err := launchgate.CheckDirectory(directory); err != nil {
		return nil, err
	}
	path := filepath.Join(directory, "upgrades.sqlite")
	for _, suffix := range []string{"", "-journal", "-wal", "-shm"} {
		flags := os.O_RDWR | syscall.O_NOFOLLOW | syscall.O_NONBLOCK
		if readOnly {
			flags = os.O_RDONLY | syscall.O_NOFOLLOW | syscall.O_NONBLOCK
		}
		if suffix == "" && !readOnly {
			flags |= os.O_CREATE
		}
		file, err := os.OpenFile(path+suffix, flags, 0600)
		if os.IsNotExist(err) && suffix != "" {
			continue
		}
		if err != nil {
			return nil, err
		}
		info, err := file.Stat()
		file.Close()
		if err != nil {
			return nil, err
		}
		owner, ok := info.Sys().(*syscall.Stat_t)
		if !info.Mode().IsRegular() || info.Mode().Perm() != 0600 || !ok || int(owner.Uid) != os.Getuid() || owner.Nlink > 1 || (suffix == "" && owner.Nlink != 1) {
			return nil, fmt.Errorf("invalid private upgrade database")
		}
	}
	dsn := &url.URL{Scheme: "file", Path: path}
	query := dsn.Query()
	query.Set("_txlock", "immediate")
	query.Add("_pragma", "busy_timeout(5000)")
	query.Add("_pragma", "synchronous(FULL)")
	if readOnly {
		query.Set("mode", "ro")
		query.Add("_pragma", "query_only(ON)")
	}
	dsn.RawQuery = query.Encode()
	connector, err := sqlite.NewConnector(dsn.String())
	if err != nil {
		return nil, err
	}
	store := &Store{db: sql.OpenDB(connector), directory: directory}
	store.db.SetMaxOpenConns(1)
	if readOnly {
		var contract string
		err := store.db.QueryRowContext(ctx, `SELECT shared_contract FROM upgrade_settings WHERE id=1`).Scan(&contract)
		if err == nil && contract != statecontract.ID() {
			err = fmt.Errorf("upgrade state contract differs from executable")
		}
		if err != nil {
			store.Close()
			return nil, err
		}
		return store, nil
	}
	tx, err := store.db.BeginTx(ctx, nil)
	if err != nil {
		store.Close()
		return nil, err
	}
	defer tx.Rollback()
	_, err = tx.ExecContext(ctx, `CREATE TABLE IF NOT EXISTS upgrade_settings(id INTEGER PRIMARY KEY CHECK(id=1), shared_contract TEXT NOT NULL);
	CREATE TABLE IF NOT EXISTS upgrades(sequence INTEGER PRIMARY KEY AUTOINCREMENT,
		submission_id TEXT UNIQUE NOT NULL, digest TEXT NOT NULL, operation_id TEXT UNIQUE NOT NULL,
		active INTEGER NOT NULL CHECK(active IN(0,1)), expired INTEGER NOT NULL DEFAULT 0,
		revision INTEGER NOT NULL, owner TEXT NOT NULL, data BLOB NOT NULL CHECK(length(data)<=262144));
	CREATE UNIQUE INDEX IF NOT EXISTS upgrade_single_active ON upgrades(active) WHERE active=1`)
	if err == nil {
		_, err = tx.ExecContext(ctx, `INSERT INTO upgrade_settings(id,shared_contract) VALUES(1,?) ON CONFLICT(id) DO NOTHING`, statecontract.ID())
	}
	var contract string
	if err == nil {
		err = tx.QueryRowContext(ctx, `SELECT shared_contract FROM upgrade_settings WHERE id=1`).Scan(&contract)
	}
	if err == nil && contract != statecontract.ID() {
		err = fmt.Errorf("upgrade state contract differs from executable")
	}
	if err == nil {
		err = tx.Commit()
	}
	if err != nil {
		tx.Rollback()
		store.Close()
		return nil, err
	}
	return store, nil
}

func (s *Store) Close() error { return s.db.Close() }

func requestDigest(request upgrade.Request) string {
	body, _ := json.Marshal(request)
	sum := sha256.Sum256(body)
	return hex.EncodeToString(sum[:])
}

type scanner interface{ Scan(...any) error }

func scan(row scanner) (Record, string, error) {
	var record Record
	var body []byte
	var digest string
	if err := row.Scan(&body, &digest); err != nil {
		return record, digest, err
	}
	err := decodeRecord(body, &record)
	if err == nil && requestDigest(record.Operation.Request) != digest {
		err = fmt.Errorf("persisted request differs from original submission digest")
	}
	return record, digest, err
}

func decodeRecord(body []byte, record *Record) error {
	if len(body) > MaxRecordBytes || json.Unmarshal(body, record) != nil {
		return fmt.Errorf("invalid durable upgrade record")
	}
	if record.Operation.ID == "" || record.Operation.Request.Validate() != nil {
		return fmt.Errorf("invalid durable upgrade identity")
	}
	return nil
}

// Existing is safe before expensive inspection/manifest resolution. The caller
// still checks current authorization and original binding on every request.
func (s *Store) Existing(ctx context.Context, request upgrade.Request) (upgrade.Operation, bool, error) {
	if err := request.Validate(); err != nil {
		return upgrade.Operation{}, false, err
	}
	record, digest, err := scan(s.db.QueryRowContext(ctx, `SELECT data,digest FROM upgrades WHERE submission_id=?`, request.SubmissionID))
	if errors.Is(err, sql.ErrNoRows) {
		return upgrade.Operation{}, false, nil
	}
	if err != nil {
		return upgrade.Operation{}, false, err
	}
	if digest != requestDigest(request) {
		return upgrade.Operation{}, true, &api.Error{Code: "SUBMISSION_CONFLICT", Detail: "submission ID already identifies another upgrade request"}
	}
	return record.Operation, true, nil
}

// Admit must be called while holding the physical installation lock and after
// current authorization. Dedup precedes historical source preconditions. Both
// rejection and acceptance are committed before returning a receipt.
func (s *Store) Admit(ctx context.Context, submission upgrade.Submission, target upgrade.Manifest, source upgrade.Inspection, configurationSHA256 string) (upgrade.Operation, error) {
	return s.admit(ctx, submission, target, source, configurationSHA256, "")
}

func (s *Store) Reject(ctx context.Context, submission upgrade.Submission, code string) (upgrade.Operation, error) {
	if api.ValidateSubmissionID(code) != nil {
		return upgrade.Operation{}, fmt.Errorf("stable rejection code required")
	}
	return s.admit(ctx, submission, upgrade.Manifest{}, upgrade.Inspection{}, "", code)
}

func (s *Store) admit(ctx context.Context, submission upgrade.Submission, target upgrade.Manifest, source upgrade.Inspection, configurationSHA256, rejection string) (upgrade.Operation, error) {
	request := submission.Request
	unknown := upgrade.Operation{Request: request, Admission: api.SubmissionUnknown}
	if err := submission.Validate(); err != nil {
		return unknown, err
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return unknown, err
	}
	defer tx.Rollback()
	digest := requestDigest(request)
	existing, original, err := scan(tx.QueryRowContext(ctx, `SELECT data,digest FROM upgrades WHERE submission_id=?`, request.SubmissionID))
	if err == nil {
		if original != digest {
			return unknown, &api.Error{Code: "SUBMISSION_CONFLICT", Detail: "submission ID already identifies another upgrade request"}
		}
		return existing.Operation, nil
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return unknown, err
	}
	var count int
	if err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM upgrades`).Scan(&count); err != nil {
		return unknown, err
	}
	if count >= MaxKeys {
		return unknown, &api.Error{Code: "UPGRADE_CAPACITY_EXHAUSTED", Detail: "installation submission evidence capacity exhausted"}
	}
	now := time.Now().UTC()
	op := upgrade.Operation{ID: wire.ID(), Request: request, Revision: "1", Admission: api.SubmissionAccepted, Phase: upgrade.Queued, Rollback: upgrade.RollbackNotNeeded, Source: source, Target: target, StateContract: statecontract.ID(), StartedAt: submission.ReservedAt.UTC(), UpdatedAt: now, Participants: []api.UpgradeHost{}}
	reject := func(code string) {
		op.Admission, op.Phase, op.Confirmed, op.Failure = api.SubmissionNotAccepted, upgrade.Failed, true, &upgrade.Issue{Code: code, Stage: "admission"}
	}
	active, _, activeErr := scan(tx.QueryRowContext(ctx, `SELECT data,digest FROM upgrades WHERE active=1`))
	if activeErr != nil && !errors.Is(activeErr, sql.ErrNoRows) {
		return unknown, activeErr
	}
	switch {
	case rejection != "":
		reject(rejection)
		if activeErr == nil {
			op.ActiveOperationID = active.Operation.ID
		}
	case !upgrade.ValidSHA256(configurationSHA256):
		reject("UPGRADE_CONFIG_UNVERIFIABLE")
	case source.Installation == nil || source.Installation.ID != request.InstallationID || source.Binding != request.Binding:
		reject("INSTALLATION_CHANGED")
	case activeErr == nil:
		reject("UPGRADE_CONFLICT")
		op.ActiveOperationID = active.Operation.ID
	case target.ValidateDownload() != nil || !target.Matches(request.Release):
		reject("RELEASE_CHANGED")
	case target.StateContract != statecontract.ID():
		reject("STATE_CONTRACT_UNSUPPORTED")
	case !source.Supported:
		reject("UPGRADE_UNSUPPORTED")
	default:
		op.Plan = upgrade.Compare(source, target)
		if !op.Plan.ReleaseUpdateRequired && !op.Plan.ConnectorRestartRequired {
			op.Phase, op.Confirmed = upgrade.AlreadyCurrent, true
		} else if source.Installation.Revision != request.ExpectedInstallationRevision || source.Running.SHA256 != request.ExpectedRunningSHA256 {
			reject("INSTALLATION_CHANGED")
		}
	}
	record := Record{Operation: op, ConfigurationSHA256: configurationSHA256, Deadline: now.Add(10 * time.Minute)}
	body, err := encode(record)
	if err != nil {
		return unknown, err
	}
	_, err = tx.ExecContext(ctx, `INSERT INTO upgrades(submission_id,digest,operation_id,active,revision,owner,data) VALUES(?,?,?,?,1,'',?)`, request.SubmissionID, digest, op.ID, activeValue(op), body)
	if err != nil {
		return unknown, err
	}
	if err := tx.Commit(); err != nil {
		return unknown, err
	}
	return op, nil
}

func activeValue(operation upgrade.Operation) int {
	if operation.Confirmed && !operation.LaunchSealed {
		return 0
	}
	return 1
}

func encode(record Record) ([]byte, error) {
	body, err := json.Marshal(record)
	if err != nil {
		return nil, err
	}
	if len(body) > MaxRecordBytes {
		return nil, fmt.Errorf("upgrade record exceeds bounded evidence capacity")
	}
	return body, nil
}

func (s *Store) Get(ctx context.Context, query upgrade.Query) (upgrade.Operation, error) {
	if !query.Binding.Valid() || api.ValidateSubmissionID(query.InstallationID) != nil || (query.SubmissionID == "") == (query.OperationID == "") {
		return upgrade.Operation{}, &api.Error{Code: "INVALID_ARGUMENT", Detail: "one original upgrade selector required"}
	}
	column, value := "submission_id", query.SubmissionID
	if value == "" {
		column, value = "operation_id", query.OperationID
	}
	if api.ValidateSubmissionID(value) != nil {
		return upgrade.Operation{}, &api.Error{Code: "INVALID_ARGUMENT", Detail: "invalid upgrade selector"}
	}
	record, _, err := scan(s.db.QueryRowContext(ctx, `SELECT data,digest FROM upgrades WHERE `+column+`=?`, value))
	if errors.Is(err, sql.ErrNoRows) {
		return upgrade.Operation{Request: upgrade.Request{SubmissionID: query.SubmissionID, Binding: query.Binding, InstallationID: query.InstallationID}, Admission: api.SubmissionUnknown}, &api.Error{Code: "UPGRADE_NOT_FOUND", Detail: "no confirmed record for this selector"}
	}
	if err != nil {
		return upgrade.Operation{}, err
	}
	if record.Operation.Request.Binding != query.Binding || record.Operation.Request.InstallationID != query.InstallationID {
		return upgrade.Operation{}, &api.Error{Code: "STALE_BINDING", Detail: "operation belongs to another installation or Runner binding"}
	}
	return record.Operation, nil
}

func nextRevision(value string) (string, error) {
	revision, err := strconv.ParseUint(value, 10, 63)
	if err != nil || revision >= 1<<62 {
		return "", fmt.Errorf("upgrade revision exhausted or invalid")
	}
	return strconv.FormatUint(revision+1, 10), nil
}
