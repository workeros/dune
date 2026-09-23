package api

import "time"

// ACPHostInfo is diagnostic evidence from the original host. PID values never
// grant execution authority. Startup contains only bounded, redacted evidence.
type ACPHostInfo struct {
	StateContract  string                `json:"state_contract"`
	Startup        *ACPStartupDiagnostic `json:"startup,omitempty"`
	Build          *BuildInfo            `json:"build,omitempty"`
	LifecycleLog   *LifecycleLogUsage    `json:"lifecycle_log,omitempty"`
	Protocol       int                   `json:"protocol"`
	Instance       string                `json:"instance,omitempty"`
	ProgramSHA256  string                `json:"program_sha256,omitempty"`
	ProgramBytes   int64                 `json:"program_bytes,omitempty"`
	HostPID        int                   `json:"host_pid,omitempty"`
	AgentPID       int                   `json:"agent_pid,omitempty"`
	GroupID        int                   `json:"group_id,omitempty"`
	StartedAt      *time.Time            `json:"started_at,omitempty"`
	Connected      bool                  `json:"connected"`
	ConnectorTerm  uint64                `json:"connector_term,omitempty"`
	LastAttachedAt *time.Time            `json:"last_attached_at,omitempty"`
}

// ACPStartupDiagnostic never includes argv, environment, protocol bytes or raw errors.
// ConfirmationTimeout is an observation, not a failure or permission to retry.
type ACPStartupDiagnostic struct {
	Phase               string `json:"phase"`
	Code                string `json:"code,omitempty"`
	ConfirmationTimeout bool   `json:"confirmation_timeout,omitempty"`
}
