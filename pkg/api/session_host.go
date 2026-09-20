package api

import "time"

// ACPHostInfo is diagnostic evidence from the original host. PID values never
// grant execution authority. Before host registration only Protocol is known.
type ACPHostInfo struct {
	Build          *BuildInfo `json:"build,omitempty"`
	Protocol       int        `json:"protocol"`
	Instance       string     `json:"instance,omitempty"`
	ProgramSHA256  string     `json:"program_sha256,omitempty"`
	ProgramBytes   int64      `json:"program_bytes,omitempty"`
	HostPID        int        `json:"host_pid,omitempty"`
	AgentPID       int        `json:"agent_pid,omitempty"`
	GroupID        int        `json:"group_id,omitempty"`
	StartedAt      *time.Time `json:"started_at,omitempty"`
	Connected      bool       `json:"connected"`
	ConnectorTerm  uint64     `json:"connector_term,omitempty"`
	LastAttachedAt *time.Time `json:"last_attached_at,omitempty"`
}
