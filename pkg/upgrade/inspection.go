package upgrade

import (
	"github.com/aiomni/dune/pkg/api"
	"github.com/aiomni/dune/pkg/runner"
	"slices"
	"time"
)

type ComponentObservation struct {
	Path    string `json:"path"`
	Present bool   `json:"present"`
	SHA256  string `json:"sha256,omitempty"`
	Bytes   int64  `json:"bytes"`
	Mode    uint32 `json:"mode"`
	Matches bool   `json:"matches"`
}

// InstallationRevision is a decimal counter independent of operation progress.
// A rollback never restores an earlier revision or an old shared-state snapshot.
type Installation struct {
	ID         string                 `json:"id"`
	Revision   string                 `json:"revision"`
	Method     string                 `json:"method"`
	Release    Manifest               `json:"release"`
	Components []ComponentObservation `json:"components"`
	Complete   bool                   `json:"complete"`
	ObservedAt time.Time              `json:"observed_at"`
}

type Inspection struct {
	StartedForUpgrade *Probe             `json:"started_for_upgrade,omitempty"`
	Binding           runner.Binding     `json:"binding"`
	Installation      *Installation      `json:"installation,omitempty"`
	Running           api.RunningProgram `json:"running"`
	Supported         bool               `json:"supported"`
	Issues            []Issue            `json:"issues"`
}

type Issue struct {
	Code  string `json:"code"`
	Stage string `json:"stage,omitempty"`
}

type Plan struct {
	ReleaseUpdateRequired    bool     `json:"release_update_required"`
	ConnectorRestartRequired bool     `json:"connector_restart_required"`
	RestartReason            string   `json:"restart_reason,omitempty"`
	Differences              []string `json:"differences"`
}

// Compare considers the actual source observation. Helper programs are resolved
// at connector startup, so changing any program or its active directory requires
// restart. Existing tmux servers and retained session hosts are never restarted.
func Compare(source Installation, running api.RunningProgram, target Manifest) Plan {
	plan := Plan{Differences: []string{}}
	observations := make(map[string]ComponentObservation, len(source.Components))
	for _, c := range source.Components {
		observations[c.Path] = c
	}
	for _, c := range target.Components {
		actual, exists := observations[c.Path]
		if !exists || !actual.Present || actual.SHA256 != c.SHA256 || actual.Bytes != c.Bytes || actual.Mode != c.Mode {
			plan.Differences = append(plan.Differences, c.Path)
		}
		delete(observations, c.Path)
	}
	for path := range observations {
		plan.Differences = append(plan.Differences, path)
	}
	slices.Sort(plan.Differences)
	plan.ReleaseUpdateRequired = len(plan.Differences) != 0 || !source.Complete
	plan.ConnectorRestartRequired = running.SHA256 != target.ProgramSHA256()
	if plan.ConnectorRestartRequired {
		plan.RestartReason = "CONNECTOR_PROGRAM_CHANGED"
	}
	// Atomic directory switching also changes the paths used to launch rg/tmux.
	// Reuse the connector only when no installation switch is needed.
	if plan.ReleaseUpdateRequired && !plan.ConnectorRestartRequired {
		plan.ConnectorRestartRequired = true
		plan.RestartReason = "DEPENDENCY_DIRECTORY_CHANGED"
	}
	return plan
}
