package sqlite

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"

	"github.com/aiomni/dune/im/channel"
)

// ReconcileConfirmedDelivery atomically finishes an unknown event and its
// conversation only when the matching delivery is already durably complete.
// There is no remote call or replay in this path.
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
	if inboxState != "unknown" {
		return errors.New("IM event is not fenced as unknown")
	}
	var keyJSON []byte
	var conversationState, currentEventID string
	if err := tx.QueryRowContext(ctx, `SELECT key_json, state, current_event_id FROM im_conversations WHERE key_hash = ? AND binding_id = ? AND chat_id = ?`, key.String(), key.BindingID, key.ChatID).Scan(&keyJSON, &conversationState, &currentEventID); err != nil {
		return err
	}
	var storedKey channel.SessionKey
	if err := json.Unmarshal(keyJSON, &storedKey); err != nil {
		return err
	}
	if storedKey != key || conversationState != "unknown" || currentEventID != eventID {
		return errors.New("IM unknown conversation does not match the event")
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
	if err := updateRecoveryRow(ctx, tx, `UPDATE im_inbox SET state = 'complete', failure = '', claim_token = '', lease_until = 0
		WHERE binding_id = ? AND event_id = ? AND state = 'unknown'`, key.BindingID, eventID); err != nil {
		return err
	}
	if err := updateRecoveryRow(ctx, tx, `UPDATE im_conversations SET state = 'ready', current_event_id = '', failure = '', lease_token = '', lease_until = 0
		WHERE key_hash = ? AND state = 'unknown' AND current_event_id = ?`, key.String(), eventID); err != nil {
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
