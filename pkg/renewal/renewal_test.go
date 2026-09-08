package renewal

import (
	"context"
	"testing"
	"time"
)

func validInput() Input {
	now := time.Unix(2_000_000, 0).UTC()
	return Input{
		Now: now, PrincipalID: "principal", PrincipalEnabled: true, RunnerID: "runner", FabricID: "cloud", CreatedAt: now.Add(-time.Hour),
		State: State{Managed: true, ResourceRef: "resource", ResourceConfirmedAt: now.Add(-30 * time.Minute), EverReady: true, FactsConfirmed: true, ExpiresAt: now.Add(time.Hour)},
	}
}

func TestPersonalAndPolicyFunc(t *testing.T) {
	input := validInput()
	input.State.ExpiresAt = input.Now.Add(5 * time.Minute)
	personal := Personal{Config: DefaultConfig()}
	decision, err := personal.Decide(context.Background(), input)
	if err != nil || !decision.Renew || decision.Reason != "RENEW" || !decision.Until.Equal(input.Now.Add(time.Hour)) {
		t.Fatal("personal policy did not preserve the default decision", decision, err)
	}

	called := false
	policy := PolicyFunc(func(_ context.Context, got Input) (Decision, error) {
		called = got == input
		return Decision{Reason: "CUSTOM", RecheckAt: got.Now.Add(time.Minute)}, nil
	})
	decision, err = policy.Decide(context.Background(), input)
	if err != nil || !called || decision.Reason != "CUSTOM" {
		t.Fatal("policy function did not receive the bounded input", called, decision, err)
	}
}

func TestValidateDecisionEnforcesLifecycleSafety(t *testing.T) {
	input := validInput()
	validRenew := Decision{Renew: true, Until: input.Now.Add(2 * time.Hour), Reason: "ENTERPRISE_RENEW"}
	if err := ValidateDecision(input, validRenew); err != nil {
		t.Fatal("valid custom renewal was rejected", err)
	}
	if err := ValidateDecision(input, Decision{Reason: "WAIT", RecheckAt: input.Now.Add(time.Minute)}); err != nil {
		t.Fatal("valid custom recheck was rejected", err)
	}
	disabledPrincipal := input
	disabledPrincipal.PrincipalEnabled = false
	if err := ValidateDecision(disabledPrincipal, validRenew); err != nil {
		t.Fatal("validator overrode enterprise account-state policy", err)
	}

	tests := []struct {
		name     string
		change   func(*Input)
		decision Decision
	}{
		{name: "missing principal", change: func(i *Input) { i.PrincipalID = "" }, decision: validRenew},
		{name: "unconfirmed facts", change: func(i *Input) { i.State.FactsConfirmed = false }, decision: validRenew},
		{name: "expired resource", change: func(i *Input) { i.State.ExpiresAt = i.Now }, decision: validRenew},
		{name: "pending mutation", change: func(i *Input) { i.State.MutationPending = true }, decision: validRenew},
		{name: "destroying", change: func(i *Input) { i.State.Destroying = true }, decision: validRenew},
		{name: "bootstrap failed", change: func(i *Input) { i.State.BootstrapFailed = true }, decision: validRenew},
		{name: "past target", decision: Decision{Renew: true, Until: input.Now, Reason: "RENEW"}},
		{name: "target does not extend expiry", decision: Decision{Renew: true, Until: input.State.ExpiresAt, Reason: "RENEW"}},
		{name: "target rounds to expiry", decision: Decision{Renew: true, Until: input.State.ExpiresAt.Add(time.Nanosecond), Reason: "RENEW"}},
		{name: "renewal and recheck", decision: Decision{Renew: true, Until: input.Now.Add(2 * time.Hour), RecheckAt: input.Now.Add(time.Minute), Reason: "RENEW"}},
		{name: "stop with target", decision: Decision{Until: input.Now.Add(time.Hour), Reason: "STOP"}},
		{name: "past recheck", decision: Decision{Reason: "WAIT", RecheckAt: input.Now}},
		{name: "recheck rounds to now", decision: Decision{Reason: "WAIT", RecheckAt: input.Now.Add(time.Nanosecond)}},
		{name: "unbounded reason", decision: Decision{Reason: string(make([]byte, 129))}},
		{name: "freeform reason", decision: Decision{Reason: "private policy detail"}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			candidate := input
			if test.change != nil {
				test.change(&candidate)
			}
			if err := ValidateDecision(candidate, test.decision); err == nil {
				t.Fatal("unsafe policy decision was accepted")
			}
		})
	}
}

func TestPersonalReturnsConfigurationErrors(t *testing.T) {
	_, err := (Personal{}).Decide(context.Background(), validInput())
	if err == nil {
		t.Fatal("invalid personal configuration was accepted", err)
	}
}

func TestNilPolicyFuncReturnsError(t *testing.T) {
	var policy PolicyFunc
	if _, err := policy.Decide(context.Background(), validInput()); err == nil {
		t.Fatal("nil policy function was called")
	}
}
