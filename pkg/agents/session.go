// Package agents describes application-managed Agent launches and recovery.
// Execution and operation queues remain owned by fabricd.
package agents

import (
	"encoding/json"
	"fmt"
	"path"
	"strings"
	"time"
	"unicode"

	"github.com/aiomni/dune/pkg/api"
	"github.com/aiomni/dune/pkg/profiles"
	"github.com/aiomni/dune/pkg/runner"
	"github.com/aiomni/dune/pkg/workbench"
)

// LaunchSnapshot is written before starting a process and never edited. Profile
// is the actual configuration after overrides, before ephemeral MCP injection.
// It is private storage data: never include it in discovery or MCP responses.
type LaunchSnapshot struct {
	Profile       api.Profile         `json:"profile"`
	SourceProfile *profiles.Selection `json:"source_profile,omitempty"`
	Binding       runner.Binding      `json:"binding"`
	ProjectID     string              `json:"project_id,omitempty"`
	DirectoryID   string              `json:"directory_id,omitempty"`
	AgentType     string              `json:"agent_type"`
	Recovery      RecoveryAdapter     `json:"recovery"`
	// Storage identifies the execution user/native session storage on this
	// concrete Runner. A replacement binding never inherits this identity.
	Storage string `json:"storage"`
}

type RecoveryAdapter struct {
	ID      string `json:"id"`
	Version int    `json:"version"`
}

// NativeSession is a confirmed observation, not a requested load argument.
// ID and storage stay immutable once captured. ResumeSupported is independent
// of history listing support and may be false even when an ID was observed.
type NativeSession struct {
	ID              string `json:"id"`
	Cwd             string `json:"cwd"`
	AgentVersion    string `json:"agent_version,omitempty"`
	Source          string `json:"source"`
	ResumeSupported bool   `json:"resume_supported"`
	Reason          string `json:"reason,omitempty"`
}

type Session struct {
	ID        string    `json:"id"`
	OwnerID   string    `json:"owner_id"`
	Revision  int64     `json:"revision"`
	CreatedAt time.Time `json:"created_at"`
	UpdatedAt time.Time `json:"updated_at"`
	// Launch is deliberately omitted even when this type is accidentally
	// serialized. APIs construct a summary without commands or environment.
	Launch LaunchSnapshot `json:"-"`
	// Selected means the database still associates this record with its last
	// Runtime's selected native session. It does not assert process liveness.
	Selected bool `json:"-"`
	SessionState
}

type SessionState struct {
	Status      string                `json:"status"` // pending_capture, available, unavailable, unknown
	Reason      string                `json:"reason,omitempty"`
	Native      *NativeSession        `json:"native,omitempty"`
	ConfirmedAt *time.Time            `json:"confirmed_at,omitempty"`
	LastRuntime *workbench.RuntimeRef `json:"last_runtime,omitempty"`
	Attempt     LaunchAttempt         `json:"attempt"`
}

// Only the latest launch attempt is retained. This is deduplication metadata,
// not a persistent job queue. Unknown attempts have no automatic takeover.
type LaunchAttempt struct {
	ID           string                `json:"id"`
	Kind         string                `json:"kind"` // start or resume
	BaseRevision int64                 `json:"base_revision"`
	State        string                `json:"state"` // starting, capturing, ready, failed, unknown
	Runtime      *workbench.RuntimeRef `json:"runtime,omitempty"`
	StartedAt    time.Time             `json:"started_at"`
}

func validText(value string, limit int) bool {
	return strings.TrimSpace(value) != "" && len(value) <= limit && !strings.ContainsFunc(value, unicode.IsControl)
}

func (s LaunchSnapshot) Validate() error {
	if err := s.Profile.Validate(); err != nil {
		return err
	}
	if s.Profile.Kind != "agent" || !s.Binding.Valid() || !validText(s.AgentType, 128) || !validText(s.Storage, 4096) {
		return fmt.Errorf("Agent launch requires an Agent Profile, Runner binding, Agent type and native storage identity")
	}
	for _, value := range []string{s.Binding.RunnerID, s.Binding.FabricID, s.Binding.MachineID} {
		if !validText(value, 256) {
			return fmt.Errorf("invalid Runner binding")
		}
	}
	for _, value := range []string{s.ProjectID, s.DirectoryID} {
		if value != "" && !validText(value, 256) {
			return fmt.Errorf("invalid project directory reference")
		}
	}
	if s.DirectoryID != "" && s.ProjectID == "" {
		return fmt.Errorf("a saved directory requires a project")
	}
	if p := s.SourceProfile; p != nil && (!validText(p.ID, 256) || p.Revision < 1) {
		return fmt.Errorf("Profile provenance requires a fixed revision")
	}
	if (s.Recovery.ID == "" && s.Recovery.Version != 0) || (s.Recovery.ID != "" && (!validText(s.Recovery.ID, 128) || s.Recovery.Version < 1)) {
		return fmt.Errorf("recovery adapter requires an ID and positive format version")
	}
	payload, err := json.Marshal(s)
	if err != nil || len(payload) > api.MaxProfileBytes {
		return fmt.Errorf("launch snapshot exceeds the storage limit")
	}
	return nil
}

func (s NativeSession) Validate() error {
	if !validText(s.ID, 4096) || !path.IsAbs(s.Cwd) || !validText(s.Cwd, 4096) || !validText(s.Source, 128) || len(s.AgentVersion) > 256 || len(s.Reason) > 4096 {
		return fmt.Errorf("native session requires a confirmed ID and bounded capture metadata")
	}
	if !s.ResumeSupported && !validText(s.Reason, 4096) {
		return fmt.Errorf("unsupported recovery requires a reason")
	}
	return nil
}
