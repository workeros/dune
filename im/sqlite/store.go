// Package sqlite supplies a durable single-host store for Dune IM bindings.
// A clustered host must use a shared store with the same atomic guarantees.
package sqlite

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"time"

	"github.com/aiomni/dune/im/channel"
	_ "modernc.org/sqlite"
)

type Store struct {
	db                   *sql.DB
	maxPendingPerBinding int
	maxEventBytes        int
	maxClaimAttempts     int
}

type Options struct {
	MaxPendingPerBinding int
	MaxEventBytes        int
	MaxClaimAttempts     int
}

func Open(ctx context.Context, path string) (*Store, error) {
	return OpenWithOptions(ctx, path, Options{})
}

func OpenWithOptions(ctx context.Context, path string, options Options) (*Store, error) {
	if path == "" {
		return nil, errors.New("IM SQLite path is required")
	}
	if options.MaxPendingPerBinding < 0 || options.MaxEventBytes < 0 || options.MaxClaimAttempts < 0 {
		return nil, errors.New("IM inbox limits cannot be negative")
	}
	if options.MaxPendingPerBinding == 0 {
		options.MaxPendingPerBinding = 10000
	}
	if options.MaxEventBytes == 0 {
		options.MaxEventBytes = 256 * 1024
	}
	if options.MaxClaimAttempts == 0 {
		options.MaxClaimAttempts = 5
	}
	file, err := os.OpenFile(path, os.O_RDWR|os.O_CREATE, 0600)
	if err != nil {
		return nil, fmt.Errorf("open IM SQLite file: %w", err)
	}
	info, statErr := file.Stat()
	closeErr := file.Close()
	if statErr != nil {
		return nil, statErr
	}
	if closeErr != nil {
		return nil, closeErr
	}
	if !info.Mode().IsRegular() || info.Mode().Perm()&0077 != 0 {
		return nil, errors.New("IM SQLite file must be regular and accessible only by its owner")
	}
	db, err := sql.Open("sqlite", path)
	if err != nil {
		return nil, err
	}
	// PRAGMAs are connection-local. One connection also keeps :memory: stores
	// and transactional ordering predictable for the single-host MVP.
	db.SetMaxOpenConns(1)
	for _, statement := range []string{
		"PRAGMA busy_timeout = 5000",
		"PRAGMA journal_mode = WAL",
		"PRAGMA synchronous = FULL",
		`CREATE TABLE IF NOT EXISTS im_inbox (
			binding_id TEXT NOT NULL,
			event_id TEXT NOT NULL,
			chat_id TEXT NOT NULL,
			chat_kind TEXT NOT NULL,
			session_hash TEXT NOT NULL DEFAULT '',
			message_json BLOB NOT NULL,
			state TEXT NOT NULL DEFAULT 'queued',
			attempts INTEGER NOT NULL DEFAULT 0,
			claim_token TEXT NOT NULL DEFAULT '',
			lease_until INTEGER NOT NULL DEFAULT 0,
			available_at INTEGER NOT NULL DEFAULT 0,
			failure TEXT NOT NULL DEFAULT '',
			created_at TEXT NOT NULL DEFAULT CURRENT_TIMESTAMP,
			PRIMARY KEY (binding_id, event_id)
		)`,
		`CREATE INDEX IF NOT EXISTS im_inbox_group_claims ON im_inbox(binding_id, chat_id, chat_kind, state)`,
		`CREATE INDEX IF NOT EXISTS im_inbox_session_order ON im_inbox(session_hash, state)`,
		`CREATE TABLE IF NOT EXISTS im_deliveries (
			binding_id TEXT NOT NULL,
			delivery_id TEXT NOT NULL,
			key_hash TEXT NOT NULL,
			state_json BLOB NOT NULL,
			revision INTEGER NOT NULL,
			PRIMARY KEY (binding_id, delivery_id)
		)`,
		`CREATE TABLE IF NOT EXISTS im_conversations (
			key_hash TEXT PRIMARY KEY,
			binding_id TEXT NOT NULL,
			chat_id TEXT NOT NULL,
			provider_thread_ref TEXT,
			key_json BLOB NOT NULL,
			session_json BLOB NOT NULL,
			revision INTEGER NOT NULL,
			state TEXT NOT NULL DEFAULT 'ready',
			lease_token TEXT NOT NULL DEFAULT '',
			lease_until INTEGER NOT NULL DEFAULT 0,
			current_event_id TEXT NOT NULL DEFAULT '',
			failure TEXT NOT NULL DEFAULT ''
		)`,
		`CREATE UNIQUE INDEX IF NOT EXISTS im_conversations_thread_ref ON im_conversations(binding_id, chat_id, provider_thread_ref)`,
		`CREATE TABLE IF NOT EXISTS im_bindings (
			id TEXT PRIMARY KEY,
			tenant_id TEXT NOT NULL,
			binding_json BLOB NOT NULL,
			revision INTEGER NOT NULL
		)`,
		`CREATE INDEX IF NOT EXISTS im_bindings_by_tenant ON im_bindings(tenant_id, id)`,
	} {
		if _, err := db.ExecContext(ctx, statement); err != nil {
			_ = db.Close()
			return nil, fmt.Errorf("initialize IM SQLite store: %w", err)
		}
	}
	return &Store{db: db, maxPendingPerBinding: options.MaxPendingPerBinding, maxEventBytes: options.MaxEventBytes, maxClaimAttempts: options.MaxClaimAttempts}, nil
}

func (s *Store) Close() error { return s.db.Close() }

func (s *Store) Insert(ctx context.Context, message channel.InboundMessage) error {
	return s.insert(ctx, nil, message)
}

func (s *Store) InsertForBinding(ctx context.Context, binding channel.BotBinding, message channel.InboundMessage) error {
	if binding.ID == "" || binding.TenantID == "" || binding.Revision < 1 || binding.Provider == "" || !binding.Enabled || message.BindingID != binding.ID || message.BindingRevision != binding.Revision {
		return errors.New("IM event does not match an active Binding revision")
	}
	return s.insert(ctx, &binding, message)
}

func (s *Store) insert(ctx context.Context, expected *channel.BotBinding, message channel.InboundMessage) error {
	if message.BindingID == "" || message.EventID == "" || message.MessageID == "" {
		return errors.New("IM event identity is incomplete")
	}
	data, err := json.Marshal(message)
	if err != nil {
		return err
	}
	if len(data) > s.maxEventBytes {
		return errors.New("IM event exceeds maximum stored size")
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if expected != nil {
		// Take SQLite's writer lock before checking the revision. A read-only
		// check could race with Put, especially when an event is a duplicate
		// and the insert path returns without making a write.
		locked, err := tx.ExecContext(ctx, `UPDATE im_bindings SET revision = revision WHERE id = ? AND tenant_id = ? AND revision = ?`, expected.ID, expected.TenantID, expected.Revision)
		if err != nil {
			return err
		}
		affected, err := locked.RowsAffected()
		if err != nil {
			return err
		}
		if affected != 1 {
			return errors.New("IM Binding revision is stale or disabled")
		}
		var bindingJSON []byte
		if err := tx.QueryRowContext(ctx, `SELECT binding_json FROM im_bindings WHERE id = ? AND tenant_id = ? AND revision = ?`, expected.ID, expected.TenantID, expected.Revision).Scan(&bindingJSON); err != nil {
			if errors.Is(err, sql.ErrNoRows) {
				return errors.New("IM Binding revision is stale or disabled")
			}
			return err
		}
		var current channel.BotBinding
		if err := json.Unmarshal(bindingJSON, &current); err != nil {
			return err
		}
		if current.ID != expected.ID || current.TenantID != expected.TenantID || current.Revision != expected.Revision || current.Provider != expected.Provider || !current.Enabled {
			return errors.New("IM Binding revision is stale or disabled")
		}
	}
	var stored []byte
	err = tx.QueryRowContext(ctx, `SELECT message_json FROM im_inbox WHERE binding_id = ? AND event_id = ?`, message.BindingID, message.EventID).Scan(&stored)
	if err == nil {
		if string(stored) != string(data) {
			// A redelivery may reach a new Channel after credential rotation.
			// ACK it only if the event is otherwise identical, retaining the
			// original revision so workers cannot execute it under the new bot.
			var existing channel.InboundMessage
			if expected == nil || json.Unmarshal(stored, &existing) != nil || existing.BindingRevision == 0 {
				return errors.New("IM event ID collides with a different payload")
			}
			message.BindingRevision = existing.BindingRevision
			normalized, err := json.Marshal(message)
			if err != nil || string(stored) != string(normalized) {
				return errors.New("IM event ID collides with a different payload")
			}
		}
		return nil
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return err
	}
	var pending int
	if err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM im_inbox WHERE binding_id = ? AND state IN ('queued', 'claimed', 'submitting')`, message.BindingID).Scan(&pending); err != nil {
		return err
	}
	if pending >= s.maxPendingPerBinding {
		return channel.ErrInboxFull
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO im_inbox(binding_id, event_id, chat_id, chat_kind, message_json) VALUES (?, ?, ?, ?, ?)`, message.BindingID, message.EventID, message.ChatID, message.ChatKind, data); err != nil {
		return err
	}
	return tx.Commit()
}

func (s *Store) Claim(ctx context.Context, lease time.Duration) (channel.WorkItem, bool, error) {
	if lease < time.Second || lease > 10*time.Minute {
		return channel.WorkItem{}, false, errors.New("IM claim lease must be between one second and ten minutes")
	}
	token, err := newToken()
	if err != nil {
		return channel.WorkItem{}, false, err
	}
	now := time.Now().UnixNano()
	// A worker may disappear without releasing its last claim. Expired claims
	// at the limit become visible failures rather than circulating forever.
	if _, err := s.db.ExecContext(ctx, `UPDATE im_inbox SET state = 'failed', failure = 'maximum claim attempts exceeded', claim_token = '', lease_until = 0
		WHERE state = 'claimed' AND lease_until < ? AND attempts >= ?`, now, s.maxClaimAttempts); err != nil {
		return channel.WorkItem{}, false, err
	}
	var data []byte
	err = s.db.QueryRowContext(ctx, `UPDATE im_inbox
		SET state = 'claimed', claim_token = ?, lease_until = ?, available_at = 0, attempts = attempts + 1
		WHERE rowid = (SELECT candidate.rowid FROM im_inbox AS candidate
			WHERE (candidate.state = 'queued' OR (candidate.state = 'claimed' AND candidate.lease_until < ?))
			AND candidate.available_at <= ?
			AND candidate.attempts < ?
			AND NOT EXISTS (
				SELECT 1 FROM im_inbox AS earlier
				WHERE earlier.binding_id = candidate.binding_id AND earlier.chat_id = candidate.chat_id
				AND earlier.state = 'claimed' AND earlier.rowid < candidate.rowid
			)
			AND (candidate.session_hash = '' OR NOT EXISTS (
				SELECT 1 FROM im_inbox AS earlier
				WHERE earlier.session_hash = candidate.session_hash AND earlier.rowid < candidate.rowid
				AND earlier.state IN ('queued', 'claimed', 'submitting', 'unknown')
			))
			ORDER BY candidate.created_at, candidate.rowid LIMIT 1)
		RETURNING message_json`, token, now+int64(lease), now, now, s.maxClaimAttempts).Scan(&data)
	if errors.Is(err, sql.ErrNoRows) {
		return channel.WorkItem{}, false, nil
	}
	if err != nil {
		return channel.WorkItem{}, false, err
	}
	var message channel.InboundMessage
	if err := json.Unmarshal(data, &message); err != nil {
		return channel.WorkItem{}, false, err
	}
	return channel.WorkItem{Message: message, Token: token}, true, nil
}

func newToken() (string, error) {
	var random [16]byte
	if _, err := rand.Read(random[:]); err != nil {
		return "", err
	}
	return hex.EncodeToString(random[:]), nil
}

func (s *Store) PrepareRoute(ctx context.Context, item channel.WorkItem, key channel.SessionKey) (bool, error) {
	if item.Token == "" || item.Message.BindingID == "" || item.Message.EventID == "" || !validSessionKey(key) || key.BindingID != item.Message.BindingID || key.ChatID != item.Message.ChatID {
		return false, errors.New("IM route preparation identity is incomplete")
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return false, err
	}
	defer tx.Rollback()
	result, err := tx.ExecContext(ctx, `UPDATE im_inbox SET session_hash = ?
		WHERE binding_id = ? AND event_id = ? AND state = 'claimed' AND claim_token = ?
		AND lease_until >= ? AND (session_hash = '' OR session_hash = ?)`, key.String(), item.Message.BindingID, item.Message.EventID, item.Token, time.Now().UnixNano(), key.String())
	if err != nil {
		return false, err
	}
	affected, err := result.RowsAffected()
	if err != nil {
		return false, err
	}
	if affected != 1 {
		return false, channel.ErrClaimLost
	}
	var earlier bool
	if err := tx.QueryRowContext(ctx, `SELECT EXISTS (
		SELECT 1 FROM im_inbox AS preceding
		WHERE preceding.session_hash = ? AND preceding.rowid < (
			SELECT rowid FROM im_inbox WHERE binding_id = ? AND event_id = ?)
		AND preceding.state IN ('queued', 'claimed', 'submitting', 'unknown')
	)`, key.String(), item.Message.BindingID, item.Message.EventID).Scan(&earlier); err != nil {
		return false, err
	}
	if err := tx.Commit(); err != nil {
		return false, err
	}
	return earlier, nil
}

func (s *Store) ReleaseClaim(ctx context.Context, item channel.WorkItem) error {
	if item.Token == "" || item.Message.BindingID == "" || item.Message.EventID == "" {
		return errors.New("IM work item identity is incomplete")
	}
	result, err := s.db.ExecContext(ctx, `UPDATE im_inbox
		SET state = CASE WHEN attempts >= ? THEN 'failed' ELSE 'queued' END,
		failure = CASE WHEN attempts >= ? THEN 'maximum claim attempts exceeded' ELSE '' END,
		claim_token = '', lease_until = 0
		WHERE binding_id = ? AND event_id = ? AND state = 'claimed' AND claim_token = ?`,
		s.maxClaimAttempts, s.maxClaimAttempts, item.Message.BindingID, item.Message.EventID, item.Token)
	if err != nil {
		return err
	}
	affected, err := result.RowsAffected()
	if err != nil {
		return err
	}
	if affected != 1 {
		return channel.ErrClaimLost
	}
	return nil
}

func (s *Store) DeferClaim(ctx context.Context, item channel.WorkItem, delay time.Duration) error {
	if item.Token == "" || item.Message.BindingID == "" || item.Message.EventID == "" || delay <= 0 || delay > 10*time.Minute {
		return errors.New("IM deferred claim identity or delay is invalid")
	}
	result, err := s.db.ExecContext(ctx, `UPDATE im_inbox SET state = 'queued', attempts = attempts - 1,
		claim_token = '', lease_until = 0, available_at = ?
		WHERE binding_id = ? AND event_id = ? AND state = 'claimed' AND claim_token = ? AND attempts > 0`,
		time.Now().Add(delay).UnixNano(), item.Message.BindingID, item.Message.EventID, item.Token)
	if err != nil {
		return err
	}
	affected, err := result.RowsAffected()
	if err != nil {
		return err
	}
	if affected != 1 {
		return channel.ErrClaimLost
	}
	return nil
}

func (s *Store) Ignore(ctx context.Context, item channel.WorkItem) error {
	return s.transition(ctx, item, "claimed", "complete", "", true)
}

func (s *Store) BeginSubmission(ctx context.Context, item channel.WorkItem) error {
	if item.Message.BindingRevision == 0 {
		return s.transition(ctx, item, "claimed", "submitting", "", true)
	}
	if item.Token == "" || item.Message.BindingID == "" || item.Message.EventID == "" {
		return errors.New("IM work item identity is incomplete")
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	// Serialize with Binding updates. The worker may have looked up a healthy
	// Binding before a concurrent rotation; this is its last pre-Agent gate.
	locked, err := tx.ExecContext(ctx, `UPDATE im_bindings SET revision = revision WHERE id = ?`, item.Message.BindingID)
	if err != nil {
		return err
	}
	affected, err := locked.RowsAffected()
	if err != nil {
		return err
	}
	if affected != 1 {
		return channel.ErrBindingChanged
	}
	var bindingJSON []byte
	var revision int64
	if err := tx.QueryRowContext(ctx, `SELECT binding_json, revision FROM im_bindings WHERE id = ?`, item.Message.BindingID).Scan(&bindingJSON, &revision); err != nil {
		return err
	}
	var binding channel.BotBinding
	if err := json.Unmarshal(bindingJSON, &binding); err != nil {
		return err
	}
	if !binding.Enabled || binding.ID != item.Message.BindingID || revision != item.Message.BindingRevision || binding.Revision != revision {
		return channel.ErrBindingChanged
	}
	result, err := tx.ExecContext(ctx, `UPDATE im_inbox SET state = 'submitting', failure = '', lease_until = 0
		WHERE binding_id = ? AND event_id = ? AND state = 'claimed' AND claim_token = ? AND lease_until >= ?`,
		item.Message.BindingID, item.Message.EventID, item.Token, time.Now().UnixNano())
	if err != nil {
		return err
	}
	affected, err = result.RowsAffected()
	if err != nil {
		return err
	}
	if affected != 1 {
		return channel.ErrClaimLost
	}
	return tx.Commit()
}

func (s *Store) Complete(ctx context.Context, item channel.WorkItem) error {
	return s.transition(ctx, item, "submitting", "complete", "", false)
}

func (s *Store) MarkFailed(ctx context.Context, item channel.WorkItem, reason string) error {
	if len(reason) > 1024 {
		reason = reason[:1024]
	}
	return s.transition(ctx, item, "submitting", "failed", reason, false)
}

func (s *Store) MarkUnknown(ctx context.Context, item channel.WorkItem, reason string) error {
	if len(reason) > 1024 {
		reason = reason[:1024]
	}
	return s.transition(ctx, item, "submitting", "unknown", reason, false)
}

func (s *Store) transition(ctx context.Context, item channel.WorkItem, from, to, reason string, requireLease bool) error {
	if item.Token == "" || item.Message.BindingID == "" || item.Message.EventID == "" {
		return errors.New("IM work item identity is incomplete")
	}
	query := `UPDATE im_inbox SET state = ?, failure = ?, lease_until = 0
		WHERE binding_id = ? AND event_id = ? AND state = ? AND claim_token = ?`
	args := []any{to, reason, item.Message.BindingID, item.Message.EventID, from, item.Token}
	if requireLease {
		query += ` AND lease_until >= ?`
		args = append(args, time.Now().UnixNano())
	}
	result, err := s.db.ExecContext(ctx, query, args...)
	if err != nil {
		return err
	}
	affected, err := result.RowsAffected()
	if err != nil {
		return err
	}
	if affected != 1 {
		if from == "claimed" && to == "submitting" {
			return channel.ErrClaimLost
		}
		return errors.New("IM work item state or lease conflict")
	}
	return nil
}

var _ channel.InboxStore = (*Store)(nil)
var _ channel.WorkQueue = (*Store)(nil)
