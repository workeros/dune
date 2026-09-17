package sqlite

import (
	"context"
	"encoding/json"
	"path/filepath"
	"strings"
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
	if err := store.UnknownTurn(ctx, lease, "private-user-question"); err != nil {
		t.Fatal(err)
	}
	manager := channel.DeliveryManager{Store: store}
	delivery, _, err := manager.Reserve(ctx, channel.Delivery{ID: "delivery", Session: key, Mode: "streaming_card", ProviderStateVersion: 2,
		Address: channel.ReplyAddress{Provider: "feishu", Version: 1, Data: json.RawMessage(`{"reply_message_id":"private-reply-address"}`)}})
	if err != nil {
		t.Fatal(err)
	}
	delivery, err = manager.Intent(ctx, delivery, "updating", json.RawMessage(`{"confirmed_text":"private-agent-answer","pending_text":"private-next-update"}`))
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
	if err := store.MarkUnknown(ctx, item, "private-user-question"); err != nil {
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
	issues, err := store.ListIssues(ctx, "bot", 10)
	if err != nil || len(issues.Events) != 1 || len(issues.Conversations) != 1 || len(issues.Deliveries) != 1 {
		t.Fatalf("durable issues: %+v %v", issues, err)
	}
	if issues.Events[0].EventID != "event" || issues.Events[0].State != "unknown" || issues.Events[0].Attempts != 1 ||
		issues.Conversations[0].Key != key || issues.Conversations[0].CurrentEventID != "event" ||
		issues.Deliveries[0].ID != "delivery" || issues.Deliveries[0].Phase != "unknown" || issues.Deliveries[0].Operation != "updating" || issues.Deliveries[0].Session != key {
		t.Fatalf("issue identity or pending operation missing: %+v", issues)
	}
	encodedIssues, err := json.Marshal(issues)
	if err != nil {
		t.Fatal(err)
	}
	for _, sensitive := range []string{"private-agent-answer", "private-next-update", "private-reply-address", "private-user-question", "provider_state", "address"} {
		if strings.Contains(string(encodedIssues), sensitive) {
			t.Fatalf("recovery issue exposed private delivery state %q: %s", sensitive, encodedIssues)
		}
	}
	if issues.Events[0].Failure != "details withheld; inspect server logs" || issues.Conversations[0].Failure != "details withheld; inspect server logs" {
		t.Fatalf("untrusted error text was not redacted: %+v", issues)
	}
	otherIssues, err := store.ListIssues(ctx, "another-bot", 10)
	if err != nil || len(otherIssues.Events)+len(otherIssues.Conversations)+len(otherIssues.Deliveries) != 0 {
		t.Fatalf("issues leaked across bindings: %+v %v", otherIssues, err)
	}
	if _, err := store.ListIssues(ctx, "bot", 101); err == nil {
		t.Fatal("unbounded issue lookup accepted")
	}
}
