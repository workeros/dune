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
// delivery is durably complete. This also covers a process crash between
// confirming delivery and marking the Inbox complete: the event remains in
// submitting, but must not be replayed. A still-running conversation requires
// an expired lease so this cannot interrupt an active worker. There is no
// remote call or replay in this path.
func (s *Store) ReconcileConfirmedDelivery(ctx context.Context, key channel.SessionKey, eventID string) error {
	return s.reconcileTerminalDelivery(ctx, key, eventID, "complete")
}

// ReconcileRejectedDelivery releases a session after the Agent finished but
// the Provider durably confirmed that no platform request was made.
func (s *Store) ReconcileRejectedDelivery(ctx context.Context, key channel.SessionKey, eventID string) error {
	return s.reconcileTerminalDelivery(ctx, key, eventID, "failed")
}

func (s *Store) reconcileTerminalDelivery(ctx context.Context, key channel.SessionKey, eventID, terminal string) error {
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
	if inboxState != "submitting" && inboxState != "unknown" && inboxState != terminal {
		return errors.New("IM event is not awaiting completion reconciliation")
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
			return errors.New("IM matching terminal delivery was not found")
		}
		return err
	}
	var delivery channel.Delivery
	if err := json.Unmarshal(deliveryJSON, &delivery); err != nil {
		return err
	}
	if delivery.ID != deliveryID || delivery.Session != key || delivery.Phase != terminal || delivery.Operation != "" || !delivery.AgentTurnCompleted {
		return errors.New("IM matching delivery lacks confirmed Agent completion and terminal outcome")
	}
	if inboxState != terminal {
		failure := ""
		if terminal == "failed" {
			failure = "IM outbound rejected before platform request"
		}
		if err := updateRecoveryRow(ctx, tx, `UPDATE im_inbox SET state = ?, failure = ?, claim_token = '', lease_until = 0
			WHERE binding_id = ? AND event_id = ? AND state = ?`, terminal, failure, key.BindingID, eventID, inboxState); err != nil {
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
