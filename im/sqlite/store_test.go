package sqlite

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"testing"
	"time"

	"github.com/aiomni/dune/im/channel"
)

func TestInboxSurvivesReopenAndRejectsEventCollision(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "im.db")
	store, err := Open(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	if info, err := os.Stat(path); err != nil || info.Mode().Perm() != 0600 {
		t.Fatalf("IM inbox file is not owner-private: info=%v err=%v", info, err)
	}
	message := channel.InboundMessage{BindingID: "bot-a", EventID: "evt-1", MessageID: "om-1", SenderID: "user-a", Text: "hello"}
	sink := channel.Ingress{Inbox: store}
	if err := sink.Accept(ctx, message); err != nil {
		t.Fatal(err)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	store, err = Open(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	if err := (channel.Ingress{Inbox: store}).Accept(ctx, message); err != nil {
		t.Fatalf("identical redelivery must be idempotent: %v", err)
	}
	var count int
	if err := store.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM im_inbox`).Scan(&count); err != nil || count != 1 {
		t.Fatalf("redelivery created duplicate work: count=%d err=%v", count, err)
	}
	message.Text = "different"
	if err := (channel.Ingress{Inbox: store}).Accept(ctx, message); err == nil {
		t.Fatal("event ID collision was accepted")
	}
}

func TestInboxCapacityIsPerBindingAndRedeliveryRemainsIdempotent(t *testing.T) {
	ctx := context.Background()
	store, err := OpenWithOptions(ctx, filepath.Join(t.TempDir(), "im.db"), Options{MaxPendingPerBinding: 1})
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	first := channel.InboundMessage{BindingID: "bot-a", EventID: "event-1", MessageID: "message-1"}
	if err := store.Insert(ctx, first); err != nil {
		t.Fatal(err)
	}
	if err := store.Insert(ctx, first); err != nil {
		t.Fatalf("duplicate failed while queue full: %v", err)
	}
	second := channel.InboundMessage{BindingID: "bot-a", EventID: "event-2", MessageID: "message-2"}
	if err := store.Insert(ctx, second); !errors.Is(err, channel.ErrInboxFull) {
		t.Fatalf("new event was not backpressured: %v", err)
	}
	other := second
	other.BindingID = "bot-b"
	if err := store.Insert(ctx, other); err != nil {
		t.Fatalf("another Binding was incorrectly backpressured: %v", err)
	}
	item, found, err := store.Claim(ctx, time.Minute)
	if err != nil || !found || item.Message.EventID != "event-1" {
		t.Fatalf("claim: %+v %t %v", item, found, err)
	}
	if err := store.Ignore(ctx, item); err != nil {
		t.Fatal(err)
	}
	if err := store.Insert(ctx, second); err != nil {
		t.Fatalf("capacity did not recover after completion: %v", err)
	}
}

func TestInboxClaimRetriesAreBoundedAndReported(t *testing.T) {
	ctx := context.Background()
	store, err := OpenWithOptions(ctx, filepath.Join(t.TempDir(), "im.db"), Options{MaxClaimAttempts: 2})
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	message := channel.InboundMessage{BindingID: "bot", EventID: "event-1", MessageID: "message-1"}
	if err := store.Insert(ctx, message); err != nil {
		t.Fatal(err)
	}
	for attempt := 0; attempt < 2; attempt++ {
		item, found, err := store.Claim(ctx, time.Minute)
		if err != nil || !found {
			t.Fatalf("attempt %d: found=%t err=%v", attempt+1, found, err)
		}
		if err := store.ReleaseClaim(ctx, item); err != nil {
			t.Fatal(err)
		}
	}
	if _, found, err := store.Claim(ctx, time.Minute); err != nil || found {
		t.Fatalf("exhausted event was claimed again: found=%t err=%v", found, err)
	}
	stats, err := store.BindingStats(ctx, "bot")
	if err != nil || stats.FailedEvents != 1 || stats.Queued != 0 {
		t.Fatalf("failed event not reported: %+v %v", stats, err)
	}
	if err := store.Insert(ctx, message); err != nil {
		t.Fatalf("same event redelivery should remain idempotent: %v", err)
	}
}

func TestBusyClaimDeferralDoesNotConsumeRetryBudget(t *testing.T) {
	ctx := context.Background()
	store, err := OpenWithOptions(ctx, filepath.Join(t.TempDir(), "im.db"), Options{MaxClaimAttempts: 1})
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	message := channel.InboundMessage{BindingID: "bot", EventID: "event-1", MessageID: "message-1"}
	if err := store.Insert(ctx, message); err != nil {
		t.Fatal(err)
	}
	item, found, err := store.Claim(ctx, time.Minute)
	if err != nil || !found {
		t.Fatalf("initial claim: found=%t err=%v", found, err)
	}
	if err := store.DeferClaim(ctx, item, 30*time.Millisecond); err != nil {
		t.Fatal(err)
	}
	if _, found, err := store.Claim(ctx, time.Minute); err != nil || found {
		t.Fatalf("busy event was reclaimed before delay: found=%t err=%v", found, err)
	}
	time.Sleep(40 * time.Millisecond)
	again, found, err := store.Claim(ctx, time.Minute)
	if err != nil || !found || again.Message.EventID != message.EventID || again.Token == item.Token {
		t.Fatalf("deferred event was not claimable with a fresh token: %+v found=%t err=%v", again, found, err)
	}
	if err := store.DeferClaim(ctx, item, time.Second); err == nil {
		t.Fatal("stale claim token deferred a new owner's event")
	}
	if err := store.ReleaseClaim(ctx, again); err != nil {
		t.Fatal(err)
	}
	if stats, err := store.BindingStats(ctx, "bot"); err != nil || stats.FailedEvents != 1 {
		t.Fatalf("a real retry failure did not consume the remaining budget: %+v err=%v", stats, err)
	}
}

func TestExpiredFinalClaimBecomesFailedWithoutWorkerRelease(t *testing.T) {
	ctx := context.Background()
	store, err := OpenWithOptions(ctx, filepath.Join(t.TempDir(), "im.db"), Options{MaxClaimAttempts: 1})
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	message := channel.InboundMessage{BindingID: "bot", EventID: "event-1", MessageID: "message-1"}
	if err := store.Insert(ctx, message); err != nil {
		t.Fatal(err)
	}
	item, found, err := store.Claim(ctx, time.Minute)
	if err != nil || !found {
		t.Fatalf("claim: found=%t err=%v", found, err)
	}
	if _, err := store.db.ExecContext(ctx, `UPDATE im_inbox SET lease_until = 0 WHERE binding_id = ? AND event_id = ?`, message.BindingID, message.EventID); err != nil {
		t.Fatal(err)
	}
	if _, found, err := store.Claim(ctx, time.Minute); err != nil || found {
		t.Fatalf("expired final claim was replayed: found=%t err=%v", found, err)
	}
	if err := store.BeginSubmission(ctx, item); err == nil {
		t.Fatal("expired worker submitted a failed event")
	}
	stats, err := store.BindingStats(ctx, "bot")
	if err != nil || stats.FailedEvents != 1 || stats.Claimed != 0 {
		t.Fatalf("expired claim was not reported as failed: %+v %v", stats, err)
	}
}

func TestConversationThreadRefScopeAndConflict(t *testing.T) {
	ctx := context.Background()
	store, err := Open(ctx, filepath.Join(t.TempDir(), "im.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	target := channel.AgentTarget{RunnerID: "runner", ProfileID: "agent", ProfileRevision: 1}
	for _, binding := range []string{"bot-a", "bot-b"} {
		key := channel.SessionKey{TenantID: "tenant", BindingID: binding, ChatID: "chat", SubjectID: "om-root"}
		if _, err := store.Ensure(ctx, key, target, channel.ReplyAddress{}, "omt-1"); err != nil {
			t.Fatal(err)
		}
	}
	conflict := channel.SessionKey{TenantID: "tenant", BindingID: "bot-a", ChatID: "chat", SubjectID: "om-other"}
	if _, err := store.Ensure(ctx, conflict, target, channel.ReplyAddress{}, "omt-1"); err == nil {
		t.Fatal("conflicting canonical root was accepted")
	}
	if key, found, err := store.FindByThreadRef(ctx, "bot-a", "other-chat", "omt-1"); err != nil || found || key.SubjectID != "" {
		t.Fatalf("thread ref leaked across chats: %+v %t %v", key, found, err)
	}
	if _, err := store.Ensure(ctx, channel.SessionKey{TenantID: "tenant", BindingID: "bot-a", ChatID: "chat", SubjectID: "om-root"}, target, channel.ReplyAddress{}, "omt-1"); err != nil {
		t.Fatalf("same thread ref was not idempotent: %v", err)
	}
	if _, err := store.Ensure(ctx, channel.SessionKey{TenantID: "tenant", BindingID: "bot-a", ChatID: "chat", SubjectID: "om-root"}, target, channel.ReplyAddress{}, "omt-other"); err == nil {
		t.Fatal("one conversation acquired a conflicting second thread ref")
	}
}

func TestStoreHasOnlyFourIMTables(t *testing.T) {
	store, err := Open(context.Background(), filepath.Join(t.TempDir(), "im.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	rows, err := store.db.Query(`SELECT name FROM sqlite_master WHERE type = 'table' AND name LIKE 'im_%' ORDER BY name`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var names []string
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			t.Fatal(err)
		}
		names = append(names, name)
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	want := []string{"im_bindings", "im_conversations", "im_deliveries", "im_inbox"}
	if !slices.Equal(names, want) {
		t.Fatalf("IM tables: got %v, want %v", names, want)
	}
}

func TestDeliveryReservationAndCASPersistAcrossReopen(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "im.db")
	store, err := Open(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	address := channel.ReplyAddress{Provider: "feishu", Version: 1, Data: json.RawMessage(`{"chat_id":"oc-1"}`)}
	initial := channel.Delivery{ID: "turn-1", Session: channel.SessionKey{TenantID: "tenant", BindingID: "bot-a", ChatID: "oc-1", SubjectID: "om-root"}, Address: address, Mode: "streaming_card", ProviderStateVersion: 1}
	state, created, err := store.Reserve(ctx, initial)
	if err != nil || !created {
		t.Fatalf("reserve: created=%t error=%v", created, err)
	}
	manager := channel.DeliveryManager{Store: store}
	stale := state
	state, err = manager.Intent(ctx, state, "create", nil)
	if err != nil {
		t.Fatal(err)
	}
	state, err = manager.Confirm(ctx, state, false, json.RawMessage(`{"card_id":"card-1"}`))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.Commit(ctx, stale); err == nil {
		t.Fatal("stale delivery state replaced a newer version")
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	store, err = Open(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	loaded, created, err := store.Reserve(ctx, initial)
	if err != nil || created || loaded.Session != state.Session || loaded.ID != state.ID || string(loaded.ProviderState) != string(state.ProviderState) || loaded.Phase != state.Phase || loaded.Revision != state.Revision {
		t.Fatalf("reopened delivery mismatch: state=%+v created=%t err=%v", loaded, created, err)
	}
}

func TestDeliveryStoreEnforcesLifecycleAndImmutableIdentity(t *testing.T) {
	ctx := context.Background()
	store, err := Open(ctx, filepath.Join(t.TempDir(), "im.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	initial := channel.Delivery{ID: "turn-1", Session: channel.SessionKey{TenantID: "tenant", BindingID: "bot", ChatID: "chat", SubjectID: "user"}, Address: channel.ReplyAddress{Provider: "feishu", Version: 1, Data: json.RawMessage(`{"chat_id":"chat"}`)}, Mode: "final_text", ProviderStateVersion: 1}
	state, _, err := store.Reserve(ctx, initial)
	if err != nil {
		t.Fatal(err)
	}
	invalid := state
	invalid.Phase = "complete"
	if _, err := store.Commit(ctx, invalid); err == nil {
		t.Fatal("delivery skipped persisted intent")
	}
	invalid = state
	invalid.Mode = "final_card"
	invalid.Phase, invalid.Operation = "pending", "send"
	if _, err := store.Commit(ctx, invalid); err == nil {
		t.Fatal("delivery mode changed during a turn")
	}
	invalid = state
	invalid.Address.Data = json.RawMessage(`{"chat_id":"other"}`)
	invalid.Phase, invalid.Operation = "pending", "send"
	if _, err := store.Commit(ctx, invalid); err == nil {
		t.Fatal("delivery reply address changed during a turn")
	}
	manager := channel.DeliveryManager{Store: store}
	state, err = manager.Intent(ctx, state, "send", nil)
	if err != nil {
		t.Fatal(err)
	}
	invalid = state
	invalid.Phase, invalid.Operation = "complete", "send"
	if _, err := store.Commit(ctx, invalid); err == nil {
		t.Fatal("delivery completed without clearing its pending operation")
	}
	state, err = manager.Unknown(ctx, state)
	if err != nil {
		t.Fatal(err)
	}
	if state.Phase != "unknown" || state.Operation != "send" {
		t.Fatalf("unknown outcome lost its operation: %+v", state)
	}
	invalid = state
	invalid.Phase, invalid.Operation = "pending", "send"
	if _, err := store.Commit(ctx, invalid); err == nil {
		t.Fatal("unknown delivery was retried without reconciliation")
	}
}

func TestDeliveryAgentCompletionEvidenceIsMonotonic(t *testing.T) {
	ctx := context.Background()
	store, err := Open(ctx, filepath.Join(t.TempDir(), "im.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	manager := channel.DeliveryManager{Store: store}
	state, _, err := manager.Reserve(ctx, channel.Delivery{ID: "turn", Session: channel.SessionKey{TenantID: "tenant", BindingID: "bot", ChatID: "chat", SubjectID: "user"}, Mode: "streaming_card", ProviderStateVersion: 2})
	if err != nil {
		t.Fatal(err)
	}
	state, err = manager.Intent(ctx, state, "create", nil)
	if err != nil {
		t.Fatal(err)
	}
	state, err = manager.Confirm(ctx, state, false, nil)
	if err != nil {
		t.Fatal(err)
	}
	state.AgentTurnCompleted = true
	state, err = manager.Intent(ctx, state, "close", nil)
	if err != nil {
		t.Fatal(err)
	}
	withoutEvidence := state
	withoutEvidence.AgentTurnCompleted = false
	if _, err := manager.Confirm(ctx, withoutEvidence, true, nil); err == nil {
		t.Fatal("delivery removed durable Agent completion evidence")
	}
	state, err = manager.Confirm(ctx, state, true, nil)
	if err != nil || !state.AgentTurnCompleted {
		t.Fatalf("Agent completion evidence was lost: %+v %v", state, err)
	}
}

func TestInboxClaimFencesWorkersAndNeverReplaysSubmittedPrompt(t *testing.T) {
	ctx := context.Background()
	store, err := Open(ctx, filepath.Join(t.TempDir(), "im.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	first := channel.InboundMessage{BindingID: "bot", EventID: "event-1", MessageID: "om-1"}
	if err := store.Insert(ctx, first); err != nil {
		t.Fatal(err)
	}
	claimed, found, err := store.Claim(ctx, time.Minute)
	if err != nil || !found || claimed.Message.EventID != first.EventID {
		t.Fatalf("first claim: item=%+v found=%t err=%v", claimed, found, err)
	}
	if _, found, err := store.Claim(ctx, time.Minute); err != nil || found {
		t.Fatalf("leased item was claimed again: found=%t err=%v", found, err)
	}
	if err := store.BeginSubmission(ctx, claimed); err != nil {
		t.Fatal(err)
	}
	if err := store.ReleaseClaim(ctx, claimed); err == nil {
		t.Fatal("submitted prompt returned to queued state")
	}
	if _, found, err := store.Claim(ctx, time.Minute); err != nil || found {
		t.Fatalf("submitted prompt was replayable: found=%t err=%v", found, err)
	}
	if err := store.MarkUnknown(ctx, claimed, "transport ended after prompt submission"); err != nil {
		t.Fatal(err)
	}
	if err := store.Complete(ctx, claimed); err == nil {
		t.Fatal("unknown result was silently completed")
	}
	if _, found, err := store.Claim(ctx, time.Minute); err != nil || found {
		t.Fatalf("unknown prompt was replayable: found=%t err=%v", found, err)
	}
	second := channel.InboundMessage{BindingID: "bot", EventID: "event-2", MessageID: "om-2"}
	if err := store.Insert(ctx, second); err != nil {
		t.Fatal(err)
	}
	item, found, err := store.Claim(ctx, time.Minute)
	if err != nil || !found || item.Message.EventID != second.EventID {
		t.Fatalf("second claim: item=%+v found=%t err=%v", item, found, err)
	}
	if err := store.ReleaseClaim(ctx, item); err != nil {
		t.Fatal(err)
	}
	if _, found, err := store.Claim(ctx, time.Minute); err != nil || !found {
		t.Fatalf("pre-submission release was not retryable: found=%t err=%v", found, err)
	}
}

func TestExpiredClaimRejectsStaleWorker(t *testing.T) {
	ctx := context.Background()
	store, err := Open(ctx, filepath.Join(t.TempDir(), "im.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	if err := store.Insert(ctx, channel.InboundMessage{BindingID: "bot", EventID: "event", MessageID: "om"}); err != nil {
		t.Fatal(err)
	}
	stale, found, err := store.Claim(ctx, time.Minute)
	if err != nil || !found {
		t.Fatalf("initial claim: found=%t err=%v", found, err)
	}
	if _, err := store.db.ExecContext(ctx, `UPDATE im_inbox SET lease_until = 1 WHERE binding_id = 'bot' AND event_id = 'event'`); err != nil {
		t.Fatal(err)
	}
	fresh, found, err := store.Claim(ctx, time.Minute)
	if err != nil || !found || fresh.Token == stale.Token {
		t.Fatalf("expired claim was not fenced: found=%t stale=%q fresh=%q err=%v", found, stale.Token, fresh.Token, err)
	}
	if err := store.BeginSubmission(ctx, stale); err == nil {
		t.Fatal("stale worker submitted a prompt after lease takeover")
	}
	if err := store.BeginSubmission(ctx, fresh); err != nil {
		t.Fatal(err)
	}
}

func TestGroupAdmissionClaimsAreOrderedPerBotAndChat(t *testing.T) {
	ctx := context.Background()
	store, err := Open(ctx, filepath.Join(t.TempDir(), "im.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	for _, message := range []channel.InboundMessage{
		{BindingID: "bot-a", EventID: "first", MessageID: "om-first", ChatKind: channel.ChatGroup, ChatID: "chat-a"},
		{BindingID: "bot-a", EventID: "second", MessageID: "om-second", ChatKind: channel.ChatGroup, ChatID: "chat-a"},
		{BindingID: "bot-a", EventID: "other-chat", MessageID: "om-other", ChatKind: channel.ChatGroup, ChatID: "chat-b"},
	} {
		if err := store.Insert(ctx, message); err != nil {
			t.Fatal(err)
		}
	}
	first, found, err := store.Claim(ctx, time.Minute)
	if err != nil || !found || first.Message.EventID != "first" {
		t.Fatalf("first group claim: item=%+v found=%t err=%v", first, found, err)
	}
	other, found, err := store.Claim(ctx, time.Minute)
	if err != nil || !found || other.Message.EventID != "other-chat" {
		t.Fatalf("another chat should progress while first is routing: item=%+v found=%t err=%v", other, found, err)
	}
	if _, found, err := store.Claim(ctx, time.Minute); err != nil || found {
		t.Fatalf("same group continuation overtook admission: found=%t err=%v", found, err)
	}
	if err := store.BeginSubmission(ctx, first); err != nil {
		t.Fatal(err)
	}
	second, found, err := store.Claim(ctx, time.Minute)
	if err != nil || !found || second.Message.EventID != "second" {
		t.Fatalf("group continuation stayed blocked after admission: item=%+v found=%t err=%v", second, found, err)
	}
}
