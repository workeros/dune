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
		var rawFailure string
		if err := rows.Scan(&issue.EventID, &issue.ChatID, &issue.State, &issue.Attempts, &rawFailure); err != nil {
			return err
		}
		issue.Failure = safeIssueFailure(rawFailure)
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
		var rawFailure string
		if err := rows.Scan(&keyJSON, &issue.State, &issue.CurrentEventID, &rawFailure); err != nil {
			return err
		}
		issue.Failure = safeIssueFailure(rawFailure)
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

// Backend and platform errors can quote a user's prompt or the Agent's
// answer. Only reasons produced by our own fixed state transitions are safe
// to expose through the read-only diagnostics API.
func safeIssueFailure(raw string) string {
	switch raw {
	case "", "maximum claim attempts exceeded", "IM Agent or delivery outcome unknown; inspect the turn and delivery", "IM submission barrier outcome unknown", "IM outbound rejected before platform request":
		return raw
	default:
		return "details withheld; inspect server logs"
	}
}

func (s *Store) listDeliveryIssues(ctx context.Context, bindingID string, limit int, issues *channel.BindingIssues) error {
	rows, err := s.db.QueryContext(ctx, `SELECT delivery_id, state_json FROM im_deliveries
		WHERE binding_id = ? AND json_extract(state_json, '$.phase') IN ('pending', 'unknown', 'failed')
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
		var delivery channel.Delivery
		if err := json.Unmarshal(data, &delivery); err != nil {
			return err
		}
		if delivery.ID != id || delivery.Session.BindingID != bindingID || !validSessionKey(delivery.Session) {
			return fmt.Errorf("IM issue points to an invalid delivery %q", id)
		}
		issues.Deliveries = append(issues.Deliveries, channel.DeliveryIssue{
			ID: delivery.ID, Session: delivery.Session, Mode: delivery.Mode,
			Phase: delivery.Phase, Operation: delivery.Operation,
			AgentTurnCompleted: delivery.AgentTurnCompleted,
		})
	}
	return rows.Err()
}

var _ channel.IssueStore = (*Store)(nil)
