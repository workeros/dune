package sqlite

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"time"

	"github.com/aiomni/dune/im/channel"
)

// ReconcileConfirmedDelivery atomically finishes bookkeeping after a matching
// delivery is durably complete. A still-running conversation requires an
// expired lease so this cannot interrupt an active worker. There is no remote
// call or replay in this path.
func (s *Store) ReconcileConfirmedDelivery(ctx context.Context, key channel.SessionKey, eventID string) error {
	if !validSessionKey(key) || eventID == "" {
		return errors.New("IM recovery requires a complete session and event ID")
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	var inboxState string
	if err := tx.QueryRowContext(ctx, `SELECT state FROM im_inbox WHERE binding_id = ? AND event_id = ?`, key.BindingID, eventID).Scan(&inboxState); err != nil {
		return err
	}
	if inboxState != "unknown" && inboxState != "complete" {
		return errors.New("IM event is neither unknown nor complete")
	}
	var keyJSON []byte
	var conversationState, currentEventID string
	var leaseUntil int64
	if err := tx.QueryRowContext(ctx, `SELECT key_json, state, current_event_id, lease_until FROM im_conversations WHERE key_hash = ? AND binding_id = ? AND chat_id = ?`, key.String(), key.BindingID, key.ChatID).Scan(&keyJSON, &conversationState, &currentEventID, &leaseUntil); err != nil {
		return err
	}
	var storedKey channel.SessionKey
	if err := json.Unmarshal(keyJSON, &storedKey); err != nil {
		return err
	}
	if storedKey != key || currentEventID != eventID || (conversationState != "unknown" && conversationState != "running") {
		return errors.New("IM unfinished conversation does not match the event")
	}
	if conversationState == "running" && leaseUntil >= time.Now().UnixNano() {
		return errors.New("IM conversation worker lease is still live")
	}
	deliveryID := channel.TurnDeliveryID(key.BindingID, eventID)
	var deliveryJSON []byte
	if err := tx.QueryRowContext(ctx, `SELECT state_json FROM im_deliveries WHERE binding_id = ? AND delivery_id = ? AND key_hash = ?`, key.BindingID, deliveryID, key.String()).Scan(&deliveryJSON); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return errors.New("IM matching delivery is not confirmed complete")
		}
		return err
	}
	var delivery channel.Delivery
	if err := json.Unmarshal(deliveryJSON, &delivery); err != nil {
		return err
	}
	if delivery.ID != deliveryID || delivery.Session != key || delivery.Phase != "complete" || delivery.Operation != "" {
		return errors.New("IM matching delivery is not confirmed complete")
	}
	if inboxState == "unknown" {
		if err := updateRecoveryRow(ctx, tx, `UPDATE im_inbox SET state = 'complete', failure = '', claim_token = '', lease_until = 0
			WHERE binding_id = ? AND event_id = ? AND state = 'unknown'`, key.BindingID, eventID); err != nil {
			return err
		}
	}
	if err := updateRecoveryRow(ctx, tx, `UPDATE im_conversations SET state = 'ready', current_event_id = '', failure = '', lease_token = '', lease_until = 0
		WHERE key_hash = ? AND state = ? AND current_event_id = ? AND lease_until = ?`, key.String(), conversationState, eventID, leaseUntil); err != nil {
		return err
	}
	return tx.Commit()
}

func updateRecoveryRow(ctx context.Context, tx *sql.Tx, query string, args ...any) error {
	result, err := tx.ExecContext(ctx, query, args...)
	if err != nil {
		return err
	}
	affected, err := result.RowsAffected()
	if err != nil {
		return err
	}
	if affected != 1 {
		return errors.New("IM recovery state changed concurrently")
	}
	return nil
}

var _ channel.RecoveryStore = (*Store)(nil)
