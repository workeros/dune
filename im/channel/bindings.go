package channel

import "context"

// BindingStore scopes bot configuration to a Tenant. A Binding ID is globally
// unique; Put uses Revision as compare-and-swap (zero creates, nonzero updates)
// and must reject moving an existing Binding to another Tenant.
type BindingStore interface {
	Put(context.Context, BotBinding) (BotBinding, error)
	GetBinding(context.Context, string, string) (BotBinding, bool, error)
	GetBindingByID(context.Context, string) (BotBinding, bool, error)
	ListBindings(context.Context, string) ([]BotBinding, error)
}
