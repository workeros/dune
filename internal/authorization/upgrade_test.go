package authorization

import (
	"context"
	"testing"
	"time"

	"github.com/aiomni/dune/pkg/access"
	"github.com/aiomni/dune/pkg/runner"
)

type verificationRepository struct {
	Repository
	resource Resource
}

func (r *verificationRepository) MachineResource(context.Context, string) (Resource, error) {
	return r.resource, nil
}

func TestMachineVerificationOnlyAllowsProbeAndRechecksBinding(t *testing.T) {
	service, sessions, _, _ := streamCheckFixture()
	binding := runner.Binding{RunnerID: "runner", MachineID: "machine", FabricID: "fabric", Revision: 1}
	repository := &verificationRepository{resource: Resource{OwnerID: "tenant", Runner: runner.Runner{ID: binding.RunnerID, Binding: &binding}}}
	service.bindings = repository
	record := ConnectionAccess{UpgradeVerification: true, Background: true, PrincipalID: binding.MachineID, PrincipalKind: "runner_verifier", Namespace: sessions.Namespace(), Target: binding.MachineID, RunnerID: binding.RunnerID, FabricID: binding.FabricID, BindingRevision: 1, OwnerID: "tenant", ExpiresAt: time.Now().Add(TicketLifetime).Unix()}
	policy := service.streamPolicy(record)
	request := access.Request{Scope: record.Scope(), RequestID: "probe-request", Operation: "runner.upgrade.probe"}
	if decision, err := access.Evaluate(t.Context(), policy.Checker, request); err != nil || !decision.Allowed {
		t.Fatal(decision, err)
	}
	for _, operation := range []string{"exec", "profile.start", "runner.upgrade.start", "runtime.list"} {
		request.Operation = operation
		if _, err := access.Evaluate(t.Context(), policy.Checker, request); err == nil {
			t.Fatal("system verification granted unrelated operation", operation)
		}
	}
	request.Operation = "runner.upgrade.probe"
	binding.Revision++
	if _, err := access.Evaluate(t.Context(), policy.Checker, request); err == nil {
		t.Fatal("verification followed a replacement binding")
	}
	if sessions.calls != 0 {
		t.Fatal("worker verification depended on browser session")
	}
}
