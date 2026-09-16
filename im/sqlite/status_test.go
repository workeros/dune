package sqlite

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"github.com/aiomni/dune/im/channel"
)

func TestBindingStatsCountDurableUnknownStates(t *testing.T) {
	ctx := context.Background()
	store, err := Open(ctx, filepath.Join(t.TempDir(), "im.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	key := channel.SessionKey{TenantID: "tenant", BindingID: "bot", ChatID: "chat", SubjectID: "user"}
	if _, err := store.Ensure(ctx, key, channel.AgentTarget{RunnerID: "runner", AgentConfigID: "agent"}, channel.ReplyAddress{}, ""); err != nil {
		t.Fatal(err)
	}
	lease, acquired, err := store.Acquire(ctx, key, time.Minute)
	if err != nil || !acquired {
		t.Fatalf("acquire: %t %v", acquired, err)
	}
	if err := store.BeginTurn(ctx, lease, "event"); err != nil {
		t.Fatal(err)
	}
	if err := store.UnknownTurn(ctx, lease, "Agent outcome uncertain"); err != nil {
		t.Fatal(err)
	}
	manager := channel.DeliveryManager{Store: store}
	delivery, _, err := manager.Reserve(ctx, channel.Delivery{ID: "delivery", Session: key, Mode: "final_text", ProviderStateVersion: 1})
	if err != nil {
		t.Fatal(err)
	}
	delivery, err = manager.Intent(ctx, delivery, "send", nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := manager.Unknown(ctx, delivery); err != nil {
		t.Fatal(err)
	}
	if err := store.Insert(ctx, channel.InboundMessage{BindingID: "bot", EventID: "event", MessageID: "message"}); err != nil {
		t.Fatal(err)
	}
	item, found, err := store.Claim(ctx, time.Minute)
	if err != nil || !found {
		t.Fatalf("claim: %t %v", found, err)
	}
	if err := store.BeginSubmission(ctx, item); err != nil {
		t.Fatal(err)
	}
	if err := store.MarkUnknown(ctx, item, "Agent outcome uncertain"); err != nil {
		t.Fatal(err)
	}
	stats, err := store.BindingStats(ctx, "bot")
	if err != nil || stats.UnknownEvents != 1 || stats.UnknownConversations != 1 || stats.UnknownDeliveries != 1 || stats.Queued != 0 {
		t.Fatalf("unknown status counts: %+v %v", stats, err)
	}
	other, err := store.BindingStats(ctx, "another-bot")
	if err != nil || other != (channel.BindingStats{}) {
		t.Fatalf("stats leaked across bindings: %+v %v", other, err)
	}
}
