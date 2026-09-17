package sqlite

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/aiomni/dune/im/channel"
)

func validSessionKey(key channel.SessionKey) bool {
	return key.TenantID != "" && key.BindingID != "" && key.ChatID != "" && key.SubjectID != ""
}

func (s *Store) Ensure(ctx context.Context, key channel.SessionKey, target channel.AgentTarget, address channel.ReplyAddress, threadRef string) (channel.ConversationSession, error) {
	if !validSessionKey(key) || target.RunnerID == "" || target.ProfileID == "" || target.ProfileRevision < 1 {
		return channel.ConversationSession{}, errors.New("IM conversation key or Agent target is incomplete")
	}
	keyJSON, err := json.Marshal(key)
	if err != nil {
		return channel.ConversationSession{}, err
	}
	initial := channel.ConversationSession{Key: key, Target: target, Address: address, ProviderThreadRef: threadRef, Revision: 1}
	sessionJSON, err := json.Marshal(initial)
	if err != nil {
		return channel.ConversationSession{}, err
	}
	var nullableRef any
	if threadRef != "" {
		nullableRef = threadRef
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return channel.ConversationSession{}, err
	}
	defer tx.Rollback()
	if _, err := tx.ExecContext(ctx, `INSERT OR IGNORE INTO im_conversations(key_hash, binding_id, chat_id, provider_thread_ref, key_json, session_json, revision) VALUES (?, ?, ?, ?, ?, ?, 1)`, key.String(), key.BindingID, key.ChatID, nullableRef, keyJSON, sessionJSON); err != nil {
		return channel.ConversationSession{}, err
	}
	if threadRef != "" {
		result, err := tx.ExecContext(ctx, `UPDATE im_conversations SET provider_thread_ref = ? WHERE key_hash = ? AND (provider_thread_ref IS NULL OR provider_thread_ref = ?)`, threadRef, key.String(), threadRef)
		if err != nil {
			return channel.ConversationSession{}, fmt.Errorf("bind provider thread ref: %w", err)
		}
		if affected, err := result.RowsAffected(); err != nil || affected != 1 {
			return channel.ConversationSession{}, errors.New("provider thread ref conflicts with existing conversation")
		}
	}
	var storedJSON []byte
	var revision, leaseUntil int64
	var state, leaseToken string
	if err := tx.QueryRowContext(ctx, `SELECT session_json, revision, state, lease_token, lease_until FROM im_conversations WHERE key_hash = ?`, key.String()).
		Scan(&storedJSON, &revision, &state, &leaseToken, &leaseUntil); err != nil {
		return channel.ConversationSession{}, err
	}
	var stored channel.ConversationSession
	if err := json.Unmarshal(storedJSON, &stored); err != nil {
		return channel.ConversationSession{}, err
	}
	if stored.Key != key || stored.Revision != revision {
		return channel.ConversationSession{}, errors.New("IM conversation storage identity mismatch")
	}
	// A turn rejected before Agent startup may leave an empty conversation.
	// It has no Agent context to preserve, so a later Binding revision may
	// retarget it. Established Runtime sessions always keep their snapshot.
	if stored.Target != target && stored.Runtime.ID == "" && stored.ACPSessionID == "" && state == string(channel.ConversationReady) && (leaseToken == "" || leaseUntil < time.Now().UnixNano()) {
		stored.Target, stored.Address, stored.Revision = target, address, revision+1
		updatedJSON, err := json.Marshal(stored)
		if err != nil {
			return channel.ConversationSession{}, err
		}
		result, err := tx.ExecContext(ctx, `UPDATE im_conversations SET session_json = ?, revision = ? WHERE key_hash = ? AND revision = ?`,
			updatedJSON, stored.Revision, key.String(), revision)
		if err != nil {
			return channel.ConversationSession{}, err
		}
		if affected, err := result.RowsAffected(); err != nil || affected != 1 {
			return channel.ConversationSession{}, errors.New("IM empty conversation target changed concurrently")
		}
	}
	if err := tx.Commit(); err != nil {
		return channel.ConversationSession{}, err
	}
	stored, _, found, err := s.Get(ctx, key)
	if err != nil {
		return channel.ConversationSession{}, err
	}
	if !found {
		return channel.ConversationSession{}, errors.New("IM conversation disappeared after creation")
	}
	return stored, nil
}

func (s *Store) FindByThreadRef(ctx context.Context, bindingID, chatID, threadRef string) (channel.SessionKey, bool, error) {
	if bindingID == "" || chatID == "" || threadRef == "" {
		return channel.SessionKey{}, false, errors.New("provider thread lookup key is incomplete")
	}
	var data []byte
	err := s.db.QueryRowContext(ctx, `SELECT key_json FROM im_conversations WHERE binding_id = ? AND chat_id = ? AND provider_thread_ref = ?`, bindingID, chatID, threadRef).Scan(&data)
	if errors.Is(err, sql.ErrNoRows) {
		return channel.SessionKey{}, false, nil
	}
	if err != nil {
		return channel.SessionKey{}, false, err
	}
	var key channel.SessionKey
	if err := json.Unmarshal(data, &key); err != nil {
		return channel.SessionKey{}, false, err
	}
	if !validSessionKey(key) || key.BindingID != bindingID || key.ChatID != chatID {
		return channel.SessionKey{}, false, errors.New("provider thread ref points to invalid conversation")
	}
	return key, true, nil
}

func (s *Store) Get(ctx context.Context, key channel.SessionKey) (channel.ConversationSession, channel.ConversationState, bool, error) {
	if !validSessionKey(key) {
		return channel.ConversationSession{}, "", false, errors.New("IM conversation key is incomplete")
	}
	var keyJSON, sessionJSON []byte
	var state string
	var threadRef sql.NullString
	err := s.db.QueryRowContext(ctx, `SELECT key_json, session_json, state, provider_thread_ref FROM im_conversations WHERE key_hash = ?`, key.String()).Scan(&keyJSON, &sessionJSON, &state, &threadRef)
	if errors.Is(err, sql.ErrNoRows) {
		return channel.ConversationSession{}, "", false, nil
	}
	if err != nil {
		return channel.ConversationSession{}, "", false, err
	}
	var storedKey channel.SessionKey
	var session channel.ConversationSession
	if err := json.Unmarshal(keyJSON, &storedKey); err != nil {
		return channel.ConversationSession{}, "", false, err
	}
	if err := json.Unmarshal(sessionJSON, &session); err != nil {
		return channel.ConversationSession{}, "", false, err
	}
	if storedKey != key || session.Key != key {
		return channel.ConversationSession{}, "", false, errors.New("IM conversation hash collision or corrupt session key")
	}
	session.ProviderThreadRef = threadRef.String
	if state != string(channel.ConversationReady) && state != string(channel.ConversationRunning) && state != string(channel.ConversationUnknown) {
		return channel.ConversationSession{}, "", false, fmt.Errorf("unknown IM conversation state %q", state)
	}
	return session, channel.ConversationState(state), true, nil
}

func (s *Store) Acquire(ctx context.Context, key channel.SessionKey, duration time.Duration) (channel.ConversationLease, bool, error) {
	if !validSessionKey(key) || !validLeaseDuration(duration) {
		return channel.ConversationLease{}, false, errors.New("invalid IM conversation key or lease duration")
	}
	token, err := newToken()
	if err != nil {
		return channel.ConversationLease{}, false, err
	}
	now := time.Now().UnixNano()
	result, err := s.db.ExecContext(ctx, `UPDATE im_conversations SET lease_token = ?, lease_until = ?
		WHERE key_hash = ? AND state = 'ready' AND (lease_token = '' OR lease_until < ?)`, token, now+int64(duration), key.String(), now)
	if err != nil {
		return channel.ConversationLease{}, false, err
	}
	affected, err := result.RowsAffected()
	if err != nil {
		return channel.ConversationLease{}, false, err
	}
	if affected != 1 {
		return channel.ConversationLease{}, false, nil
	}
	return channel.ConversationLease{Key: key, Token: token}, true, nil
}

func (s *Store) Renew(ctx context.Context, lease channel.ConversationLease, duration time.Duration) error {
	if !validLeaseDuration(duration) {
		return errors.New("invalid IM conversation lease duration")
	}
	return s.leaseUpdate(ctx, lease, `UPDATE im_conversations SET lease_until = ?
		WHERE key_hash = ? AND lease_token = ? AND state IN ('ready', 'running')`, time.Now().Add(duration).UnixNano())
}

func (s *Store) Save(ctx context.Context, lease channel.ConversationLease, session channel.ConversationSession) (channel.ConversationSession, error) {
	if lease.Token == "" || !validSessionKey(lease.Key) || session.Key != lease.Key || session.Revision < 1 {
		return channel.ConversationSession{}, errors.New("invalid IM conversation save")
	}
	previous := session.Revision
	session.Revision++
	data, err := json.Marshal(session)
	if err != nil {
		return channel.ConversationSession{}, err
	}
	result, err := s.db.ExecContext(ctx, `UPDATE im_conversations SET session_json = ?, revision = ?
		WHERE key_hash = ? AND lease_token = ? AND revision = ?
		AND (state = 'running' OR (state = 'ready' AND lease_until >= ?))`,
		data, session.Revision, lease.Key.String(), lease.Token, previous, time.Now().UnixNano())
	if err != nil {
		return channel.ConversationSession{}, err
	}
	affected, err := result.RowsAffected()
	if err != nil {
		return channel.ConversationSession{}, err
	}
	if affected != 1 {
		return channel.ConversationSession{}, errors.New("IM conversation lease or revision conflict")
	}
	return session, nil
}

func (s *Store) BeginTurn(ctx context.Context, lease channel.ConversationLease, eventID string) error {
	if eventID == "" {
		return errors.New("IM turn event ID is required")
	}
	return s.leaseUpdate(ctx, lease, `UPDATE im_conversations SET state = 'running', current_event_id = ?, failure = ''
		WHERE state = 'ready' AND lease_until >= ? AND key_hash = ? AND lease_token = ?`, eventID, time.Now().UnixNano())
}

func (s *Store) FinishTurn(ctx context.Context, lease channel.ConversationLease) error {
	return s.leaseUpdate(ctx, lease, `UPDATE im_conversations SET state = 'ready', current_event_id = '', failure = ''
		WHERE key_hash = ? AND lease_token = ? AND state = 'running'`)
}

func (s *Store) UnknownTurn(ctx context.Context, lease channel.ConversationLease, reason string) error {
	if len(reason) > 1024 {
		reason = reason[:1024]
	}
	return s.leaseUpdate(ctx, lease, `UPDATE im_conversations SET state = 'unknown', failure = ?
		WHERE key_hash = ? AND lease_token = ? AND state = 'running'`, reason)
}

func (s *Store) ReleaseLease(ctx context.Context, lease channel.ConversationLease) error {
	return s.leaseUpdate(ctx, lease, `UPDATE im_conversations SET lease_token = '', lease_until = 0
		WHERE key_hash = ? AND lease_token = ? AND state = 'ready'`)
}

func (s *Store) leaseUpdate(ctx context.Context, lease channel.ConversationLease, query string, prefix ...any) error {
	if !validSessionKey(lease.Key) || lease.Token == "" {
		return errors.New("IM conversation lease is incomplete")
	}
	args := append(prefix, lease.Key.String(), lease.Token)
	result, err := s.db.ExecContext(ctx, query, args...)
	if err != nil {
		return err
	}
	affected, err := result.RowsAffected()
	if err != nil {
		return err
	}
	if affected != 1 {
		return errors.New("IM conversation state or lease conflict")
	}
	return nil
}

func validLeaseDuration(duration time.Duration) bool {
	return duration >= time.Second && duration <= 10*time.Minute
}

var _ channel.ConversationStore = (*Store)(nil)
