package api

import "time"

// UpgradeReport is a read-only observation made by the proposed connector
// executable. A preview is not permission to switch later: the installer must
// acquire the launch gate and repeat the check before stopping fabricd.
type UpgradeReport struct {
	TargetBuild BuildInfo               `json:"target_build"`
	CheckedAt   time.Time               `json:"checked_at"`
	Allowed     bool                    `json:"allowed"`
	ProtocolMin int                     `json:"protocol_min"`
	ProtocolMax int                     `json:"protocol_max"`
	Hosts       []UpgradeHost           `json:"hosts"`
	Issues      []RuntimeDiscoveryIssue `json:"issues"`
}

type UpgradeHost struct {
	Runtime       Runtime           `json:"runtime"`
	Target        *SubmissionTarget `json:"target,omitempty"`
	Instance      string            `json:"instance,omitempty"`
	Protocol      int               `json:"protocol"`
	ProgramSHA256 string            `json:"program_sha256,omitempty"`
	Phase         string            `json:"phase"`
}
