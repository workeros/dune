// Package workbench describes saved workspaces and personal views. References
// identify selected execution instances; saving them never grants execution access.
package workbench

import (
	"fmt"
	"path"
	"strings"
	"time"
	"unicode"

	"github.com/aiomni/dune/pkg/profiles"
	"github.com/aiomni/dune/pkg/runner"
)

type Project struct {
	ID        string    `json:"id"`
	OwnerID   string    `json:"owner_id"`
	Revision  int64     `json:"revision"`
	CreatedAt time.Time `json:"created_at"`
	UpdatedAt time.Time `json:"updated_at"`
	ProjectSpec
}

type ProjectSpec struct {
	Name           string              `json:"name"`
	DefaultProfile *profiles.Selection `json:"default_profile,omitempty"`
	Directories    []Directory         `json:"directories"`
}

// Directory pins a checkout to a Runner binding. Replacing that Runner does not
// silently turn an old path into a reference to a different filesystem.
type Directory struct {
	ID         string         `json:"id"`
	Binding    runner.Binding `json:"binding"`
	Path       string         `json:"path"`
	Repository string         `json:"repository,omitempty"`
}

func validText(value string, limit int) bool {
	return strings.TrimSpace(value) != "" && len(value) <= limit && !strings.ContainsFunc(value, unicode.IsControl)
}

func validPath(value string) bool {
	return validText(value, 4096) && path.IsAbs(value) && path.Clean(value) == value
}

func (p ProjectSpec) Validate() error {
	if !validText(p.Name, 120) || strings.TrimSpace(p.Name) != p.Name {
		return fmt.Errorf("project name must be 1..120 bytes without surrounding whitespace or control characters")
	}
	if p.DefaultProfile != nil && (!validText(p.DefaultProfile.ID, 256) || p.DefaultProfile.Revision < 1) {
		return fmt.Errorf("default Profile requires an ID and a positive revision")
	}
	if len(p.Directories) > 32 {
		return fmt.Errorf("a project can reference at most 32 directories")
	}
	seen := make(map[string]bool, len(p.Directories))
	for _, directory := range p.Directories {
		if !validText(directory.ID, 128) || seen[directory.ID] {
			return fmt.Errorf("directory IDs must be unique and 1..128 bytes")
		}
		seen[directory.ID] = true
		b := directory.Binding
		if !b.Valid() || !validText(b.RunnerID, 256) || !validText(b.FabricID, 256) || !validText(b.MachineID, 256) {
			return fmt.Errorf("directory requires a complete Runner binding")
		}
		if !validPath(directory.Path) || (directory.Repository != "" && !validPath(directory.Repository)) {
			return fmt.Errorf("directory and repository paths must be clean absolute paths")
		}
	}
	return nil
}
