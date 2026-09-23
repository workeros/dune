package upgrade

import (
	"context"
	"fmt"
	"github.com/aiomni/dune/pkg/api"
	"github.com/aiomni/dune/pkg/identity"
	"github.com/aiomni/dune/pkg/runner"
	"time"
)

// Scope is checked against current host permissions on every operation, even
// when an offline observation is available. Operation IDs are only selectors.
type Scope struct {
	Principal identity.User
	OwnerID   string
	Binding   runner.Binding
}

type PreviewRequest struct {
	Binding runner.Binding `json:"binding"`
	Release ReleaseRef     `json:"release"`
}
type Preview struct {
	Source      Inspection        `json:"source"`
	Target      Manifest          `json:"target"`
	Plan        Plan              `json:"plan"`
	SourceCheck api.UpgradeReport `json:"source_check"`
	TargetCheck api.UpgradeReport `json:"target_check"`
	Allowed     bool              `json:"allowed"`
	Issues      []Issue           `json:"issues"`
}
type ListRequest struct {
	Binding        runner.Binding `json:"binding"`
	InstallationID string         `json:"installation_id"`
	Cursor         string         `json:"cursor,omitempty"`
	Limit          int            `json:"limit,omitempty"`
}

type Service interface {
	InspectRunner(context.Context, Scope) (Inspection, error)
	PreviewUpgrade(context.Context, Scope, PreviewRequest) (Preview, error)
	StartUpgrade(context.Context, Scope, Request) (Observation, error)
	GetUpgrade(context.Context, Scope, Query) (Observation, error)
	ListUpgrades(context.Context, Scope, ListRequest) (History, error)
}

type History struct {
	ObservationIssue *Issue    `json:"observation_issue,omitempty"`
	Page             Page      `json:"page"`
	Freshness        string    `json:"freshness"`
	ObservedAt       time.Time `json:"observed_at"`
}

func (q Query) Validate() error {
	if !q.Binding.Valid() || api.ValidateSubmissionID(q.InstallationID) != nil || (q.SubmissionID == "") == (q.OperationID == "") {
		return fmt.Errorf("exactly one original upgrade selector required")
	}
	value := q.SubmissionID
	if value == "" {
		value = q.OperationID
	}
	return api.ValidateSubmissionID(value)
}
func (r ListRequest) Validate() error {
	if !r.Binding.Valid() || api.ValidateSubmissionID(r.InstallationID) != nil || r.Limit < 0 || r.Limit > 100 {
		return fmt.Errorf("bounded original upgrade history scope required")
	}
	if r.Cursor != "" {
		if _, err := parseCursor(r.Cursor); err != nil {
			return err
		}
	}
	return nil
}
