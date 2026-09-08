package managed

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/aiomni/dune/internal/lifecycle"
	"github.com/aiomni/dune/internal/metadata"
	"github.com/aiomni/dune/pkg/fabric"
	"github.com/aiomni/dune/pkg/renewal"
)

const renewalPolicyFailureRecheck = 30 * time.Second

type MaintenanceExecutor struct {
	store         *metadata.Store
	providers     map[string]fabric.InspectProvider
	policy        renewal.Policy
	policyVersion string
	policyTimeout time.Duration
}

func NewMaintenanceExecutor(store *metadata.Store, providers map[string]fabric.InspectProvider, policy renewal.Policy, policyVersion string, policyTimeout time.Duration) (*MaintenanceExecutor, error) {
	if store == nil || policy == nil || !validProviderName(policyVersion) || policyTimeout <= 0 || policyTimeout > 10*time.Minute {
		return nil, fmt.Errorf("invalid managed renewal configuration")
	}
	copy := make(map[string]fabric.InspectProvider, len(providers))
	for id, provider := range providers {
		if !validProviderName(id) || id == "attached" || provider == nil {
			return nil, fmt.Errorf("invalid managed inspection provider configuration")
		}
		copy[id] = provider
	}
	return &MaintenanceExecutor{store: store, providers: copy, policy: policy, policyVersion: policyVersion, policyTimeout: policyTimeout}, nil
}

func validProviderName(value string) bool {
	return value != "" && len(value) <= 128 && utf8.ValidString(value) && strings.TrimSpace(value) == value && !strings.ContainsFunc(value, unicode.IsControl)
}

func inspectionResult(providerCtx context.Context, inspection fabric.Inspection, err error) (lifecycle.ResourceInspection, error) {
	if err != nil {
		status := lifecycle.InspectionUnknown
		if errors.Is(err, context.DeadlineExceeded) || errors.Is(providerCtx.Err(), context.DeadlineExceeded) {
			status = lifecycle.InspectionTimedOut
		}
		return lifecycle.ResourceInspection{Status: status}, nil
	}
	if inspection.Status == fabric.InspectionUnknown {
		if inspection.ResourceRef != "" || !inspection.ExpiresAt.IsZero() || inspection.Gone {
			return lifecycle.ResourceInspection{}, ErrProviderContract
		}
		return lifecycle.ResourceInspection{Status: lifecycle.InspectionUnknown}, nil
	}
	if inspection.Status != fabric.InspectionConfirmed || inspection.ResourceRef == "" || len(inspection.ResourceRef) > 1024 || !utf8.ValidString(inspection.ResourceRef) || strings.ContainsFunc(inspection.ResourceRef, unicode.IsControl) ||
		(inspection.Gone && !inspection.ExpiresAt.IsZero()) || (!inspection.Gone && (inspection.ExpiresAt.IsZero() || inspection.ExpiresAt.UnixMilli() <= 0)) {
		return lifecycle.ResourceInspection{}, ErrProviderContract
	}
	return lifecycle.ResourceInspection{Status: lifecycle.InspectionConfirmed, ResourceRef: inspection.ResourceRef, ExpiresAt: inspection.ExpiresAt, Gone: inspection.Gone}, nil
}

// Execute inspects one claimed resource and evaluates its policy outside SQL
// transactions, then atomically rechecks the snapshot and saves the result.
func (e *MaintenanceExecutor) Execute(ctx context.Context, providerCtx context.Context, claimed lifecycle.RenewalSchedule) error {
	provider, ok := e.providers[claimed.FabricID]
	if !ok {
		return ErrProviderUnavailable
	}
	inspection, err := provider.Inspect(providerCtx, fabric.InspectCall{
		RunnerID: claimed.RunnerID, FabricID: claimed.FabricID, ResourceRef: claimed.ResourceRef, BindingRevision: claimed.BindingRevision,
	})
	result, resultErr := inspectionResult(providerCtx, inspection, err)
	if resultErr != nil {
		return resultErr
	}
	input, err := e.store.ManagedRenewalPolicyInput(ctx, claimed, result)
	if err != nil {
		return err
	}
	policyCtx, cancel := context.WithTimeout(ctx, e.policyTimeout)
	decision, policyErr := e.policy.Decide(policyCtx, input)
	cancel()
	if policyErr != nil {
		decision = renewal.Decision{Reason: "POLICY_ERROR", RecheckAt: input.Now.Add(renewalPolicyFailureRecheck)}
	} else if renewal.ValidateDecision(input, decision) != nil {
		decision = renewal.Decision{Reason: "POLICY_INVALID", RecheckAt: input.Now.Add(renewalPolicyFailureRecheck)}
	}
	_, err = e.store.RecordManagedRenewalDecision(ctx, claimed, e.policyVersion, input, result, decision)
	return err
}
