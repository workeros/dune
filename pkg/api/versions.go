package api

import "time"

// BuildInfo is embedded in the running executable. It never reads the installed
// current symlink or assumes that replacing a file updated an existing process.
type BuildInfo struct {
	Version   string `json:"version"`
	Revision  string `json:"revision,omitempty"`
	Modified  bool   `json:"modified"`
	GoVersion string `json:"go_version"`
	OS        string `json:"os"`
	Arch      string `json:"arch"`
}

type ConnectorInfo struct {
	Build       BuildInfo `json:"build"`
	PID         int       `json:"pid"`
	StartedAt   time.Time `json:"started_at"`
	Incarnation string    `json:"incarnation"`
	ProtocolMin int       `json:"acp_host_protocol_min"`
	ProtocolMax int       `json:"acp_host_protocol_max"`
}

// TmuxVersion explicitly separates the executable used for a new client from
// the version reported by the existing server. Only server_state=running carries
// a confirmed current server PID/version. These PIDs are diagnostic only.
type TmuxVersion struct {
	Namespace     string    `json:"namespace"`
	CheckedAt     time.Time `json:"checked_at"`
	ClientVersion string    `json:"client_version,omitempty"`
	ServerVersion string    `json:"server_version,omitempty"`
	ServerPID     int       `json:"server_pid,omitempty"`
	ServerState   string    `json:"server_state"`
	ErrorCode     string    `json:"error_code,omitempty"`
}
