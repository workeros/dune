package sqlite

import (
	"context"
	"encoding/json"
	"path/filepath"
	"testing"
	"time"

	"github.com/aiomni/dune/im/channel"
)

func TestTenantCanBindMultipleBotsWithoutMovingOrOverwritingAnotherTenant(t *testing.T) {
	ctx := context.Background()
	store, err := Open(ctx, filepath.Join(t.TempDir(), "im.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	newBinding := func(id, tenant string) channel.BotBinding {
		return channel.BotBinding{
			ID: id, TenantID: tenant, Provider: "feishu", ConfigVersion: 1,
			Config: json.RawMessage(`{"app_id":"cli_example"}`), CredentialRef: "secret:" + id,
			Target: channel.AgentTarget{RunnerID: "runner-1", ProfileID: "agent-1", ProfileRevision: 1}, Enabled: true,
		}
	}
	botA, err := store.Put(ctx, newBinding("bot-a", "tenant-a"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.Put(ctx, newBinding("bot-b", "tenant-a")); err != nil {
		t.Fatal(err)
	}
	if _, err := store.Put(ctx, newBinding("bot-c", "tenant-b")); err != nil {
		t.Fatal(err)
	}
	list, err := store.ListBindings(ctx, "tenant-a")
	if err != nil || len(list) != 2 || list[0].ID != "bot-a" || list[1].ID != "bot-b" {
		t.Fatalf("tenant did not retain two scoped bots: %+v err=%v", list, err)
	}
	if _, found, err := store.GetBinding(ctx, "tenant-b", "bot-a"); err != nil || found {
		t.Fatalf("cross-tenant binding lookup succeeded: found=%t err=%v", found, err)
	}
	stale := botA
	botA.Enabled = false
	botA, err = store.Put(ctx, botA)
	if err != nil || botA.Revision != 2 {
		t.Fatalf("binding update: %+v err=%v", botA, err)
	}
	if _, err := store.Put(ctx, stale); err == nil {
		t.Fatal("stale binding revision overwrote a newer one")
	}
	botA.TenantID = "tenant-b"
	if _, err := store.Put(ctx, botA); err == nil {
		t.Fatal("binding moved to another tenant")
	}
}

func TestBindingGuardedInboxRejectsOldRevisionIncludingDuplicateEvent(t *testing.T) {
	ctx := context.Background()
	store, err := Open(ctx, filepath.Join(t.TempDir(), "im.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	binding, err := store.Put(ctx, channel.BotBinding{ID: "bot", TenantID: "tenant", Provider: "feishu", ConfigVersion: 1,
		Config: json.RawMessage(`{"app_id":"cli_example"}`), CredentialRef: "secret", Target: channel.AgentTarget{RunnerID: "runner", ProfileID: "agent", ProfileRevision: 1}, Enabled: true})
	if err != nil {
		t.Fatal(err)
	}
	event := channel.InboundMessage{BindingID: "bot", BindingRevision: binding.Revision, EventID: "event", MessageID: "message", ChatID: "chat", ChatKind: channel.ChatDirect, SenderID: "user", Text: "hello"}
	if err := store.InsertForBinding(ctx, binding, event); err != nil {
		t.Fatal(err)
	}
	updated, err := store.Put(ctx, binding)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.InsertForBinding(ctx, binding, event); err == nil {
		t.Fatal("old Binding revision ACKed an already stored event")
	}
	currentEvent := event
	currentEvent.BindingRevision = updated.Revision
	if err := store.InsertForBinding(ctx, updated, currentEvent); err != nil {
		t.Fatalf("current Binding could not ACK an identical redelivery: %v", err)
	}
	if err := store.InsertForBinding(ctx, updated, event); err == nil {
		t.Fatal("new Binding accepted an event stamped with an old revision")
	}
	changedPayload := currentEvent
	changedPayload.Text = "different"
	if err := store.InsertForBinding(ctx, updated, changedPayload); err == nil {
		t.Fatal("event ID collision with different content was ACKed")
	}
	newEvent := currentEvent
	newEvent.EventID = "new-event"
	if err := store.InsertForBinding(ctx, updated, newEvent); err != nil {
		t.Fatalf("current Binding could not accept a new event: %v", err)
	}
	newEvent.EventID = "disabled-event"
	updated.Enabled = false
	if _, err := store.Put(ctx, updated); err != nil {
		t.Fatal(err)
	}
	if err := store.InsertForBinding(ctx, updated, newEvent); err == nil {
		t.Fatal("disabled Binding inserted a new event")
	}
	item, found, err := store.Claim(ctx, time.Minute)
	if err != nil || !found || item.Message.EventID != "event" {
		t.Fatalf("original event was lost: %+v found=%t err=%v", item, found, err)
	}
	if _, found, err := store.Claim(ctx, time.Minute); err != nil || found {
		t.Fatalf("later same-chat event bypassed the claimed admission barrier: found=%t err=%v", found, err)
	}
	if err := store.Ignore(ctx, item); err != nil {
		t.Fatal(err)
	}
	item, found, err = store.Claim(ctx, time.Minute)
	if err != nil || !found || item.Message.EventID != "new-event" {
		t.Fatalf("current event was lost: %+v found=%t err=%v", item, found, err)
	}
	if _, found, err := store.Claim(ctx, time.Minute); err != nil || found {
		t.Fatalf("disabled Binding inserted an event: found=%t err=%v", found, err)
	}
}
