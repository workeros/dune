package upgrade

import "github.com/aiomni/dune/pkg/runner"

// Probe selects the current durable attempt. It only observes an installation;
// it cannot start work, advance a worker, choose a new version or create a session.
type Probe struct {
	Binding        runner.Binding `json:"binding"`
	InstallationID string         `json:"installation_id"`
	OperationID    string         `json:"operation_id"`
	AttemptID      string         `json:"attempt_id"`
	Challenge      string         `json:"challenge"`
}

func (o Operation) Probe() Probe {
	return Probe{Binding: o.Request.Binding, InstallationID: o.Request.InstallationID, OperationID: o.ID, AttemptID: o.AttemptID, Challenge: o.Challenge}
}

// Same-image updates cannot identify a restart by program digest. Startup must
// have observed this attempt and the selected dependency directory together.
func (o Operation) RequiresStartupEvidence() bool {
	return o.Plan.ConnectorRestartRequired && o.Source.Running.SHA256 == o.Target.ProgramSHA256()
}
