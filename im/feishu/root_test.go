package feishu

import (
	"context"
	"encoding/json"
	"errors"
	"path/filepath"
	"testing"

	"github.com/aiomni/dune/im/channel"
	"github.com/aiomni/dune/im/sqlite"
)

type lookupFunc func(context.Context, string) (MessageMetadata, error)

func (f lookupFunc) LookupMessage(ctx context.Context, id string) (MessageMetadata, error) {
	return f(ctx, id)
}

func groupMessage(id, root, thread string) channel.InboundMessage {
	data, _ := json.Marshal(replyAddress{ChatKind: channel.ChatGroup, ChatID: "oc_group", ReplyMessageID: id, RootMessageID: root, ThreadID: thread})
	return channel.InboundMessage{BindingID: "bot", ChatID: "oc_group", ChatKind: channel.ChatGroup, MessageID: id,
		Address: channel.ReplyAddress{Provider: Kind, Version: 1, Data: data}}
}

func testConversationStore(t *testing.T) *sqlite.Store {
	t.Helper()
	store, err := sqlite.Open(context.Background(), filepath.Join(t.TempDir(), "im.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	return store
}

func TestFeishuThreadAnchorAndLocalRef(t *testing.T) {
	ctx := context.Background()
	store := testConversationStore(t)
	resolver := RootResolver{Conversations: store, Messages: lookupFunc(func(context.Context, string) (MessageMetadata, error) {
		return MessageMetadata{}, errors.New("lookup must not be needed")
	})}
	binding := channel.BotBinding{ID: "bot", TenantID: "tenant"}
	root, ref, err := channel.Route(ctx, binding, groupMessage("om_root", "", ""), resolver)
	if err != nil || ref != "" || root.SubjectID != "om_root" {
		t.Fatalf("top-level: %+v %q %v", root, ref, err)
	}
	reply, ref, err := channel.Route(ctx, binding, groupMessage("om_child", "om_root", "omt_thread"), resolver)
	if err != nil || root != reply || ref != "omt_thread" {
		t.Fatalf("thread reply: %+v %q %v", reply, ref, err)
	}
	target := channel.AgentTarget{RunnerID: "runner", AgentConfigID: "agent"}
	if _, err := store.Ensure(ctx, root, target, channel.ReplyAddress{}, ""); err != nil {
		t.Fatal(err)
	}
	if _, err := store.Ensure(ctx, reply, target, channel.ReplyAddress{}, ref); err != nil {
		t.Fatal(err)
	}
	third, thirdRef, err := channel.Route(ctx, binding, groupMessage("om_third", "", "omt_thread"), resolver)
	if err != nil || third != root || thirdRef != ref {
		t.Fatalf("local thread ref: %+v %q %v", third, thirdRef, err)
	}
	other, _, err := channel.Route(ctx, binding, groupMessage("om_other", "", ""), resolver)
	if err != nil || other == root {
		t.Fatalf("separate topic: %+v %v", other, err)
	}
}

func TestFeishuUnknownThreadRequiresVerifiedRoot(t *testing.T) {
	store := testConversationStore(t)
	lookups := 0
	resolver := RootResolver{Conversations: store, Messages: lookupFunc(func(_ context.Context, id string) (MessageMetadata, error) {
		lookups++
		return MessageMetadata{MessageID: id, ChatID: "oc_group", RootID: "om_root", ThreadID: "omt_thread"}, nil
	})}
	got, err := resolver.ResolveGroup(context.Background(), groupMessage("om_child", "", "omt_thread"))
	if err != nil || got.SubjectID != "om_root" || got.ProviderThreadRef != "omt_thread" || lookups != 1 {
		t.Fatalf("lookup: %+v %d %v", got, lookups, err)
	}
	// Resolution itself does not bind. Only conversation Ensure does.
	_, err = resolver.ResolveGroup(context.Background(), groupMessage("om_child", "", "omt_thread"))
	if err != nil || lookups != 2 {
		t.Fatalf("unexpected implicit binding: %d %v", lookups, err)
	}
}

func TestFeishuThreadRootMessageCanAnchorItselfAfterLookup(t *testing.T) {
	store := testConversationStore(t)
	resolver := RootResolver{Conversations: store, Messages: lookupFunc(func(_ context.Context, id string) (MessageMetadata, error) {
		return MessageMetadata{MessageID: id, ChatID: "oc_group", ThreadID: "omt_thread"}, nil
	})}
	got, err := resolver.ResolveGroup(context.Background(), groupMessage("om_root", "", "omt_thread"))
	if err != nil || got.SubjectID != "om_root" || got.ProviderThreadRef != "omt_thread" {
		t.Fatalf("verified root message was not accepted: %+v %v", got, err)
	}
	resolver.Messages = lookupFunc(func(_ context.Context, id string) (MessageMetadata, error) {
		return MessageMetadata{MessageID: id, ChatID: "oc_group", ParentID: "om_parent", ThreadID: "omt_thread"}, nil
	})
	if _, err := resolver.ResolveGroup(context.Background(), groupMessage("om_child", "", "omt_thread")); err == nil {
		t.Fatal("reply without a verified root was guessed as its own root")
	}
}

func TestFeishuPlainReplyAndUnverifiedThread(t *testing.T) {
	store := testConversationStore(t)
	resolver := RootResolver{Conversations: store, Messages: lookupFunc(func(_ context.Context, id string) (MessageMetadata, error) {
		return MessageMetadata{MessageID: id, ChatID: "oc_group", RootID: "om_other", ParentID: "om_other"}, nil
	})}
	got, err := resolver.ResolveGroup(context.Background(), groupMessage("om_reply", "om_other", ""))
	if err != nil || got.SubjectID != "om_reply" {
		t.Fatalf("plain quote joined topic: %+v %v", got, err)
	}
	resolver.Messages = lookupFunc(func(_ context.Context, id string) (MessageMetadata, error) {
		return MessageMetadata{MessageID: id, ChatID: "oc_elsewhere", RootID: "om_root", ThreadID: "omt_thread"}, nil
	})
	if _, err := resolver.ResolveGroup(context.Background(), groupMessage("om_child", "", "omt_thread")); err == nil {
		t.Fatal("cross-chat lookup accepted")
	}
	resolver.Messages = nil
	if _, err := resolver.ResolveGroup(context.Background(), groupMessage("om_child", "", "omt_thread")); err == nil {
		t.Fatal("unresolved thread accepted")
	}
}
