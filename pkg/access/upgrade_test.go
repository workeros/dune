package access

import (
	"strings"
	"testing"
	"time"

	"github.com/aiomni/dune/pkg/api"
	"github.com/aiomni/dune/pkg/upgrade"
	pb "github.com/aiomni/dune/proto/dune/dtp/v1"
)

func TestUpgradeDispatchRequiresReservationAndOriginalScope(t *testing.T) {
	scope := testScope()
	submission := upgrade.Submission{
		Request:    upgrade.Request{SubmissionID: "submission", Binding: scope.Binding, InstallationID: "installation", ExpectedInstallationRevision: "1", ExpectedRunningSHA256: strings.Repeat("a", 64), Release: upgrade.ReleaseRef{ID: "release", ManifestSHA256: strings.Repeat("b", 64)}},
		ReservedAt: time.Now().UTC(),
	}
	for _, change := range []string{"valid", "missing-reservation", "rebound", "runtime"} {
		t.Run(change, func(t *testing.T) {
			request := submission
			message := &pb.Message{Kind: "request", RequestId: "request", Target: scope.Binding.MachineID, Operation: "runner.upgrade.start"}
			switch change {
			case "missing-reservation":
				request.ReservedAt = time.Time{}
			case "rebound":
				request.Request.Binding.Revision++
			case "runtime":
				message.RuntimeId = "runtime"
			}
			message.Payload = api.Payload(request)
			_, err := Describe(scope, message)
			if (err == nil) != (change == "valid") {
				t.Fatal("unexpected dispatch authorization", err)
			}
		})
	}
}
