package lifecycle

import (
	"testing"
	"time"
)

func TestPersonalRenewalLifecycle(t *testing.T) {
	now := time.Date(2026, 9, 7, 12, 0, 0, 0, time.UTC)
	healthy := RenewalState{Managed: true, ResourceRef: "bound-resource", ResourceConfirmedAt: now.Add(-time.Minute), FactsConfirmed: true, ExpiresAt: now.Add(time.Minute)}
	for _, test := range []struct {
		name           string
		change         func(*RenewalState)
		reason         string
		renew, recheck bool
	}{
		{"first connection within grace", func(*RenewalState) {}, "RENEW", true, false},
		{"ever ready no longer uses first connection grace", func(s *RenewalState) { s.EverReady = true; s.ResourceConfirmedAt = now.Add(-time.Hour) }, "RENEW", true, false},
		{"attached never acquires TTL", func(s *RenewalState) { s.Managed = false }, "ATTACHED", false, false},
		{"confirmed external deletion", func(s *RenewalState) { s.ResourceGone = true }, "RESOURCE_GONE", false, false},
		{"destroy retains access and renewal restriction", func(s *RenewalState) { s.Destroying = true }, "DESTROYING", false, false},
		{"bootstrap explicitly failed", func(s *RenewalState) { s.BootstrapFailed = true }, "BOOTSTRAP_FAILED", false, false},
		{"unknown create has no target", func(s *RenewalState) { s.ResourceRef = "" }, "RESOURCE_UNKNOWN", false, false},
		{"unresolved external mutation", func(s *RenewalState) { s.MutationPending = true }, "MUTATION_PENDING", false, true},
		{"lookup failed despite cached future expiry", func(s *RenewalState) { s.FactsConfirmed = false }, "FACTS_UNKNOWN", false, true},
		{"expiry missing", func(s *RenewalState) { s.ExpiresAt = time.Time{} }, "FACTS_UNKNOWN", false, true},
		{"past expiry is not proof of deletion", func(s *RenewalState) { s.ExpiresAt = now.Add(-time.Second) }, "EXPIRY_REQUIRES_RECONCILIATION", false, true},
		{"expiry boundary", func(s *RenewalState) { s.ExpiresAt = now }, "EXPIRY_REQUIRES_RECONCILIATION", false, true},
		{"not yet due", func(s *RenewalState) { s.ExpiresAt = now.Add(time.Hour) }, "NOT_DUE", false, true},
		{"due boundary", func(s *RenewalState) { s.ExpiresAt = now.Add(10 * time.Minute) }, "RENEW", true, false},
		{"grace boundary", func(s *RenewalState) { s.ResourceConfirmedAt = now.Add(-5 * time.Minute) }, "FIRST_CONNECTION_TIMEOUT", false, false},
		{"grace expired", func(s *RenewalState) { s.ResourceConfirmedAt = now.Add(-time.Hour) }, "FIRST_CONNECTION_TIMEOUT", false, false},
	} {
		t.Run(test.name, func(t *testing.T) {
			state := healthy
			test.change(&state)
			before := state
			got, err := DecideRenewal(now, DefaultRenewalConfig(), state)
			if err != nil || got.Reason != test.reason || got.Renew != test.renew || got.RecheckAt.IsZero() == test.recheck {
				t.Fatal(got, err)
			}
			if state != before {
				t.Fatal("decision changed lifecycle facts")
			}
			if got.Renew && !got.Until.Equal(now.Add(time.Hour)) {
				t.Fatal("renewal did not capture absolute target", got)
			}
			if !got.Renew && !got.Until.IsZero() {
				t.Fatal("non-renewal supplied a mutation target", got)
			}
		})
	}
}

func TestRenewalUsesPersistedGraceAndCurrentFacts(t *testing.T) {
	confirmed := time.Date(2026, 9, 7, 12, 0, 0, 0, time.UTC)
	config := DefaultRenewalConfig()
	state := RenewalState{Managed: true, ResourceRef: "bound-resource", ResourceConfirmedAt: confirmed, FactsConfirmed: true, ExpiresAt: confirmed.Add(time.Hour)}
	// A fresh worker, another query or a browser closing cannot restart grace.
	now := confirmed.Add(5*time.Minute - time.Second)
	got, err := DecideRenewal(now, config, state)
	if err != nil || got.Renew || !got.RecheckAt.Equal(confirmed.Add(5*time.Minute)) {
		t.Fatal("poll exceeded original grace", got, err)
	}
	got, err = DecideRenewal(confirmed.Add(5*time.Minute), config, state)
	if err != nil || got.Reason != "FIRST_CONNECTION_TIMEOUT" {
		t.Fatal("worker restart reset grace", got, err)
	}
	// Once usable, a disconnect does not reapply first-connection grace. Failed
	// inspection still cannot use the old expiry as permission to renew.
	state.EverReady = true
	state.FactsConfirmed = false
	now = confirmed.Add(55 * time.Minute)
	got, err = DecideRenewal(now, config, state)
	if err != nil || got.Renew || got.RecheckAt.IsZero() {
		t.Fatal("offline resource followed cached facts", got, err)
	}
	state.FactsConfirmed = true
	got, err = DecideRenewal(now, config, state)
	if err != nil || !got.Renew {
		t.Fatal("confirmed offline resource stopped maintenance", got, err)
	}
	// Repeated policy evaluation is not an idempotency mechanism. The executor
	// must keep the original Until with the submitted action, not use a new one.
	first := got.Until
	later, err := DecideRenewal(now.Add(time.Second), config, state)
	if err != nil || !later.Until.After(first) {
		t.Fatal(later, err)
	}
	if !got.Until.Equal(first) {
		t.Fatal("original decision changed")
	}
}

func TestRenewalScheduleAndConfiguration(t *testing.T) {
	now := time.Date(2026, 9, 7, 12, 0, 0, 0, time.UTC)
	state := RenewalState{Managed: true, EverReady: true, ResourceRef: "resource", ResourceConfirmedAt: now.Add(-time.Hour), FactsConfirmed: true, ExpiresAt: now.Add(10*time.Minute + time.Second)}
	config := DefaultRenewalConfig()
	got, err := DecideRenewal(now, config, state)
	if err != nil || !got.RecheckAt.Equal(now.Add(time.Second)) {
		t.Fatal("poll missed renewal threshold", got, err)
	}
	state.ExpiresAt = now.Add(time.Hour)
	got, err = DecideRenewal(now, config, state)
	if err != nil || !got.RecheckAt.Equal(now.Add(50*time.Minute)) {
		t.Fatal("known expiry scheduled unnecessary polling", got, err)
	}
	config.Enabled = false
	got, err = DecideRenewal(now, config, state)
	if err != nil || got.Reason != "DISABLED" || got.Renew {
		t.Fatal(got, err)
	}
	config = DefaultRenewalConfig()
	config.FirstConnectionGrace = 0
	state.EverReady = false
	got, err = DecideRenewal(now, config, state)
	if err != nil || got.Reason != "FIRST_CONNECTION_TIMEOUT" {
		t.Fatal("explicit zero grace replaced with default", got, err)
	}
	for _, invalid := range []RenewalConfig{
		{}, {ExtendBy: time.Hour, RenewBefore: time.Hour}, {ExtendBy: time.Hour, RenewBefore: 2 * time.Hour},
		{ExtendBy: time.Hour, RenewBefore: time.Minute, FirstConnectionGrace: -1},
		{ExtendBy: -1, RenewBefore: time.Minute}, {ExtendBy: time.Hour, RenewBefore: -1},
	} {
		if _, err := DecideRenewal(now, invalid, state); err == nil {
			t.Fatal("invalid configuration accepted", invalid)
		}
	}
	config = DefaultRenewalConfig()
	for _, invalid := range []time.Time{{}, now.Add(time.Second)} {
		state.ResourceConfirmedAt = invalid
		if _, err := DecideRenewal(now, config, state); err == nil {
			t.Fatal("unknown or reset confirmation time accepted")
		}
	}
	if _, err := DecideRenewal(time.Time{}, config, state); err == nil {
		t.Fatal("missing clock accepted")
	}
}
