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
	observe       func(RenewalDecisionObservation)
}

// RenewalDecisionObservation contains the exact policy result accepted by the
// metadata transaction. It excludes provider responses, template parameters,
// credentials and identity subject.
type RenewalDecisionObservation struct {
	ObservedAt    time.Time
	NextCheckAt   time.Time
	RenewUntil    time.Time
	PrincipalID   string
	Namespace     string
	RunnerID      string
	FabricID      string
	ResourceRef   string
	PolicyVersion string
	Reason        string
	Outcome       string
}

func notifyRenewalDecision(observer func(RenewalDecisionObservation), observation RenewalDecisionObservation) {
	if observer == nil {
		return
	}
	defer func() { _ = recover() }()
	observer(observation)
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
		if inspection.ResourceRef != "" || !inspection.ExpiresAt.IsZero() || inspection.Gone || inspection.State != "" || inspection.Capabilities != nil {
			return lifecycle.ResourceInspection{}, ErrProviderContract
		}
		return lifecycle.ResourceInspection{Status: lifecycle.InspectionUnknown}, nil
	}
	if inspection.Status != fabric.InspectionConfirmed || inspection.ResourceRef == "" || len(inspection.ResourceRef) > 1024 || !utf8.ValidString(inspection.ResourceRef) || strings.ContainsFunc(inspection.ResourceRef, unicode.IsControl) ||
		(inspection.Gone && !inspection.ExpiresAt.IsZero()) || (!inspection.Gone && (inspection.ExpiresAt.IsZero() || inspection.ExpiresAt.UnixMilli() <= 0)) {
		return lifecycle.ResourceInspection{}, ErrProviderContract
	}
	if !inspection.Gone && inspection.State != "" && inspection.State != fabric.ResourceReady && inspection.State != fabric.ResourcePaused && inspection.State != fabric.ResourceUnknown {
		return lifecycle.ResourceInspection{}, ErrProviderContract
	}
	state := string(inspection.State)
	if state == "" && !inspection.Gone {
		state = string(fabric.ResourceUnknown)
	}
	return lifecycle.ResourceInspection{Status: lifecycle.InspectionConfirmed, ResourceRef: inspection.ResourceRef, ExpiresAt: inspection.ExpiresAt, Gone: inspection.Gone, State: state, Capabilities: inspection.Capabilities}, nil
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
		ProviderBindingID: claimed.ProviderBindingID, ProviderBindingRevision: claimed.ProviderBindingRevision,
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
	outcome := "stopped"
	if policyErr != nil {
		decision = renewal.Decision{Reason: "POLICY_ERROR", RecheckAt: input.Now.Add(renewalPolicyFailureRecheck)}
		outcome = "policy_error"
	} else if renewal.ValidateDecision(input, decision) != nil {
		decision = renewal.Decision{Reason: "POLICY_INVALID", RecheckAt: input.Now.Add(renewalPolicyFailureRecheck)}
		outcome = "policy_invalid"
	} else if decision.Renew {
		outcome = "renew"
	} else if !decision.RecheckAt.IsZero() {
		outcome = "recheck"
	}
	saved, err := e.store.RecordManagedRenewalDecision(ctx, claimed, e.policyVersion, input, result, decision)
	if err != nil {
		return err
	}
	notifyRenewalDecision(e.observe, RenewalDecisionObservation{
		ObservedAt: saved.ObservedAt, NextCheckAt: saved.NextCheckAt, RenewUntil: saved.RenewUntil,
		PrincipalID: input.PrincipalID, Namespace: input.Namespace,
		RunnerID: saved.RunnerID, FabricID: saved.FabricID, ResourceRef: saved.ResourceRef,
		PolicyVersion: saved.PolicyVersion, Reason: saved.Reason, Outcome: outcome,
	})
	return nil
}
