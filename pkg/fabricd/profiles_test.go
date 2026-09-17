package fabricd

import (
	"strings"
	"testing"

	"github.com/aiomni/dune/internal/wire"
	"github.com/aiomni/dune/pkg/api"
	pb "github.com/aiomni/dune/proto/dune/dtp/v1"
	"google.golang.org/protobuf/proto"
)

func TestProfileFailureIsReducedToProtocolFrame(t *testing.T) {
	result := api.ExecResult{Stdout: strings.Repeat("\x00", 512*1024), Stderr: strings.Repeat("\x00", 512*1024)}
	progress, failure := profileSetupFailure("oversized-output-attempt", "environment", 0, strings.Repeat("n", api.MaxProfileStepNameBytes), result, nil)
	if failure.StepResult == nil || !failure.StepResult.Truncated || (!failure.StepResult.StdoutTruncated && !failure.StepResult.StderrTruncated) {
		t.Fatal("oversized failure output was not reduced and marked truncated")
	}
	errorFrame := profileFailureMessage(progress.ExecutionID, progress, failure)
	if size := proto.Size(errorFrame); size > wire.MaxMessage {
		t.Fatal("reduced failure error exceeds protocol frame", size)
	}
	statusProgress := progress
	statusProgress.StepResult = nil
	statusProgress.Failure = nil
	status := api.ProfileStatus{ExecutionID: progress.ExecutionID, Kind: "environment", State: "failed", Progress: statusProgress, Failure: failure}
	statusFrame := &pb.Message{Kind: "result", RequestId: progress.ExecutionID, Payload: api.Payload(status)}
	if size := proto.Size(statusFrame); size > wire.MaxMessage {
		t.Fatal("reduced failure status exceeds protocol frame", size)
	}
}
