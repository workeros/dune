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
