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

const DefaultMaxKeys = 4096

type Options struct {
	// MaxKeys bounds persistent evidence, including rejected and unfinished
	// claims. Evidence is never evicted to make room for another execution.
	MaxKeys int
}

type Registry struct {
	db      *sql.DB
	maxKeys int
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
	dsn.RawQuery = query.Encode()
	connector, err := sqlite.NewConnector(dsn.String())
	if err != nil {
		return nil, err
	}
	r := &Registry{db: sql.OpenDB(connector), maxKeys: options.MaxKeys}
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
		id INTEGER PRIMARY KEY CHECK (id = 1), max_keys INTEGER NOT NULL);
		CREATE TABLE IF NOT EXISTS submission_keys (
		key TEXT PRIMARY KEY, digest TEXT NOT NULL, receiver TEXT NOT NULL,
		token TEXT NOT NULL, state TEXT NOT NULL CHECK (state IN ('claimed','accepted','not_accepted')),
		operation_ref TEXT NOT NULL DEFAULT '', error_code TEXT NOT NULL DEFAULT '')`)
	if err != nil {
		return err
	}
	if _, err = tx.ExecContext(ctx, `INSERT INTO registry_settings(id,max_keys) VALUES(1,?) ON CONFLICT(id) DO NOTHING`, r.maxKeys); err != nil {
		return err
	}
	var configured int
	if err = tx.QueryRowContext(ctx, `SELECT max_keys FROM registry_settings WHERE id=1`).Scan(&configured); err != nil {
		return err
	}
	if configured != r.maxKeys {
		return fmt.Errorf("registry capacity differs from its persisted configuration")
	}
	return tx.Commit()
}

func (r *Registry) Close() error { return r.db.Close() }

// ClaimKey atomically binds a key to one digest and receiver. receiver names a
// session host instance or the independent registry, not a network connection.
// Acquired is false for every existing record; callers must not execute again.
func (r *Registry) ClaimKey(ctx context.Context, key api.SubmissionKey, digest [32]byte, receiver string) (Claim, api.SubmissionReceipt, error) {
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
		if stored.digest != hex.EncodeToString(digest[:]) || stored.receiver != receiver {
			return Claim{}, result, conflict()
		}
		return Claim{}, stored.receipt(key), nil
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return Claim{}, result, err
	}
	var count int
	if err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM submission_keys`).Scan(&count); err != nil {
		return Claim{}, result, err
	}
	if count >= r.maxKeys {
		return Claim{}, result, &api.Error{Code: "SUBMISSION_CAPACITY_EXHAUSTED", Detail: "persistent submission evidence capacity reached"}
	}
	claim := Claim{key: key, token: wire.ID()}
	_, err = tx.ExecContext(ctx, `INSERT INTO submission_keys(key,digest,receiver,token,state) VALUES(?,?,?,?,'claimed')`, encoded, hex.EncodeToString(digest[:]), receiver, claim.token)
	if err != nil {
		return Claim{}, result, err
	}
	if err = tx.Commit(); err != nil {
		// A failed commit acknowledgement grants no permission to execute.
		return Claim{}, result, err
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
	return r.resolve(ctx, claim, "accepted", operationRef, "")
}

// Reject permanently closes a claimed key. Capacity eviction and transport
// reconnect cannot turn this decision back into an executable submission.
func (r *Registry) Reject(ctx context.Context, claim Claim, code string) (api.SubmissionReceipt, error) {
	if api.ValidateSubmissionID(code) != nil {
		return api.SubmissionReceipt{}, &api.Error{Code: "INVALID_ARGUMENT", Detail: "bounded rejection code required"}
	}
	return r.resolve(ctx, claim, "not_accepted", "", code)
}

func (r *Registry) resolve(ctx context.Context, claim Claim, state, operationRef, code string) (api.SubmissionReceipt, error) {
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
	if record.state != "claimed" {
		if record.state != state || record.operationRef != operationRef || record.errorCode != code {
			return result, conflict()
		}
		return record.receipt(claim.key), nil
	}
	_, err = tx.ExecContext(ctx, `UPDATE submission_keys SET state=?,operation_ref=?,error_code=? WHERE key=?`, state, operationRef, code, encoded)
	if err != nil {
		return result, err
	}
	if err = tx.Commit(); err != nil {
		return result, err
	}
	return api.SubmissionReceipt{SubmissionKey: claim.key, Admission: api.SubmissionAdmission(state), OperationRef: operationRef, ErrorCode: code}, nil
}

type record struct {
	digest, receiver, token, state, operationRef, errorCode string
}

func (r record) receipt(key api.SubmissionKey) api.SubmissionReceipt {
	admission := api.SubmissionAdmission(r.state)
	if r.state == "claimed" {
		admission = api.SubmissionUnknown
	}
	return api.SubmissionReceipt{SubmissionKey: key, Admission: admission, OperationRef: r.operationRef, ErrorCode: r.errorCode}
}

type queryRow interface {
	QueryRowContext(context.Context, string, ...any) *sql.Row
}

func readRecord(ctx context.Context, db queryRow, key string) (record, error) {
	var result record
	err := db.QueryRowContext(ctx, `SELECT digest,receiver,token,state,operation_ref,error_code FROM submission_keys WHERE key=?`, key).Scan(
		&result.digest, &result.receiver, &result.token, &result.state, &result.operationRef, &result.errorCode)
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
	if !info.Mode().IsRegular() || info.Mode().Perm() != 0600 || !ok || int(stat.Uid) != os.Getuid() || stat.Nlink != 1 {
		return fmt.Errorf("registry files must be private regular files owned by this user without hard links")
	}
	return nil
}
