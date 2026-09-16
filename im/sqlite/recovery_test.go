package sqlite

import (
	"context"
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
	delivery, _, err := manager.Reserve(ctx, channel.Delivery{ID: channel.TurnDeliveryID("bot", message.EventID), Session: key, Mode: "final_text", ProviderStateVersion: 1})
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
