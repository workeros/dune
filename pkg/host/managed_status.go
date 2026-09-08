package host

import (
	"context"
	"time"

	"github.com/aiomni/dune/internal/metadata"
)

type managedStatusConfig struct {
	policyVersion    string
	expiryRiskWindow time.Duration
}

// ManagedStatus is a coarse, global lifecycle snapshot for a trusted host
// operator. It contains counts only, with no principal or resource identity.
// Enabled is false when this App has no Managed configuration; the remaining
// fields are then zero.
type ManagedStatus struct {
	Enabled             bool      `json:"enabled"`
	CheckedAt           time.Time `json:"checked_at,omitempty"`
	Runners             int64     `json:"runners"`
	KnownResources      int64     `json:"known_resources"`
	AccessibleResources int64     `json:"accessible_resources"`
	UnknownOperations   int64     `json:"unknown_operations"`
	TimedOutOperations  int64     `json:"timed_out_operations"`
	RenewalBacklog      int64     `json:"renewal_backlog"`
	ExpiryRiskResources int64     `json:"expiry_risk_resources"`
	ResidualResources   int64     `json:"residual_resources"`
}

// ManagedStatusSnapshot reads aggregate lifecycle pressure using the shared
// database clock and the App's configured renewal policy. The caller must
// authenticate and authorize its operator before calling this trusted method.
// It does not contact providers or imply that an external platform is healthy.
func (a *App) ManagedStatusSnapshot(ctx context.Context) (ManagedStatus, error) {
	ctx, finish, err := a.adminContext(ctx)
	if err != nil {
		return ManagedStatus{}, err
	}
	defer finish()
	if a.managedStatus == nil {
		return ManagedStatus{Enabled: false}, nil
	}
	status, err := a.store.ManagedStatusSnapshot(ctx, a.managedStatus.policyVersion, a.managedStatus.expiryRiskWindow)
	if err != nil {
		return ManagedStatus{}, err
	}
	return managedStatusFromMetadata(status), nil
}

func managedStatusFromMetadata(status metadata.ManagedStatus) ManagedStatus {
	return ManagedStatus{
		Enabled: true, CheckedAt: status.CheckedAt, Runners: status.Runners,
		KnownResources: status.KnownResources, AccessibleResources: status.AccessibleResources,
		UnknownOperations: status.UnknownOperations, TimedOutOperations: status.TimedOutOperations,
		RenewalBacklog: status.RenewalBacklog, ExpiryRiskResources: status.ExpiryRiskResources,
		ResidualResources: status.ResidualResources,
	}
}
