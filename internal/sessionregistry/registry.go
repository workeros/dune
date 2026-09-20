// Package sessionregistry stores minimum local admission evidence independently
// of fabricd and session hosts. It never starts an Agent or retains request bodies.
package sessionregistry

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
	"strings"
	"syscall"

	"github.com/aiomni/dune/internal/wire"
	"github.com/aiomni/dune/pkg/api"
	"modernc.org/sqlite"
)

const (
	DefaultMaxKeys     = 4096
	DefaultMaxControls = 4096
	MaxLiveRuntimes    = 16
	MaxRuntimeRecords  = 256
)

type Options struct {
	// MaxKeys bounds persistent evidence, including rejected and unfinished
	// claims. Evidence is never evicted to make room for another execution.
	MaxKeys int
	// MaxControls independently bounds each cancel/permission reservation class,
	// including completed evidence. Runtime stop/forget each have their own slot.
	MaxControls int
}

type Registry struct {
	db          *sql.DB
	maxKeys     int
	maxControls int
}

// Claim is an exclusive, private permission to resolve one key's admission.
// An existing key never returns another permission, even for the same payload.
// The claim itself is not proof that a business operation was admitted.
type Claim struct {
	key   api.SubmissionKey
	token string
}

func (c Claim) Acquired() bool { return c.token != "" }

// Digest includes the operation name and the frozen semantic request. Callers
// must omit transport epochs and include every business parameter. Only this
// digest, never the payload, is persisted by the registry.
func Digest(operation string, payload []byte) [32]byte {
	h := sha256.New()
	h.Write([]byte(operation))
	h.Write([]byte{0})
	h.Write(payload)
	var digest [32]byte
	copy(digest[:], h.Sum(nil))
	return digest
}

func Open(ctx context.Context, directory string, options Options) (*Registry, error) {
	if options.MaxKeys == 0 {
		options.MaxKeys = DefaultMaxKeys
	}
	if options.MaxControls == 0 {
		options.MaxControls = DefaultMaxControls
	}
	if options.MaxControls < 1 || options.MaxControls > 65536 {
		return nil, fmt.Errorf("registry control capacity must be 1..65536 per class")
	}
	if options.MaxKeys < 1 || options.MaxKeys > 65536 {
		return nil, fmt.Errorf("registry max keys must be 1..65536")
	}
	if err := prepareDirectory(directory); err != nil {
		return nil, err
	}
	file := filepath.Join(directory, "registry.sqlite")
	if err := prepareFile(file, true); err != nil {
		return nil, err
	}
	for _, suffix := range []string{"-journal", "-wal", "-shm"} {
		if err := prepareFile(file+suffix, false); err != nil {
			return nil, err
		}
	}
	dsn := &url.URL{Scheme: "file", Path: file}
	query := dsn.Query()
	query.Set("_txlock", "immediate")
	query.Add("_pragma", "busy_timeout(5000)")
	query.Add("_pragma", "synchronous(FULL)")
	query.Add("_pragma", "foreign_keys(ON)")
	dsn.RawQuery = query.Encode()
	connector, err := sqlite.NewConnector(dsn.String())
	if err != nil {
		return nil, err
	}
	r := &Registry{db: sql.OpenDB(connector), maxKeys: options.MaxKeys, maxControls: options.MaxControls}
	r.db.SetMaxOpenConns(1)
	if err := r.initialize(ctx); err != nil {
		r.Close()
		return nil, err
	}
	return r, nil
}

func (r *Registry) initialize(ctx context.Context) error {
	tx, err := r.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	_, err = tx.ExecContext(ctx, `CREATE TABLE IF NOT EXISTS registry_settings (
		id INTEGER PRIMARY KEY CHECK (id = 1), max_keys INTEGER NOT NULL, max_controls INTEGER NOT NULL);
		CREATE TABLE IF NOT EXISTS submission_keys (
		key TEXT PRIMARY KEY, digest TEXT NOT NULL, receiver TEXT NOT NULL,
		token TEXT NOT NULL, state TEXT NOT NULL CHECK (state IN ('claimed','accepted','not_accepted')),
		operation_ref TEXT NOT NULL DEFAULT '', error_code TEXT NOT NULL DEFAULT '',
		control_resource TEXT NOT NULL DEFAULT '', stage TEXT NOT NULL DEFAULT '', runtime BLOB, worktree BLOB, cleanup BLOB, raw_input BLOB);
		CREATE TABLE IF NOT EXISTS runtime_reservations (
			target TEXT PRIMARY KEY, live INTEGER NOT NULL DEFAULT 1, sealed INTEGER NOT NULL DEFAULT 0);
		CREATE TABLE IF NOT EXISTS control_reservations (
			resource TEXT PRIMARY KEY, target TEXT NOT NULL, kind TEXT NOT NULL,
			consumed_key TEXT NOT NULL DEFAULT '', FOREIGN KEY(target) REFERENCES runtime_reservations(target));
		CREATE TABLE IF NOT EXISTS session_hosts (
			target TEXT PRIMARY KEY REFERENCES runtime_reservations(target), instance TEXT NOT NULL UNIQUE,
			boot_id TEXT NOT NULL, pid INTEGER NOT NULL, group_id INTEGER NOT NULL DEFAULT 0,
			group_generation INTEGER NOT NULL DEFAULT 0, phase TEXT NOT NULL DEFAULT 'active',
			runtime BLOB NOT NULL, registration BLOB NOT NULL, resources BLOB NOT NULL);
		CREATE TABLE IF NOT EXISTS cleanup_jobs (
			key TEXT PRIMARY KEY REFERENCES submission_keys(key), host BLOB NOT NULL, local BLOB NOT NULL,
			confirmed INTEGER NOT NULL DEFAULT 0, executor_term INTEGER NOT NULL DEFAULT 0)`)
	if err != nil {
		return err
	}
	if _, err = tx.ExecContext(ctx, `INSERT INTO registry_settings(id,max_keys,max_controls) VALUES(1,?,?) ON CONFLICT(id) DO NOTHING`, r.maxKeys, r.maxControls); err != nil {
		return err
	}
	var configured, controls int
	if err = tx.QueryRowContext(ctx, `SELECT max_keys,max_controls FROM registry_settings WHERE id=1`).Scan(&configured, &controls); err != nil {
		return err
	}
	if configured != r.maxKeys || controls != r.maxControls {
		return fmt.Errorf("registry capacity differs from its persisted configuration")
	}
	return tx.Commit()
}

func (r *Registry) Close() error { return r.db.Close() }

// ClaimKey atomically binds a key to one digest and receiver. receiver names a
// session host instance or the independent registry, not a network connection.
// Acquired is false for every existing record; callers must not execute again.
func (r *Registry) ClaimKey(ctx context.Context, key api.SubmissionKey, digest [32]byte, receiver string) (Claim, api.SubmissionReceipt, error) {
	return r.claimKey(ctx, key, digest, receiver, "", "")
}

func (r *Registry) claimKey(ctx context.Context, key api.SubmissionKey, digest [32]byte, receiver, controlResource, stopRef string) (Claim, api.SubmissionReceipt, error) {
	result := api.SubmissionReceipt{SubmissionKey: key, Admission: api.SubmissionUnknown}
	encoded, err := encodeKey(key)
	if err != nil {
		return Claim{}, result, err
	}
	if receiver == "" || len(receiver) > 256 || strings.ContainsAny(receiver, "\x00\r\n") {
		return Claim{}, result, &api.Error{Code: "INVALID_ARGUMENT", Detail: "registry receiver identity required"}
	}
	tx, err := r.db.BeginTx(ctx, nil)
	if err != nil {
		return Claim{}, result, err
	}
	defer tx.Rollback()
	stored, err := readRecord(ctx, tx, encoded)
	if err == nil {
		if stored.digest != hex.EncodeToString(digest[:]) || stored.receiver != receiver || stored.controlResource != controlResource {
			return Claim{}, result, conflict()
		}
		return Claim{}, stored.receipt(key), nil
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return Claim{}, result, err
	}
	if err := checkRuntimeOpen(ctx, tx, key.Target); err != nil {
		return Claim{}, result, err
	}
	if controlResource == "" {
		var count int
		if err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM submission_keys WHERE control_resource=''`).Scan(&count); err != nil {
			return Claim{}, result, err
		}
		if count >= r.maxKeys {
			return Claim{}, result, &api.Error{Code: "SUBMISSION_CAPACITY_EXHAUSTED", Detail: "ordinary persistent submission evidence capacity reached"}
		}
	} else {
		var target, consumed string
		if err := tx.QueryRowContext(ctx, `SELECT target,consumed_key FROM control_reservations WHERE resource=?`, controlResource).Scan(&target, &consumed); err != nil || target != encodeTarget(key.Target) || consumed != "" {
			return Claim{}, result, &api.Error{Code: "CONTROL_UNAVAILABLE", Detail: "control reservation is absent or already consumed"}
		}
		if _, err := tx.ExecContext(ctx, `UPDATE control_reservations SET consumed_key=? WHERE resource=?`, encoded, controlResource); err != nil {
			return Claim{}, result, err
		}
	}
	claim := Claim{key: key, token: wire.ID()}
	state, stage := "claimed", ""
	if stopRef != "" {
		state, stage = "accepted", "stopping"
	}
	_, err = tx.ExecContext(ctx, `INSERT INTO submission_keys(key,digest,receiver,token,state,control_resource,operation_ref,stage) VALUES(?,?,?,?,?,?,?,?)`, encoded, hex.EncodeToString(digest[:]), receiver, claim.token, state, controlResource, stopRef, stage)
	if err != nil {
		return Claim{}, result, err
	}
	if err = tx.Commit(); err != nil {
		// A failed commit acknowledgement grants no permission to execute.
		return Claim{}, result, err
	}
	if stopRef != "" {
		result.Admission, result.OperationRef, result.Stage = api.SubmissionAccepted, stopRef, stage
	}
	return claim, result, nil
}

// Get is observational: both an absent key and an unfinished private claim are
// unknown. Looking up a key neither reserves it nor closes its admission path.
func (r *Registry) Get(ctx context.Context, key api.SubmissionKey) (api.SubmissionReceipt, error) {
	result := api.SubmissionReceipt{SubmissionKey: key, Admission: api.SubmissionUnknown}
	encoded, err := encodeKey(key)
	if err != nil {
		return result, err
	}
	record, err := readRecord(ctx, r.db, encoded)
	if errors.Is(err, sql.ErrNoRows) {
		return result, nil
	}
	if err != nil {
		return result, err
	}
	return record.receipt(key), nil
}

// Accept records the admission decision before its owner releases business
// work. An interrupted owner must never restart that work from this receipt.
func (r *Registry) Accept(ctx context.Context, claim Claim, operationRef string) (api.SubmissionReceipt, error) {
	if len(operationRef) > 8192 {
		return api.SubmissionReceipt{}, &api.Error{Code: "INVALID_ARGUMENT", Detail: "operation reference exceeds registry budget"}
	}
	return r.resolve(ctx, claim, "accepted", operationRef, "", nil)
}

// AcceptRaw stores input identity/order with the admission decision. Complete
// message bytes remain owned by the original host, never in discovery storage.
func (r *Registry) AcceptRaw(ctx context.Context, claim Claim, operationRef string, input api.RawACPInputReceipt) (api.SubmissionReceipt, error) {
	if api.ValidateSubmissionID(operationRef) != nil || api.ValidateSubmissionID(input.StreamID) != nil || input.InputEpoch == 0 || input.Length < 0 || input.Length > api.RawACPMaxMessageBytes || (input.Sequence == 0) != (input.Length == 0) {
		return api.SubmissionReceipt{SubmissionKey: claim.key, Admission: api.SubmissionUnknown}, &api.Error{Code: "INVALID_ARGUMENT", Detail: "valid raw input identity and order required"}
	}
	return r.resolve(ctx, claim, "accepted", operationRef, "", &input)
}

// Reject permanently closes a claimed key. Capacity eviction and transport
// reconnect cannot turn this decision back into an executable submission.
func (r *Registry) Reject(ctx context.Context, claim Claim, code string) (api.SubmissionReceipt, error) {
	if api.ValidateSubmissionID(code) != nil {
		return api.SubmissionReceipt{}, &api.Error{Code: "INVALID_ARGUMENT", Detail: "bounded rejection code required"}
	}
	return r.resolve(ctx, claim, "not_accepted", "", code, nil)
}

func (r *Registry) resolve(ctx context.Context, claim Claim, state, operationRef, code string, input *api.RawACPInputReceipt) (api.SubmissionReceipt, error) {
	result := api.SubmissionReceipt{SubmissionKey: claim.key, Admission: api.SubmissionUnknown}
	encoded, err := encodeKey(claim.key)
	if err != nil || !claim.Acquired() {
		return result, &api.Error{Code: "INVALID_ARGUMENT", Detail: "exclusive submission claim required"}
	}
	tx, err := r.db.BeginTx(ctx, nil)
	if err != nil {
		return result, err
	}
	defer tx.Rollback()
	record, err := readRecord(ctx, tx, encoded)
	if err != nil {
		return result, err
	}
	if record.token != claim.token {
		return result, conflict()
	}
	var rawInput []byte
	if input != nil {
		rawInput = api.Payload(input)
	}
	if record.state != "claimed" {
		if record.state != state || record.operationRef != operationRef || record.errorCode != code || string(record.rawInput) != string(rawInput) {
			return result, conflict()
		}
		return record.receipt(claim.key), nil
	}
	if state == "accepted" {
		if err := checkRuntimeOpen(ctx, tx, claim.key.Target); err != nil {
			return result, err
		}
	}
	_, err = tx.ExecContext(ctx, `UPDATE submission_keys SET state=?,operation_ref=?,error_code=?,raw_input=? WHERE key=?`, state, operationRef, code, rawInput, encoded)
	if err != nil {
		return result, err
	}
	if err = tx.Commit(); err != nil {
		return result, err
	}
	return api.SubmissionReceipt{SubmissionKey: claim.key, Admission: api.SubmissionAdmission(state), OperationRef: operationRef, ErrorCode: code, RawInput: input}, nil
}

type record struct {
	worktree                                                                        []byte
	digest, receiver, token, state, operationRef, errorCode, stage, controlResource string
	runtime                                                                         []byte
	cleanup                                                                         []byte
	rawInput                                                                        []byte
}

func (r record) receipt(key api.SubmissionKey) api.SubmissionReceipt {
	admission := api.SubmissionAdmission(r.state)
	if r.state == "claimed" {
		admission = api.SubmissionUnknown
	}
	receipt := api.SubmissionReceipt{SubmissionKey: key, Admission: admission, OperationRef: r.operationRef, ErrorCode: r.errorCode, Stage: r.stage}
	if len(r.runtime) != 0 {
		_ = json.Unmarshal(r.runtime, &receipt.Runtime)
	}
	if len(r.worktree) != 0 {
		_ = json.Unmarshal(r.worktree, &receipt.Worktree)
	}
	if len(r.cleanup) != 0 {
		_ = json.Unmarshal(r.cleanup, &receipt.Cleanup)
	}
	if len(r.rawInput) != 0 {
		_ = json.Unmarshal(r.rawInput, &receipt.RawInput)
	}
	return receipt
}

type queryRow interface {
	QueryRowContext(context.Context, string, ...any) *sql.Row
}

func readRecord(ctx context.Context, db queryRow, key string) (record, error) {
	var result record
	err := db.QueryRowContext(ctx, `SELECT digest,receiver,token,state,operation_ref,error_code,stage,control_resource,runtime,worktree,cleanup,raw_input FROM submission_keys WHERE key=?`, key).Scan(
		&result.digest, &result.receiver, &result.token, &result.state, &result.operationRef, &result.errorCode, &result.stage, &result.controlResource, &result.runtime, &result.worktree, &result.cleanup, &result.rawInput)
	return result, err
}

func encodeKey(key api.SubmissionKey) (string, error) {
	if err := key.Validate(); err != nil {
		return "", &api.Error{Code: "INVALID_ARGUMENT", Detail: err.Error()}
	}
	encoded, err := json.Marshal(key)
	return string(encoded), err
}

func conflict() error {
	return &api.Error{Code: "SUBMISSION_CONFLICT", Detail: "submission key is already bound to another request or admission decision"}
}

func prepareDirectory(directory string) error {
	if !filepath.IsAbs(directory) || filepath.Clean(directory) == "/" {
		return fmt.Errorf("registry directory must be absolute and private")
	}
	if err := os.MkdirAll(directory, 0700); err != nil {
		return err
	}
	info, err := os.Lstat(directory)
	if err != nil {
		return err
	}
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !info.IsDir() || info.Mode().Perm() != 0700 || !ok || int(stat.Uid) != os.Getuid() {
		return fmt.Errorf("registry directory must be owned by this user, mode 0700, and not a symlink")
	}
	return nil
}

func prepareFile(path string, create bool) error {
	flags := os.O_RDWR | syscall.O_NOFOLLOW | syscall.O_NONBLOCK
	if create {
		flags |= os.O_CREATE
	}
	f, err := os.OpenFile(path, flags, 0600)
	if !create && errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil {
		return err
	}
	stat, ok := info.Sys().(*syscall.Stat_t)
	// A concurrently committing SQLite connection can unlink its journal after
	// this open. That old descriptor (Nlink=0) is harmless; the main database
	// must remain linked, and multiple links are never accepted for any file.
	if !info.Mode().IsRegular() || info.Mode().Perm() != 0600 || !ok || int(stat.Uid) != os.Getuid() || stat.Nlink > 1 || (create && stat.Nlink != 1) {
		return fmt.Errorf("%s must be a private regular registry file owned by this user without hard links", filepath.Base(path))
	}
	return nil
}

// Lookup checks a frozen request without claiming its key. Control admission
// uses this before lifecycle validation so a consumed target can still return
// its original receipt, while invalid new targets spend no reserved capacity.
func (r *Registry) Lookup(ctx context.Context, key api.SubmissionKey, digest [32]byte, receiver string) (api.SubmissionReceipt, bool, error) {
	result := api.SubmissionReceipt{SubmissionKey: key, Admission: api.SubmissionUnknown}
	encoded, err := encodeKey(key)
	if err != nil {
		return result, false, err
	}
	record, err := readRecord(ctx, r.db, encoded)
	if errors.Is(err, sql.ErrNoRows) {
		return result, false, nil
	}
	if err != nil {
		return result, false, err
	}
	if record.digest != hex.EncodeToString(digest[:]) || record.receiver != receiver {
		return result, true, conflict()
	}
	return record.receipt(key), true, nil
}
