package channel

import (
	"context"
	"errors"
	"testing"
)

type rootFunc func(context.Context, InboundMessage) (GroupRoute, error)

func (f rootFunc) ResolveGroup(ctx context.Context, msg InboundMessage) (GroupRoute, error) {
	return f(ctx, msg)
}

func TestRouteDirectSeparatesUsersAndBindings(t *testing.T) {
	msg := InboundMessage{BindingID: "bot-a", ChatKind: ChatDirect, ChatID: "chat", SenderID: "alice"}
	bot := BotBinding{ID: "bot-a", TenantID: "tenant"}
	a, _, err := Route(context.Background(), bot, msg, nil)
	if err != nil {
		t.Fatal(err)
	}
	msg.SenderID = "bob"
	b, _, err := Route(context.Background(), bot, msg, nil)
	if err != nil || a.String() == b.String() {
		t.Fatalf("users must have separate sessions: %v", err)
	}
	bot.ID, msg.BindingID, msg.SenderID = "bot-b", "bot-b", "alice"
	c, _, err := Route(context.Background(), bot, msg, nil)
	if err != nil || a.String() == c.String() {
		t.Fatalf("bindings must have separate sessions: %v", err)
	}
}

func TestRouteGroupSharesThreadButNotOtherThreads(t *testing.T) {
	bot := BotBinding{ID: "bot", TenantID: "tenant"}
	msg := InboundMessage{BindingID: "bot", ChatKind: ChatGroup, ChatID: "group", SenderID: "alice"}
	root := "root-one"
	resolver := rootFunc(func(context.Context, InboundMessage) (GroupRoute, error) { return GroupRoute{SubjectID: root}, nil })
	a, _, err := Route(context.Background(), bot, msg, resolver)
	if err != nil {
		t.Fatal(err)
	}
	msg.SenderID = "bob"
	b, _, err := Route(context.Background(), bot, msg, resolver)
	if err != nil || a.String() != b.String() {
		t.Fatalf("members of one thread must share a session: %v", err)
	}
	root = "root-two"
	c, _, err := Route(context.Background(), bot, msg, resolver)
	if err != nil || a.String() == c.String() {
		t.Fatalf("different threads must be isolated: %v", err)
	}
}

func TestRouteRejectsUnresolvedAndMismatchedMessages(t *testing.T) {
	bot := BotBinding{ID: "bot", TenantID: "tenant"}
	msg := InboundMessage{BindingID: "other", ChatKind: ChatGroup, ChatID: "group"}
	if _, _, err := Route(context.Background(), bot, msg, nil); err == nil {
		t.Fatal("mismatched binding accepted")
	}
	msg.BindingID = "bot"
	if _, _, err := Route(context.Background(), bot, msg, nil); err == nil {
		t.Fatal("unresolved group root accepted")
	}
	resolver := rootFunc(func(context.Context, InboundMessage) (GroupRoute, error) {
		return GroupRoute{}, errors.New("unknown thread")
	})
	if _, _, err := Route(context.Background(), bot, msg, resolver); err == nil {
		t.Fatal("unknown thread accepted")
	}
}
