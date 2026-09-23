package agents

import "context"

const (
	DirectoryReady       = "ready"
	DirectoryMember      = "member"
	DirectoryMetadata    = "metadata"
	DirectoryInvalidated = "invalidated"
	MaxDirectoryMembers  = 4096
)

// DirectoryWatch selects an authorized Tenant range, optionally restricted to
// logical Runner IDs. It is not a list of Runtime attachments or a page cursor.
type DirectoryWatch struct {
	RunnerIDs []string `json:"runner_ids,omitempty"`
}

type DirectoryEvent struct {
	SubscriptionID string `json:"subscription_id"`
	Kind           string `json:"kind"`
	Agent          *Agent `json:"agent,omitempty"`
	Code           string `json:"code,omitempty"`
}

// DirectorySubscription is ready when Subscribe returns. Every error other
// than a caller's Next deadline invalidates it. Close releases all Runner
// sources. Neither reopening nor discovering may issue native Agent controls.
type DirectorySubscription interface {
	ID() string
	Next(context.Context) (DirectoryEvent, error)
	Close() error
}

// DirectoryObserver separates streaming from one-shot discovery providers.
// Host.AgentDirectoryObserver exposes this public service alongside Directory.
type DirectoryObserver interface {
	Subscribe(context.Context, Scope, DirectoryWatch) (DirectorySubscription, error)
}
