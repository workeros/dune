package sqlite

import (
	"context"
	"errors"

	"github.com/aiomni/dune/im/channel"
)

func (s *Store) BindingStats(ctx context.Context, bindingID string) (channel.BindingStats, error) {
	var stats channel.BindingStats
	if bindingID == "" {
		return stats, errors.New("IM binding ID is required")
	}
	err := s.db.QueryRowContext(ctx, `SELECT
		COUNT(CASE WHEN state = 'queued' THEN 1 END),
		COUNT(CASE WHEN state = 'claimed' THEN 1 END),
		COUNT(CASE WHEN state = 'submitting' THEN 1 END),
		COUNT(CASE WHEN state = 'failed' THEN 1 END),
		COUNT(CASE WHEN state = 'unknown' THEN 1 END)
		FROM im_inbox WHERE binding_id = ?`, bindingID).Scan(&stats.Queued, &stats.Claimed, &stats.Submitting, &stats.FailedEvents, &stats.UnknownEvents)
	if err != nil {
		return channel.BindingStats{}, err
	}
	if err := s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM im_conversations WHERE binding_id = ? AND state = 'unknown'`, bindingID).Scan(&stats.UnknownConversations); err != nil {
		return channel.BindingStats{}, err
	}
	if err := s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM im_deliveries WHERE binding_id = ? AND json_extract(state_json, '$.phase') = 'unknown'`, bindingID).Scan(&stats.UnknownDeliveries); err != nil {
		return channel.BindingStats{}, err
	}
	if err := s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM im_deliveries WHERE binding_id = ? AND json_extract(state_json, '$.phase') = 'failed'`, bindingID).Scan(&stats.FailedDeliveries); err != nil {
		return channel.BindingStats{}, err
	}
	return stats, nil
}

var _ channel.StatusStore = (*Store)(nil)
