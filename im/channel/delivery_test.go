package channel_test

import (
	"context"
	"encoding/json"
	"path/filepath"
	"testing"

	"github.com/aiomni/dune/im/channel"
	"github.com/aiomni/dune/im/sqlite"
)

func TestDeliveryManagerRequiresDurableIntentAndFencesUnknown(t *testing.T) {
	ctx := context.Background()
	store, err := sqlite.Open(ctx, filepath.Join(t.TempDir(), "im.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	manager := channel.DeliveryManager{Store: store}
	initial := channel.Delivery{ID: "turn-1", Session: channel.SessionKey{TenantID: "tenant", BindingID: "bot", ChatID: "chat", SubjectID: "user"}, Mode: "final_text", ProviderStateVersion: 1}
	state, created, err := manager.Reserve(ctx, initial)
	if err != nil || !created {
		t.Fatalf("reserve: %t %v", created, err)
	}
	if _, err := manager.Confirm(ctx, state, true, nil); err == nil {
		t.Fatal("completion without durable intent accepted")
	}
	state, err = manager.Intent(ctx, state, "send", json.RawMessage(`{"attempt":1}`))
	if err != nil || state.Phase != "pending" {
		t.Fatalf("intent: %+v %v", state, err)
	}
	if _, err := manager.Intent(ctx, state, "send", nil); err == nil {
		t.Fatal("pending operation was repeated")
	}
	state, err = manager.Unknown(ctx, state)
	if err != nil || state.Phase != "unknown" || state.Operation != "send" {
		t.Fatalf("unknown: %+v %v", state, err)
	}
	if _, err := manager.Intent(ctx, state, "send", nil); err == nil {
		t.Fatal("unknown operation was retried")
	}
	loaded, created, err := manager.Reserve(ctx, initial)
	if err != nil || created || loaded.Phase != "unknown" {
		t.Fatalf("unknown not durable: %+v %t %v", loaded, created, err)
	}
}
