package sqlite

import (
	"context"
	"encoding/json"
	"path/filepath"
	"testing"

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
			Target: channel.AgentTarget{RunnerID: "runner-1", AgentConfigID: "agent-1"}, Enabled: true,
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
