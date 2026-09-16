package sqlite

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/aiomni/dune/im/channel"
)

func (s *Store) Reserve(ctx context.Context, initial channel.Delivery) (channel.Delivery, bool, error) {
	if initial.ID == "" || !validSessionKey(initial.Session) || initial.Mode == "" || initial.ProviderStateVersion < 1 {
		return channel.Delivery{}, false, errors.New("IM delivery identity or configuration is incomplete")
	}
	initial.Phase, initial.Revision = "reserved", 1
	data, err := json.Marshal(initial)
	if err != nil {
		return channel.Delivery{}, false, err
	}
	result, err := s.db.ExecContext(ctx, `INSERT OR IGNORE INTO im_deliveries(binding_id, delivery_id, key_hash, state_json, revision) VALUES (?, ?, ?, ?, 1)`, initial.Session.BindingID, initial.ID, initial.Session.String(), data)
	if err != nil {
		return channel.Delivery{}, false, err
	}
	created, err := result.RowsAffected()
	if err != nil {
		return channel.Delivery{}, false, err
	}
	var storedJSON []byte
	err = s.db.QueryRowContext(ctx, `SELECT state_json FROM im_deliveries WHERE binding_id = ? AND delivery_id = ?`, initial.Session.BindingID, initial.ID).Scan(&storedJSON)
	if err != nil {
		return channel.Delivery{}, false, err
	}
	var stored channel.Delivery
	if err := json.Unmarshal(storedJSON, &stored); err != nil {
		return channel.Delivery{}, false, err
	}
	a, _ := json.Marshal(initial.Address)
	b, _ := json.Marshal(stored.Address)
	if stored.Session != initial.Session || stored.Mode != initial.Mode || string(a) != string(b) || stored.ProviderStateVersion != initial.ProviderStateVersion {
		return channel.Delivery{}, false, errors.New("IM DeliveryID conflicts with a different session, address or mode")
	}
	return stored, created == 1, nil
}

func (s *Store) Commit(ctx context.Context, state channel.Delivery) (channel.Delivery, error) {
	if state.ID == "" || !validSessionKey(state.Session) || state.Revision < 1 || state.Phase == "" {
		return channel.Delivery{}, errors.New("IM delivery identity, phase or revision is invalid")
	}
	var previousJSON []byte
	if err := s.db.QueryRowContext(ctx, `SELECT state_json FROM im_deliveries WHERE binding_id = ? AND delivery_id = ? AND key_hash = ?`, state.Session.BindingID, state.ID, state.Session.String()).Scan(&previousJSON); err != nil {
		return channel.Delivery{}, err
	}
	var previousState channel.Delivery
	if err := json.Unmarshal(previousJSON, &previousState); err != nil {
		return channel.Delivery{}, err
	}
	if previousState.Revision != state.Revision {
		return channel.Delivery{}, errors.New("IM delivery revision conflict")
	}
	if previousState.ID != state.ID || previousState.Session != state.Session || previousState.Mode != state.Mode || previousState.ProviderStateVersion != state.ProviderStateVersion || !sameReplyAddress(previousState.Address, state.Address) {
		return channel.Delivery{}, errors.New("IM delivery identity, address, mode or provider state version cannot change")
	}
	if err := validateDeliveryTransition(previousState, state); err != nil {
		return channel.Delivery{}, err
	}
	previous := state.Revision
	state.Revision++
	data, err := json.Marshal(state)
	if err != nil {
		return channel.Delivery{}, err
	}
	result, err := s.db.ExecContext(ctx, `UPDATE im_deliveries SET state_json = ?, revision = ? WHERE binding_id = ? AND delivery_id = ? AND key_hash = ? AND revision = ?`, data, state.Revision, state.Session.BindingID, state.ID, state.Session.String(), previous)
	if err != nil {
		return channel.Delivery{}, err
	}
	affected, err := result.RowsAffected()
	if err != nil {
		return channel.Delivery{}, err
	}
	if affected != 1 {
		return channel.Delivery{}, errors.New("IM delivery revision conflict")
	}
	return state, nil
}

func sameReplyAddress(a, b channel.ReplyAddress) bool {
	if a.Provider != b.Provider || a.Version != b.Version {
		return false
	}
	left, leftErr := json.Marshal(a.Data)
	right, rightErr := json.Marshal(b.Data)
	return leftErr == nil && rightErr == nil && string(left) == string(right)
}

func validateDeliveryTransition(before, after channel.Delivery) error {
	switch before.Phase {
	case "reserved", "active":
		if after.Phase == "pending" && after.Operation != "" {
			return nil
		}
	case "pending":
		if before.Operation == "" {
			break
		}
		if after.Phase == "unknown" && after.Operation == before.Operation {
			return nil
		}
		if (after.Phase == "active" || after.Phase == "complete") && after.Operation == "" {
			return nil
		}
	}
	return fmt.Errorf("invalid IM delivery transition %q/%q to %q/%q", before.Phase, before.Operation, after.Phase, after.Operation)
}

func (s *Store) GetDelivery(ctx context.Context, bindingID, deliveryID string) (channel.Delivery, bool, error) {
	var data []byte
	err := s.db.QueryRowContext(ctx, `SELECT state_json FROM im_deliveries WHERE binding_id = ? AND delivery_id = ?`, bindingID, deliveryID).Scan(&data)
	if errors.Is(err, sql.ErrNoRows) {
		return channel.Delivery{}, false, nil
	}
	if err != nil {
		return channel.Delivery{}, false, err
	}
	var state channel.Delivery
	if err := json.Unmarshal(data, &state); err != nil {
		return channel.Delivery{}, false, err
	}
	return state, true, nil
}

var _ channel.DeliveryStore = (*Store)(nil)
