// Package runner describes logical execution environments above the Dune
// protocol. A binding is a snapshot, not a request to follow future replacements.
package runner

import "errors"

var ErrBindingChanged = errors.New("runner binding changed; resolve and review the current environment")

// Binding fixes the concrete environment selected for an access request.
// Revision is independent of Gateway routing epochs and Runtime generations.
type Binding struct {
	RunnerID  string `json:"runner_id"`
	FabricID  string `json:"fabric_id"`
	MachineID string `json:"machine_id"`
	Revision  int64  `json:"revision"`
}

func (b Binding) Valid() bool {
	return b.RunnerID != "" && b.FabricID != "" && b.MachineID != "" && b.Revision > 0
}

// Runner is the product identity. Runtime IDs continue to identify sessions on
// its concrete machine. A missing binding does not authorize any execution.
type Runner struct {
	ID        string   `json:"id"`
	Name      string   `json:"name"`
	Kind      string   `json:"kind"`
	CreatedAt int64    `json:"created_at"`
	Binding   *Binding `json:"binding,omitempty"`
}
