package sqlite

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/aiomni/dune/im/channel"
)

func (s *Store) Put(ctx context.Context, binding channel.BotBinding) (channel.BotBinding, error) {
	if binding.ID == "" || binding.TenantID == "" || binding.Provider == "" || binding.CredentialRef == "" ||
		binding.ConfigVersion < 1 || binding.Target.RunnerID == "" || binding.Target.ProfileID == "" || binding.Target.ProfileRevision < 1 ||
		binding.Revision < 0 || !json.Valid(binding.Config) {
		return channel.BotBinding{}, errors.New("IM bot binding is incomplete or invalid")
	}
	previous := binding.Revision
	binding.Revision++
	data, err := json.Marshal(binding)
	if err != nil {
		return channel.BotBinding{}, err
	}
	if previous == 0 {
		_, err = s.db.ExecContext(ctx, `INSERT INTO im_bindings(id, tenant_id, binding_json, revision) VALUES (?, ?, ?, 1)`, binding.ID, binding.TenantID, data)
		if err != nil {
			return channel.BotBinding{}, fmt.Errorf("create IM bot binding: %w", err)
		}
		return binding, nil
	}
	result, err := s.db.ExecContext(ctx, `UPDATE im_bindings SET binding_json = ?, revision = ? WHERE id = ? AND tenant_id = ? AND revision = ?`,
		data, binding.Revision, binding.ID, binding.TenantID, previous)
	if err != nil {
		return channel.BotBinding{}, err
	}
	affected, err := result.RowsAffected()
	if err != nil {
		return channel.BotBinding{}, err
	}
	if affected != 1 {
		return channel.BotBinding{}, errors.New("IM bot binding tenant or revision conflict")
	}
	return binding, nil
}

func (s *Store) GetBinding(ctx context.Context, tenantID, bindingID string) (channel.BotBinding, bool, error) {
	if tenantID == "" || bindingID == "" {
		return channel.BotBinding{}, false, errors.New("IM tenant and binding IDs are required")
	}
	var data []byte
	err := s.db.QueryRowContext(ctx, `SELECT binding_json FROM im_bindings WHERE tenant_id = ? AND id = ?`, tenantID, bindingID).Scan(&data)
	if errors.Is(err, sql.ErrNoRows) {
		return channel.BotBinding{}, false, nil
	}
	if err != nil {
		return channel.BotBinding{}, false, err
	}
	var binding channel.BotBinding
	if err := json.Unmarshal(data, &binding); err != nil {
		return channel.BotBinding{}, false, err
	}
	if binding.ID != bindingID || binding.TenantID != tenantID {
		return channel.BotBinding{}, false, errors.New("IM bot binding storage identity mismatch")
	}
	return binding, true, nil
}

func (s *Store) GetBindingByID(ctx context.Context, bindingID string) (channel.BotBinding, bool, error) {
	if bindingID == "" {
		return channel.BotBinding{}, false, errors.New("IM binding ID is required")
	}
	var tenantID string
	err := s.db.QueryRowContext(ctx, `SELECT tenant_id FROM im_bindings WHERE id = ?`, bindingID).Scan(&tenantID)
	if errors.Is(err, sql.ErrNoRows) {
		return channel.BotBinding{}, false, nil
	}
	if err != nil {
		return channel.BotBinding{}, false, err
	}
	return s.GetBinding(ctx, tenantID, bindingID)
}

func (s *Store) ListBindings(ctx context.Context, tenantID string) ([]channel.BotBinding, error) {
	if tenantID == "" {
		return nil, errors.New("IM tenant ID is required")
	}
	rows, err := s.db.QueryContext(ctx, `SELECT binding_json FROM im_bindings WHERE tenant_id = ? ORDER BY id`, tenantID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var bindings []channel.BotBinding
	for rows.Next() {
		var data []byte
		if err := rows.Scan(&data); err != nil {
			return nil, err
		}
		var binding channel.BotBinding
		if err := json.Unmarshal(data, &binding); err != nil {
			return nil, err
		}
		if binding.TenantID != tenantID {
			return nil, errors.New("IM bot binding escaped tenant scope")
		}
		bindings = append(bindings, binding)
	}
	return bindings, rows.Err()
}

var _ channel.BindingStore = (*Store)(nil)
