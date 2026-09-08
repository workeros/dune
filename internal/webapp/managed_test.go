package webapp

import (
	"testing"
	"time"

	"github.com/aiomni/dune/internal/lifecycle"
)

func TestStatusResponseIncludesRenewalDecision(t *testing.T) {
	now := time.Now().UTC().Truncate(time.Millisecond)
	view := statusResponse(lifecycle.ManagedStatus{
		Operation:            lifecycle.Operation{Intent: lifecycle.Intent{ID: "operation", RunnerID: "runner", FabricID: "cloud", BindingRevision: 1, Action: "create"}, CreatedAt: now},
		RenewalPolicyVersion: "enterprise-v4", RenewalReason: "ENTERPRISE_WAIT",
		RenewalObservedAt: now, RenewalNextCheckAt: now.Add(time.Minute),
	})
	if view.RenewalPolicyVersion != "enterprise-v4" || view.RenewalReason != "ENTERPRISE_WAIT" || view.RenewalObservedAt == nil || !view.RenewalObservedAt.Equal(now) || view.RenewalNextCheckAt == nil || !view.RenewalNextCheckAt.Equal(now.Add(time.Minute)) || view.RenewalUntil != nil {
		t.Fatal("renewal decision was omitted from the public status view", view)
	}
}
