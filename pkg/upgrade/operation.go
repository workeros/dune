package upgrade

import (
	"fmt"
	"strconv"
	"time"

	"github.com/aiomni/dune/pkg/api"
	"github.com/aiomni/dune/pkg/runner"
)

type Request struct {
	SubmissionID                 string         `json:"submission_id"`
	Binding                      runner.Binding `json:"binding"`
	InstallationID               string         `json:"installation_id"`
	ExpectedInstallationRevision string         `json:"expected_installation_revision"`
	ExpectedRunningSHA256        string         `json:"expected_running_sha256"`
	Release                      ReleaseRef     `json:"release"`
}

func (r Request) Validate() error {
	if api.ValidateSubmissionID(r.SubmissionID) != nil || !r.Binding.Valid() || api.ValidateSubmissionID(r.InstallationID) != nil || !ValidSHA256(r.ExpectedRunningSHA256) || !ValidSHA256(r.Release.ManifestSHA256) || r.Release.ID == "" || len(r.Release.ID) > 128 {
		return fmt.Errorf("complete original upgrade identity and release reference required")
	}
	revision, err := strconv.ParseUint(r.ExpectedInstallationRevision, 10, 64)
	if err != nil || revision == 0 || strconv.FormatUint(revision, 10) != r.ExpectedInstallationRevision {
		return fmt.Errorf("canonical positive installation revision required")
	}
	return nil
}

type Phase string

const (
	Queued            Phase = "queued"
	Preparing         Phase = "preparing"
	Downloading       Phase = "downloading"
	Checking          Phase = "checking"
	Switching         Phase = "switching"
	Reconnecting      Phase = "reconnecting"
	Verifying         Phase = "verifying"
	Succeeded         Phase = "succeeded"
	AlreadyCurrent    Phase = "already_current"
	Failed            Phase = "failed"
	RollingBack       Phase = "rolling_back"
	RollbackVerifying Phase = "rollback_verifying"
	RecoveryBlocked   Phase = "recovery_blocked"
)

type RollbackState string

const (
	RollbackNotNeeded   RollbackState = "not_needed"
	RollbackRunning     RollbackState = "running"
	RollbackRestored    RollbackState = "restored"
	RollbackFailed      RollbackState = "failed"
	RollbackBlocked     RollbackState = "blocked"
	RollbackUnconfirmed RollbackState = "unconfirmed"
)

// Proof is returned only after a normal host → Gateway → Runner round trip.
// Worker validation includes its fresh challenge; another attempt cannot reuse
// an otherwise matching process or historical successful distribution.
type Proof struct {
	OperationID                  string             `json:"operation_id"`
	AttemptID                    string             `json:"attempt_id"`
	Challenge                    string             `json:"challenge"`
	Binding                      runner.Binding     `json:"binding"`
	InstallationID               string             `json:"installation_id"`
	InstallationRevision         string             `json:"installation_revision"`
	ManifestSHA256               string             `json:"manifest_sha256"`
	Running                      api.RunningProgram `json:"running"`
	Incarnation                  string             `json:"incarnation"`
	ConnectionGeneration         uint64             `json:"connection_generation"`
	RouteEpoch                   uint64             `json:"route_epoch"`
	OriginalInstallationRestored bool               `json:"original_installation_restored"`
	ReleaseVerified              bool               `json:"release_verified"`
	GatewayAccepted              bool               `json:"gateway_accepted"`
	Routed                       bool               `json:"routed"`
	ObservedAt                   time.Time          `json:"observed_at"`
}

type Operation struct {
	ID                string                  `json:"id"`
	Request           Request                 `json:"request"`
	Revision          string                  `json:"revision"`
	Admission         api.SubmissionAdmission `json:"admission"`
	Phase             Phase                   `json:"phase"`
	Confirmed         bool                    `json:"confirmed"`
	Rollback          RollbackState           `json:"rollback"`
	Source            Inspection              `json:"source"`
	Target            Manifest                `json:"target"`
	Plan              Plan                    `json:"plan"`
	Participants      []api.UpgradeHost       `json:"participants"`
	StateContract     string                  `json:"state_contract"`
	LaunchSealed      bool                    `json:"launch_sealed"`
	AttemptID         string                  `json:"attempt_id,omitempty"`
	Challenge         string                  `json:"challenge,omitempty"`
	AttemptStartedAt  time.Time               `json:"attempt_started_at,omitempty"`
	StartedAt         time.Time               `json:"started_at"`
	UpdatedAt         time.Time               `json:"updated_at"`
	Failure           *Issue                  `json:"failure,omitempty"`
	RollbackFailure   *Issue                  `json:"rollback_failure,omitempty"`
	ActiveOperationID string                  `json:"active_operation_id,omitempty"`
	Proof             *Proof                  `json:"proof,omitempty"`
}

// Observation describes freshness independently of execution. Offline snapshots
// preserve the executor's last facts and never infer a new terminal result.
type Observation struct {
	Operation  Operation `json:"operation"`
	Freshness  string    `json:"freshness"`
	ObservedAt time.Time `json:"observed_at"`
}

type Query struct {
	Binding        runner.Binding `json:"binding"`
	InstallationID string         `json:"installation_id"`
	SubmissionID   string         `json:"submission_id,omitempty"`
	OperationID    string         `json:"operation_id,omitempty"`
}

type Page struct {
	Active     *Operation  `json:"active,omitempty"`
	Items      []Operation `json:"items"`
	NextCursor string      `json:"next_cursor,omitempty"`
}
