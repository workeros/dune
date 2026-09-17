package sqlite

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"github.com/aiomni/dune/im/channel"
)

func directKey(sender string) channel.SessionKey {
	return channel.SessionKey{TenantID: "tenant-a", BindingID: "bot-a", ChatID: "p2p-a", SubjectID: sender}
}

func TestConversationRetargetsOnlyBeforeAgentStartAndPersistsRuntime(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "im.db")
	store, err := Open(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	key := directKey("user-a")
	target := channel.AgentTarget{RunnerID: "runner-1", ProfileID: "agent-1", ProfileRevision: 1, WorkingDirectory: "/work/a"}
	address := channel.ReplyAddress{Provider: "feishu", Version: 1}
	session, err := store.Ensure(ctx, key, target, address, "")
	if err != nil {
		t.Fatal(err)
	}
	changed := channel.AgentTarget{RunnerID: "runner-2", ProfileID: "agent-2", ProfileRevision: 1}
	again, err := store.Ensure(ctx, key, changed, address, "")
	if err != nil || again.Target != changed || again.Revision <= session.Revision {
		t.Fatalf("empty conversation did not adopt the current target: %+v err=%v", again, err)
	}
	lease, acquired, err := store.Acquire(ctx, key, time.Minute)
	if err != nil || !acquired {
		t.Fatalf("acquire: %t %v", acquired, err)
	}
	again.Runtime = channel.RuntimeHandle{ID: "runtime-a", Incarnation: "inc-a", Generation: 7, Adapter: "acp"}
	again.ACPSessionID = "acp-session-a"
	again, err = store.Save(ctx, lease, again)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.Save(ctx, lease, session); err == nil {
		t.Fatal("stale conversation revision overwrote Runtime")
	}
	if established, err := store.Ensure(ctx, key, target, address, ""); err != nil || established.Target != changed {
		t.Fatalf("established Agent session changed target: %+v err=%v", established, err)
	}
	if err := store.ReleaseLease(ctx, lease); err != nil {
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
	loaded, state, found, err := store.Get(ctx, key)
	if err != nil || !found || state != channel.ConversationReady || loaded.Runtime != again.Runtime || loaded.ACPSessionID != again.ACPSessionID || loaded.Target != changed {
		t.Fatalf("conversation did not survive reopen: %+v %s %t %v", loaded, state, found, err)
	}
	other, err := store.Ensure(ctx, directKey("user-b"), changed, address, "")
	if err != nil || other.Target != changed || other.Key == loaded.Key {
		t.Fatalf("users did not get independent sessions: %+v err=%v", other, err)
	}
}

func TestConversationLeaseNeverTakesOverRunningOrUnknownTurn(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "im.db")
	first, err := Open(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	defer first.Close()
	second, err := Open(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	defer second.Close()
	key := directKey("user-a")
	if _, err := first.Ensure(ctx, key, channel.AgentTarget{RunnerID: "runner", ProfileID: "agent", ProfileRevision: 1}, channel.ReplyAddress{}, ""); err != nil {
		t.Fatal(err)
	}
	lease, acquired, err := first.Acquire(ctx, key, time.Minute)
	if err != nil || !acquired {
		t.Fatalf("first acquire: %t %v", acquired, err)
	}
	if _, acquired, err := second.Acquire(ctx, key, time.Minute); err != nil || acquired {
		t.Fatalf("second worker acquired active lease: %t %v", acquired, err)
	}
	if err := first.BeginTurn(ctx, lease, "event-1"); err != nil {
		t.Fatal(err)
	}
	if _, err := first.db.ExecContext(ctx, `UPDATE im_conversations SET lease_until = 1 WHERE key_hash = ?`, key.String()); err != nil {
		t.Fatal(err)
	}
	if _, acquired, err := second.Acquire(ctx, key, time.Minute); err != nil || acquired {
		t.Fatalf("running turn was taken over after lease expiry: %t %v", acquired, err)
	}
	if err := first.UnknownTurn(ctx, lease, "Agent outcome unknown"); err != nil {
		t.Fatal(err)
	}
	if _, acquired, err := second.Acquire(ctx, key, time.Minute); err != nil || acquired {
		t.Fatalf("unknown turn was taken over: %t %v", acquired, err)
	}
	if _, state, found, err := second.Get(ctx, key); err != nil || !found || state != channel.ConversationUnknown {
		t.Fatalf("unknown turn not durable: state=%s found=%t err=%v", state, found, err)
	}
}

func TestExpiredReadyLeaseCanBeFenced(t *testing.T) {
	ctx := context.Background()
	store, err := Open(ctx, filepath.Join(t.TempDir(), "im.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	key := directKey("user-a")
	if _, err := store.Ensure(ctx, key, channel.AgentTarget{RunnerID: "runner", ProfileID: "agent", ProfileRevision: 1}, channel.ReplyAddress{}, ""); err != nil {
		t.Fatal(err)
	}
	stale, acquired, err := store.Acquire(ctx, key, time.Minute)
	if err != nil || !acquired {
		t.Fatalf("initial acquire: %t %v", acquired, err)
	}
	if _, err := store.db.ExecContext(ctx, `UPDATE im_conversations SET lease_until = 1 WHERE key_hash = ?`, key.String()); err != nil {
		t.Fatal(err)
	}
	fresh, acquired, err := store.Acquire(ctx, key, time.Minute)
	if err != nil || !acquired || fresh.Token == stale.Token {
		t.Fatalf("ready lease was not fenced: %t %v", acquired, err)
	}
	if err := store.BeginTurn(ctx, stale, "old-event"); err == nil {
		t.Fatal("stale worker started a turn")
	}
	if err := store.BeginTurn(ctx, fresh, "new-event"); err != nil {
		t.Fatal(err)
	}
	if err := store.FinishTurn(ctx, fresh); err != nil {
		t.Fatal(err)
	}
	if err := store.ReleaseLease(ctx, fresh); err != nil {
		t.Fatal(err)
	}
}
