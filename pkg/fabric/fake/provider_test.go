package fake

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/aiomni/dune/pkg/fabric"
)

func createCall() fabric.CreateCall {
	return fabric.CreateCall{Action: fabric.Action{ID: "create-1", RunnerID: "runner-1", RequestDigest: "digest"}}
}

func TestProviderLifecycleAndReadOnlyReconciliation(t *testing.T) {
	now := time.Date(2026, 9, 15, 12, 0, 0, 0, time.UTC)
	p := New(Config{TTL: time.Hour, Now: func() time.Time { return now }})
	ctx := context.Background()
	available, err := p.Availability(ctx)
	if err != nil || !available.Available {
		t.Fatal("fake provider unavailable", available, err)
	}
	created, err := p.Create(ctx, createCall())
	if err != nil || created.Outcome != fabric.OutcomeSucceeded || created.ResourceRef != "fake:runner-1" || !created.ExpiresAt.Equal(now.Add(time.Hour)) {
		t.Fatal("create failed", created, err)
	}
	again, err := p.Create(ctx, createCall())
	if err != nil || again.Outcome != created.Outcome || again.ResourceRef != created.ResourceRef || !again.ExpiresAt.Equal(created.ExpiresAt) || again.State != created.State {
		t.Fatal("same create action was not idempotent", again, err)
	}
	unknown, err := p.ReconcileCreate(ctx, fabric.ReconcileCall{Action: fabric.Action{ID: "missing"}})
	if err != nil || unknown.Outcome != fabric.OutcomeUnknown {
		t.Fatal("missing create was not unknown", unknown, err)
	}
	reconciled, err := p.ReconcileCreate(ctx, fabric.ReconcileCall{Action: createCall().Action, KnownResourceRef: created.ResourceRef})
	if err != nil || reconciled.Outcome != created.Outcome || reconciled.ResourceRef != created.ResourceRef || !reconciled.ExpiresAt.Equal(created.ExpiresAt) || reconciled.State != created.State {
		t.Fatal("create reconciliation changed facts", reconciled, err)
	}
	bootstrap := fabric.Action{ID: "bootstrap-1", ResourceRef: created.ResourceRef}
	bootstrapCall := fabric.BootstrapCall{Action: bootstrap, EnrollmentToken: "one-shot", Version: "v1"}
	if observed, err := p.Bootstrap(ctx, bootstrapCall); err != nil || observed.Outcome != fabric.OutcomeSucceeded {
		t.Fatal("bootstrap failed", observed, err)
	}
	if observed, err := p.Bootstrap(ctx, bootstrapCall); err != nil || observed.Outcome != fabric.OutcomeSucceeded {
		t.Fatal("same bootstrap action was not idempotent", observed, err)
	}
	if observed, err := p.ReconcileBootstrap(ctx, fabric.BootstrapReconcileCall{Action: bootstrap}); err != nil || observed.Outcome != fabric.OutcomeSucceeded {
		t.Fatal("bootstrap reconciliation failed", observed, err)
	}
	if err := p.Pause(ctx, fabric.PauseResumeCall{ResourceRef: created.ResourceRef}); err != nil {
		t.Fatal(err)
	}
	inspection, err := p.Inspect(ctx, fabric.InspectCall{ResourceRef: created.ResourceRef})
	if err != nil || inspection.Status != fabric.InspectionConfirmed || inspection.State != fabric.ResourcePaused || inspection.Capabilities == nil || !inspection.Capabilities.PauseResume {
		t.Fatal("pause facts missing", inspection, err)
	}
	if err := p.Resume(ctx, fabric.PauseResumeCall{ResourceRef: created.ResourceRef}); err != nil {
		t.Fatal(err)
	}
	renew := fabric.Action{ID: "renew-1", ResourceRef: created.ResourceRef, RenewUntil: now.Add(2 * time.Hour)}
	if observed, err := p.Renew(ctx, fabric.RenewCall{Action: renew}); err != nil || !observed.ExpiresAt.Equal(renew.RenewUntil) {
		t.Fatal("renew failed", observed, err)
	}
	if observed, err := p.ReconcileRenew(ctx, fabric.RenewReconcileCall{Action: renew}); err != nil || !observed.ExpiresAt.Equal(renew.RenewUntil) {
		t.Fatal("renew reconciliation failed", observed, err)
	}
	candidate, err := p.VerifyCandidate(ctx, fabric.CandidateCall{Action: createCall().Action, CandidateResourceRef: created.ResourceRef})
	if err != nil || candidate.Outcome != fabric.OutcomeSucceeded {
		t.Fatal("candidate verification failed", candidate, err)
	}
	destroy := fabric.Action{ID: "destroy-1", ResourceRef: created.ResourceRef}
	if observed, err := p.Destroy(ctx, fabric.DestroyCall{Action: destroy}); err != nil || observed.Outcome != fabric.OutcomeSucceeded || !observed.Gone {
		t.Fatal("destroy failed", observed, err)
	}
	if observed, err := p.ReconcileDestroy(ctx, fabric.DestroyReconcileCall{Action: destroy}); err != nil || !observed.Gone {
		t.Fatal("destroy reconciliation failed", observed, err)
	}
	inspection, err = p.Inspect(ctx, fabric.InspectCall{ResourceRef: created.ResourceRef})
	if err != nil || inspection.Status != fabric.InspectionConfirmed || !inspection.Gone {
		t.Fatal("destroyed resource facts missing", inspection, err)
	}
	if err := p.Resume(ctx, fabric.PauseResumeCall{ResourceRef: created.ResourceRef}); err == nil {
		t.Fatal("destroyed resource resumed")
	}
}

func TestProviderRejectsActionConflictsAndHonorsCancellation(t *testing.T) {
	p := New(Config{})
	ctx := context.Background()
	if _, err := p.Create(ctx, createCall()); err != nil {
		t.Fatal(err)
	}
	conflict := createCall()
	conflict.Action.RunnerID = "runner-2"
	if _, err := p.Create(ctx, conflict); err == nil {
		t.Fatal("same action ID accepted different create content")
	}
	if observed, err := p.ReconcileCreate(ctx, fabric.ReconcileCall{Action: createCall().Action, KnownResourceRef: "fake:other"}); err != nil || observed.Outcome != fabric.OutcomeUnknown {
		t.Fatal("mismatched known resource was not unknown", observed, err)
	}
	created, err := p.ReconcileCreate(ctx, fabric.ReconcileCall{Action: createCall().Action})
	if err != nil || created.Outcome != fabric.OutcomeSucceeded {
		t.Fatal("create reconciliation failed", created, err)
	}
	bootstrap := fabric.BootstrapCall{Action: fabric.Action{ID: "bootstrap-1", ResourceRef: created.ResourceRef}, EnrollmentToken: "token-a", Version: "v1"}
	if _, err := p.Bootstrap(ctx, bootstrap); err != nil {
		t.Fatal(err)
	}
	bootstrap.EnrollmentToken = "token-b"
	if _, err := p.Bootstrap(ctx, bootstrap); err == nil {
		t.Fatal("same action ID accepted different bootstrap content")
	}
	renew := fabric.RenewCall{Action: fabric.Action{ID: "renew-1", ResourceRef: created.ResourceRef, RenewUntil: time.Now().UTC().Add(time.Hour)}}
	if _, err := p.Renew(ctx, renew); err != nil {
		t.Fatal(err)
	}
	renew.Action.RenewUntil = renew.Action.RenewUntil.Add(time.Hour)
	if _, err := p.Renew(ctx, renew); err == nil {
		t.Fatal("same action ID accepted a different renewal target")
	}
	canceled, cancel := context.WithCancel(ctx)
	cancel()
	if _, err := p.Availability(canceled); !errors.Is(err, context.Canceled) {
		t.Fatal("canceled provider call continued", err)
	}
}
