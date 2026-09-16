package sqlite

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/aiomni/dune/im/channel"
)

// ListIssues bounds each category independently so a large inbox backlog
// cannot hide an uncertain conversation or outbound delivery.
func (s *Store) ListIssues(ctx context.Context, bindingID string, perCategoryLimit int) (channel.BindingIssues, error) {
	var issues channel.BindingIssues
	if bindingID == "" || perCategoryLimit < 1 || perCategoryLimit > 100 {
		return issues, errors.New("IM issue lookup requires a Binding ID and a 1..100 per-category limit")
	}
	if err := s.listEventIssues(ctx, bindingID, perCategoryLimit, &issues); err != nil {
		return channel.BindingIssues{}, err
	}
	if err := s.listConversationIssues(ctx, bindingID, perCategoryLimit, &issues); err != nil {
		return channel.BindingIssues{}, err
	}
	if err := s.listDeliveryIssues(ctx, bindingID, perCategoryLimit, &issues); err != nil {
		return channel.BindingIssues{}, err
	}
	return issues, nil
}

func (s *Store) listEventIssues(ctx context.Context, bindingID string, limit int, issues *channel.BindingIssues) error {
	rows, err := s.db.QueryContext(ctx, `SELECT event_id, chat_id, state, attempts, failure FROM im_inbox
		WHERE binding_id = ? AND state IN ('submitting', 'unknown', 'failed')
		ORDER BY created_at, rowid LIMIT ?`, bindingID, limit)
	if err != nil {
		return err
	}
	defer rows.Close()
	for rows.Next() {
		var issue channel.EventIssue
		if err := rows.Scan(&issue.EventID, &issue.ChatID, &issue.State, &issue.Attempts, &issue.Failure); err != nil {
			return err
		}
		issues.Events = append(issues.Events, issue)
	}
	return rows.Err()
}

func (s *Store) listConversationIssues(ctx context.Context, bindingID string, limit int, issues *channel.BindingIssues) error {
	rows, err := s.db.QueryContext(ctx, `SELECT key_json, state, current_event_id, failure FROM im_conversations
		WHERE binding_id = ? AND state IN ('running', 'unknown') ORDER BY key_hash LIMIT ?`, bindingID, limit)
	if err != nil {
		return err
	}
	defer rows.Close()
	for rows.Next() {
		var keyJSON []byte
		var issue channel.ConversationIssue
		if err := rows.Scan(&keyJSON, &issue.State, &issue.CurrentEventID, &issue.Failure); err != nil {
			return err
		}
		if err := json.Unmarshal(keyJSON, &issue.Key); err != nil {
			return err
		}
		if !validSessionKey(issue.Key) || issue.Key.BindingID != bindingID {
			return errors.New("IM issue points to an invalid conversation key")
		}
		issues.Conversations = append(issues.Conversations, issue)
	}
	return rows.Err()
}

func (s *Store) listDeliveryIssues(ctx context.Context, bindingID string, limit int, issues *channel.BindingIssues) error {
	rows, err := s.db.QueryContext(ctx, `SELECT delivery_id, state_json FROM im_deliveries
		WHERE binding_id = ? AND json_extract(state_json, '$.phase') IN ('pending', 'unknown')
		ORDER BY delivery_id LIMIT ?`, bindingID, limit)
	if err != nil {
		return err
	}
	defer rows.Close()
	for rows.Next() {
		var id string
		var data []byte
		if err := rows.Scan(&id, &data); err != nil {
			return err
		}
		var issue channel.Delivery
		if err := json.Unmarshal(data, &issue); err != nil {
			return err
		}
		if issue.ID != id || issue.Session.BindingID != bindingID || !validSessionKey(issue.Session) {
			return fmt.Errorf("IM issue points to an invalid delivery %q", id)
		}
		issues.Deliveries = append(issues.Deliveries, issue)
	}
	return rows.Err()
}

var _ channel.IssueStore = (*Store)(nil)
