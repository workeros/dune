package channel

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
)

type registryProvider struct {
	kind   string
	opened bool
}

func (p *registryProvider) Kind() string { return p.kind }
func (p *registryProvider) Validate(config json.RawMessage, credentials []byte) error {
	if string(credentials) != "secret" {
		return errors.New("bad credentials")
	}
	return nil
}
func (p *registryProvider) Open(context.Context, BotBinding, []byte, EventSink) (Channel, error) {
	p.opened = true
	return registryChannel{}, nil
}

type registryChannel struct{}

func (registryChannel) Start(context.Context) error { return nil }
func (registryChannel) Stop(context.Context) error  { return nil }
func (registryChannel) Send(context.Context, ReplyAddress, OutboundMessage) (json.RawMessage, error) {
	return nil, nil
}

type registrySink struct{}

func (registrySink) Accept(context.Context, InboundMessage) error { return nil }

func TestRegistryResolvesOpenProviderAndRejectsDuplicateKind(t *testing.T) {
	var registry Registry
	provider := &registryProvider{kind: "future_platform"}
	if err := registry.Register(provider); err != nil {
		t.Fatal(err)
	}
	if err := registry.Register(&registryProvider{kind: "future_platform"}); err == nil {
		t.Fatal("duplicate kind registered")
	}
	if _, err := registry.Open(context.Background(), BotBinding{Provider: "future_platform"}, []byte("bad"), registrySink{}); err == nil || provider.opened {
		t.Fatal("invalid credentials reached Open")
	}
	if _, err := registry.Open(context.Background(), BotBinding{Provider: "future_platform"}, []byte("secret"), registrySink{}); err != nil || !provider.opened {
		t.Fatalf("registered provider did not open: %v", err)
	}
	if _, err := registry.Open(context.Background(), BotBinding{Provider: "absent"}, nil, registrySink{}); err == nil {
		t.Fatal("unregistered provider opened")
	}
}
