package sqlite

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"

	"github.com/aiomni/dune/im/channel"
)

func TestReconcileConfirmedDeliveryCompletesUnknownTurnWithoutReplay(t *testing.T) {
	ctx := context.Background()
	store, err := Open(ctx, filepath.Join(t.TempDir(), "im.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	message := channel.InboundMessage{BindingID: "bot", EventID: "event-1", MessageID: "message-1", ChatID: "chat", ChatKind: channel.ChatDirect, SenderID: "user", Text: "question"}
	if err := store.Insert(ctx, message); err != nil {
		t.Fatal(err)
	}
	key := channel.SessionKey{TenantID: "tenant", BindingID: "bot", ChatID: "chat", SubjectID: "user"}
	if _, err := store.Ensure(ctx, key, channel.AgentTarget{RunnerID: "runner", AgentConfigID: "agent"}, channel.ReplyAddress{}, ""); err != nil {
		t.Fatal(err)
	}
	lease, acquired, err := store.Acquire(ctx, key, time.Minute)
	if err != nil || !acquired {
		t.Fatalf("acquire: %t %v", acquired, err)
	}
	if err := store.BeginTurn(ctx, lease, message.EventID); err != nil {
		t.Fatal(err)
	}
	item, found, err := store.Claim(ctx, time.Minute)
	if err != nil || !found {
		t.Fatalf("claim: %t %v", found, err)
	}
	if err := store.BeginSubmission(ctx, item); err != nil {
		t.Fatal(err)
	}
	manager := channel.DeliveryManager{Store: store}
	delivery, _, err := manager.Reserve(ctx, channel.Delivery{ID: channel.TurnDeliveryID("bot", message.EventID), Session: key, Mode: "final_text", ProviderStateVersion: 1, AgentTurnCompleted: true})
	if err != nil {
		t.Fatal(err)
	}
	delivery, err = manager.Intent(ctx, delivery, "send", nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.MarkUnknown(ctx, item, "inbox completion failed"); err != nil {
		t.Fatal(err)
	}
	if err := store.UnknownTurn(ctx, lease, "inbox completion failed"); err != nil {
		t.Fatal(err)
	}
	if err := store.ReconcileConfirmedDelivery(ctx, key, message.EventID); err == nil {
		t.Fatal("pending outbound operation was treated as confirmed")
	}
	if _, err := manager.Confirm(ctx, delivery, true, []byte(`{"message_id":"om_answer"}`)); err != nil {
		t.Fatal(err)
	}
	wrong := key
	wrong.SubjectID = "someone-else"
	if err := store.ReconcileConfirmedDelivery(ctx, wrong, message.EventID); err == nil {
		t.Fatal("different SessionKey cleared an unknown turn")
	}
	if err := store.ReconcileConfirmedDelivery(ctx, key, "different-event"); err == nil {
		t.Fatal("different event cleared an unknown turn")
	}
	if err := store.ReconcileConfirmedDelivery(ctx, key, message.EventID); err != nil {
		t.Fatal(err)
	}
	if _, state, found, err := store.Get(ctx, key); err != nil || !found || state != channel.ConversationReady {
		t.Fatalf("reconciled conversation: state=%s found=%t err=%v", state, found, err)
	}
	if stats, err := store.BindingStats(ctx, "bot"); err != nil || stats.UnknownEvents != 0 || stats.UnknownConversations != 0 || stats.UnknownDeliveries != 0 {
		t.Fatalf("unknown state remained after confirmed delivery: %+v %v", stats, err)
	}
	if _, found, err := store.Claim(ctx, time.Minute); err != nil || found {
		t.Fatalf("recovery replayed an already delivered event: found=%t err=%v", found, err)
	}
	if err := store.ReconcileConfirmedDelivery(ctx, key, message.EventID); err == nil {
		t.Fatal("completed turn was reconciled twice")
	}
	if nextLease, acquired, err := store.Acquire(ctx, key, time.Minute); err != nil || !acquired {
		t.Fatalf("reconciled session remained locked: %+v acquired=%t err=%v", nextLease, acquired, err)
	}
}

func TestReconcileRejectedDeliveryRecordsKnownFailureWithoutReplay(t *testing.T) {
	ctx := context.Background()
	store, err := Open(ctx, filepath.Join(t.TempDir(), "im.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	message := channel.InboundMessage{BindingID: "bot", EventID: "event-rejected", MessageID: "message-rejected",
		ChatID: "chat", ChatKind: channel.ChatDirect, SenderID: "user", Text: "question"}
	if err := store.Insert(ctx, message); err != nil {
		t.Fatal(err)
	}
	key := channel.SessionKey{TenantID: "tenant", BindingID: "bot", ChatID: "chat", SubjectID: "user"}
	if _, err := store.Ensure(ctx, key, channel.AgentTarget{RunnerID: "runner", AgentConfigID: "agent"}, channel.ReplyAddress{}, ""); err != nil {
		t.Fatal(err)
	}
	lease, acquired, err := store.Acquire(ctx, key, time.Minute)
	if err != nil || !acquired {
		t.Fatalf("acquire: %t %v", acquired, err)
	}
	if err := store.BeginTurn(ctx, lease, message.EventID); err != nil {
		t.Fatal(err)
	}
	item, found, err := store.Claim(ctx, time.Minute)
	if err != nil || !found {
		t.Fatalf("claim: %t %v", found, err)
	}
	if err := store.BeginSubmission(ctx, item); err != nil {
		t.Fatal(err)
	}
	manager := channel.DeliveryManager{Store: store}
	delivery, _, err := manager.Reserve(ctx, channel.Delivery{ID: channel.TurnDeliveryID("bot", message.EventID), Session: key,
		Mode: "final_text", ProviderStateVersion: 1, AgentTurnCompleted: true})
	if err != nil {
		t.Fatal(err)
	}
	delivery, err = manager.Intent(ctx, delivery, "send", nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.ReconcileRejectedDelivery(ctx, key, message.EventID); err == nil {
		t.Fatal("unresolved outbound send was treated as a known rejection")
	}
	if _, err := manager.Reject(ctx, delivery); err != nil {
		t.Fatal(err)
	}
	if err := store.MarkUnknown(ctx, item, "bookkeeping interrupted"); err != nil {
		t.Fatal(err)
	}
	if err := store.UnknownTurn(ctx, lease, "bookkeeping interrupted"); err != nil {
		t.Fatal(err)
	}
	if err := store.ReconcileConfirmedDelivery(ctx, key, message.EventID); err == nil {
		t.Fatal("rejected reply was mistaken for delivered reply")
	}
	if err := store.ReconcileRejectedDelivery(ctx, key, message.EventID); err != nil {
		t.Fatal(err)
	}
	if _, state, found, err := store.Get(ctx, key); err != nil || !found || state != channel.ConversationReady {
		t.Fatalf("rejected turn stayed blocked: state=%s found=%t err=%v", state, found, err)
	}
	if stats, err := store.BindingStats(ctx, "bot"); err != nil || stats.FailedEvents != 1 || stats.FailedDeliveries != 1 || stats.UnknownEvents != 0 || stats.UnknownConversations != 0 {
		t.Fatalf("rejected turn status: %+v err=%v", stats, err)
	}
	if _, found, err := store.Claim(ctx, time.Minute); err != nil || found {
		t.Fatalf("rejected turn was replayed: found=%t err=%v", found, err)
	}
}

func TestReconcileCompletedInboxRequiresExpiredWorkerLease(t *testing.T) {
	ctx := context.Background()
	store, err := Open(ctx, filepath.Join(t.TempDir(), "im.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	message := channel.InboundMessage{BindingID: "bot", EventID: "event-2", MessageID: "message-2", ChatID: "chat", ChatKind: channel.ChatDirect, SenderID: "user", Text: "question"}
	if err := store.Insert(ctx, message); err != nil {
		t.Fatal(err)
	}
	key := channel.SessionKey{TenantID: "tenant", BindingID: "bot", ChatID: "chat", SubjectID: "user"}
	if _, err := store.Ensure(ctx, key, channel.AgentTarget{RunnerID: "runner", AgentConfigID: "agent"}, channel.ReplyAddress{}, ""); err != nil {
		t.Fatal(err)
	}
	lease, acquired, err := store.Acquire(ctx, key, time.Minute)
	if err != nil || !acquired {
		t.Fatalf("acquire: %t %v", acquired, err)
	}
	if err := store.BeginTurn(ctx, lease, message.EventID); err != nil {
		t.Fatal(err)
	}
	item, found, err := store.Claim(ctx, time.Minute)
	if err != nil || !found {
		t.Fatalf("claim: %t %v", found, err)
	}
	if err := store.BeginSubmission(ctx, item); err != nil {
		t.Fatal(err)
	}
	manager := channel.DeliveryManager{Store: store}
	delivery, _, err := manager.Reserve(ctx, channel.Delivery{ID: channel.TurnDeliveryID("bot", message.EventID), Session: key, Mode: "final_text", ProviderStateVersion: 1, AgentTurnCompleted: true})
	if err != nil {
		t.Fatal(err)
	}
	delivery, err = manager.Intent(ctx, delivery, "send", nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := manager.Confirm(ctx, delivery, true, []byte(`{"message_id":"om_answer"}`)); err != nil {
		t.Fatal(err)
	}
	if err := store.Complete(ctx, item); err != nil {
		t.Fatal(err)
	}
	if err := store.ReconcileConfirmedDelivery(ctx, key, message.EventID); err == nil {
		t.Fatal("live worker's completed event was reconciled before its lease expired")
	}
	if _, err := store.db.ExecContext(ctx, `UPDATE im_conversations SET lease_until = 0 WHERE key_hash = ?`, key.String()); err != nil {
		t.Fatal(err)
	}
	if err := store.ReconcileConfirmedDelivery(ctx, key, message.EventID); err != nil {
		t.Fatal(err)
	}
	if err := store.FinishTurn(ctx, lease); err == nil {
		t.Fatal("stale worker finished a reconciled turn")
	}
	if _, state, found, err := store.Get(ctx, key); err != nil || !found || state != channel.ConversationReady {
		t.Fatalf("reconciled completed inbox: state=%s found=%t err=%v", state, found, err)
	}
	if _, found, err := store.Claim(ctx, time.Minute); err != nil || found {
		t.Fatalf("completed event was replayed: found=%t err=%v", found, err)
	}
}

func TestReconcileSubmittingInboxAfterReopen(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "im.db")
	store, err := Open(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	message := channel.InboundMessage{BindingID: "bot", EventID: "event-crash", MessageID: "message-crash", ChatID: "chat", ChatKind: channel.ChatDirect, SenderID: "user", Text: "question"}
	if err := store.Insert(ctx, message); err != nil {
		t.Fatal(err)
	}
	key := channel.SessionKey{TenantID: "tenant", BindingID: "bot", ChatID: "chat", SubjectID: "user"}
	if _, err := store.Ensure(ctx, key, channel.AgentTarget{RunnerID: "runner", AgentConfigID: "agent"}, channel.ReplyAddress{}, ""); err != nil {
		t.Fatal(err)
	}
	lease, acquired, err := store.Acquire(ctx, key, time.Minute)
	if err != nil || !acquired {
		t.Fatalf("acquire: %t %v", acquired, err)
	}
	if err := store.BeginTurn(ctx, lease, message.EventID); err != nil {
		t.Fatal(err)
	}
	item, found, err := store.Claim(ctx, time.Minute)
	if err != nil || !found {
		t.Fatalf("claim: %t %v", found, err)
	}
	if err := store.BeginSubmission(ctx, item); err != nil {
		t.Fatal(err)
	}
	manager := channel.DeliveryManager{Store: store}
	delivery, created, err := manager.Reserve(ctx, channel.Delivery{ID: channel.TurnDeliveryID("bot", message.EventID), Session: key, Mode: "final_text", ProviderStateVersion: 1, AgentTurnCompleted: true})
	if err != nil || !created {
		t.Fatalf("reserve: created=%t err=%v", created, err)
	}
	if err := store.ReconcileConfirmedDelivery(ctx, key, message.EventID); err == nil {
		t.Fatal("reserved delivery was treated as confirmed")
	}
	delivery, err = manager.Intent(ctx, delivery, "send", nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := manager.Confirm(ctx, delivery, true, []byte(`{"message_id":"om_answer"}`)); err != nil {
		t.Fatal(err)
	}
	if err := store.ReconcileConfirmedDelivery(ctx, key, message.EventID); err == nil {
		t.Fatal("live worker was interrupted while inbox was submitting")
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	store, err = Open(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	if _, found, err := store.Claim(ctx, time.Minute); err != nil || found {
		t.Fatalf("reopened store replayed a submitted prompt: found=%t err=%v", found, err)
	}
	if err := store.ReconcileConfirmedDelivery(ctx, key, message.EventID); err == nil {
		t.Fatal("reopened live lease was treated as expired")
	}
	if _, err := store.db.ExecContext(ctx, `UPDATE im_conversations SET lease_until = 0 WHERE key_hash = ?`, key.String()); err != nil {
		t.Fatal(err)
	}
	if err := store.ReconcileConfirmedDelivery(ctx, key, message.EventID); err != nil {
		t.Fatal(err)
	}
	var inboxState string
	if err := store.db.QueryRowContext(ctx, `SELECT state FROM im_inbox WHERE binding_id = ? AND event_id = ?`, message.BindingID, message.EventID).Scan(&inboxState); err != nil || inboxState != "complete" {
		t.Fatalf("reconciled inbox: state=%s err=%v", inboxState, err)
	}
	if _, state, found, err := store.Get(ctx, key); err != nil || !found || state != channel.ConversationReady {
		t.Fatalf("reconciled conversation: state=%s found=%t err=%v", state, found, err)
	}
	if _, found, err := store.Claim(ctx, time.Minute); err != nil || found {
		t.Fatalf("recovery replayed an already delivered event: found=%t err=%v", found, err)
	}
}

func TestSubmittingWorkNeverReplaysAcrossProcessRestart(t *testing.T) {
	const helperDB = "DUNE_IM_RESTART_TEST_DB"
	ctx := context.Background()
	if path := os.Getenv(helperDB); path != "" {
		store, err := Open(ctx, path)
		if err != nil {
			t.Fatal(err)
		}
		// Deliberately leave the store open at process exit, like a worker
		// disappearing after its durable Agent submission barrier.
		message := channel.InboundMessage{BindingID: "bot", EventID: "event-restart", MessageID: "message-restart", ChatID: "chat", ChatKind: channel.ChatDirect, SenderID: "user", Text: "question"}
		if err := store.Insert(ctx, message); err != nil {
			t.Fatal(err)
		}
		item, found, err := store.Claim(ctx, time.Minute)
		if err != nil || !found {
			t.Fatalf("child claim: found=%t err=%v", found, err)
		}
		if err := store.BeginSubmission(ctx, item); err != nil {
			t.Fatal(err)
		}
		return
	}
	binary, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "im.db")
	child := exec.CommandContext(ctx, binary, "-test.run=^TestSubmittingWorkNeverReplaysAcrossProcessRestart$")
	child.Env = append(os.Environ(), helperDB+"="+path)
	if output, err := child.CombinedOutput(); err != nil {
		t.Fatalf("child process failed: %v\n%s", err, output)
	}
	store, err := Open(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	if _, found, err := store.Claim(ctx, time.Minute); err != nil || found {
		t.Fatalf("new process replayed the already submitted prompt: found=%t err=%v", found, err)
	}
	stats, err := store.BindingStats(ctx, "bot")
	if err != nil || stats.Submitting != 1 {
		t.Fatalf("submitted work was not durably observable: %+v err=%v", stats, err)
	}
}

func TestClosedFailureNoticeDoesNotProveAgentTurnCompleted(t *testing.T) {
	ctx := context.Background()
	store, err := Open(ctx, filepath.Join(t.TempDir(), "im.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	message := channel.InboundMessage{BindingID: "bot", EventID: "event-failed", MessageID: "message-failed", ChatID: "chat", ChatKind: channel.ChatDirect, SenderID: "user", Text: "question"}
	if err := store.Insert(ctx, message); err != nil {
		t.Fatal(err)
	}
	key := channel.SessionKey{TenantID: "tenant", BindingID: "bot", ChatID: "chat", SubjectID: "user"}
	if _, err := store.Ensure(ctx, key, channel.AgentTarget{RunnerID: "runner", AgentConfigID: "agent"}, channel.ReplyAddress{}, ""); err != nil {
		t.Fatal(err)
	}
	lease, acquired, err := store.Acquire(ctx, key, time.Minute)
	if err != nil || !acquired {
		t.Fatalf("acquire: %t %v", acquired, err)
	}
	if err := store.BeginTurn(ctx, lease, message.EventID); err != nil {
		t.Fatal(err)
	}
	item, found, err := store.Claim(ctx, time.Minute)
	if err != nil || !found {
		t.Fatalf("claim: %t %v", found, err)
	}
	if err := store.BeginSubmission(ctx, item); err != nil {
		t.Fatal(err)
	}
	manager := channel.DeliveryManager{Store: store}
	delivery, _, err := manager.Reserve(ctx, channel.Delivery{ID: channel.TurnDeliveryID("bot", message.EventID), Session: key, Mode: "streaming_card", ProviderStateVersion: 2})
	if err != nil {
		t.Fatal(err)
	}
	delivery, err = manager.Intent(ctx, delivery, "close_failure_notice", nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := manager.Confirm(ctx, delivery, true, []byte(`{"message_id":"om_failure_notice"}`)); err != nil {
		t.Fatal(err)
	}
	if err := store.MarkUnknown(ctx, item, "Agent prompt result unknown"); err != nil {
		t.Fatal(err)
	}
	if err := store.UnknownTurn(ctx, lease, "Agent prompt result unknown"); err != nil {
		t.Fatal(err)
	}
	if err := store.ReconcileConfirmedDelivery(ctx, key, message.EventID); err == nil {
		t.Fatal("closing an error notice incorrectly unlocked an unknown Agent turn")
	}
	if _, state, found, err := store.Get(ctx, key); err != nil || !found || state != channel.ConversationUnknown {
		t.Fatalf("unknown Agent turn was cleared: state=%s found=%t err=%v", state, found, err)
	}
}
