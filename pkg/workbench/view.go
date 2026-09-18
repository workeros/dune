package workbench

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"math"
	"time"

	"github.com/aiomni/dune/pkg/runner"
)

// AgentTarget is a saved selection, never an authorization grant. Runtime
// liveness and the current binding must be checked before attaching or sending.
type AgentTarget struct {
	Binding runner.Binding `json:"binding"`
	Runtime RuntimeRef     `json:"runtime"`
}

type RuntimeRef struct {
	ID          string `json:"id"`
	Incarnation string `json:"incarnation"`
	Generation  uint64 `json:"generation"`
	Adapter     string `json:"adapter"`
}

func (t AgentTarget) Validate() error {
	if !t.Binding.Valid() || !validText(t.Binding.RunnerID, 256) || !validText(t.Binding.MachineID, 256) || !validText(t.Binding.FabricID, 256) ||
		!validText(t.Runtime.ID, 256) || !validText(t.Runtime.Incarnation, 256) || t.Runtime.Generation == 0 || (t.Runtime.Adapter != "pty" && t.Runtime.Adapter != "acp") {
		return fmt.Errorf("a complete Runner binding and Runtime identity are required")
	}
	return nil
}

func (t AgentTarget) Key() string {
	encoded, _ := json.Marshal([]any{t.Binding.RunnerID, t.Binding.FabricID, t.Binding.MachineID, t.Binding.Revision, t.Runtime.ID, t.Runtime.Incarnation, t.Runtime.Generation})
	digest := sha256.Sum256(encoded)
	return hex.EncodeToString(digest[:])
}

type View struct {
	ID        string    `json:"id"`
	Revision  int64     `json:"revision"`
	UpdatedAt time.Time `json:"updated_at"`
	ViewSpec
}

type ViewSpec struct {
	Root       *SplitNode `json:"root"`
	FocusPane  string     `json:"focus_pane,omitempty"`
	ReviewPane string     `json:"review_pane,omitempty"`
}

// SplitNode is either a pane or a binary split. View removal never stops its Agent.
type SplitNode struct {
	ID        string       `json:"id"`
	Pane      *Pane        `json:"pane,omitempty"`
	Direction string       `json:"direction,omitempty"`
	Ratio     float64      `json:"ratio,omitempty"`
	Children  []*SplitNode `json:"children,omitempty"`
}

type Pane struct {
	Target          AgentTarget `json:"target"`
	ProjectID       string      `json:"project_id,omitempty"`
	DirectoryID     string      `json:"directory_id,omitempty"`
	SessionRecordID string      `json:"session_record_id,omitempty"`
}

// Panes validates structural limits before returning the leaf selections. At
// most 32 panes are saved; duplicate execution targets must focus the existing pane.
func (v ViewSpec) Panes() ([]Pane, error) {
	panes := []Pane{}
	ids, leaves, targets := map[string]bool{}, map[string]bool{}, map[string]bool{}
	var visit func(*SplitNode, int) error
	visit = func(node *SplitNode, depth int) error {
		if node == nil || depth > 16 || !validText(node.ID, 128) || ids[node.ID] || len(ids) >= 63 {
			return fmt.Errorf("layout requires unique node IDs, at most 32 panes and depth 16")
		}
		ids[node.ID] = true
		if node.Pane != nil {
			if len(node.Children) != 0 || node.Direction != "" || node.Ratio != 0 {
				return fmt.Errorf("pane cannot also be a split")
			}
			pane := *node.Pane
			if err := pane.Target.Validate(); err != nil {
				return err
			}
			for _, value := range []string{pane.ProjectID, pane.DirectoryID, pane.SessionRecordID} {
				if value != "" && !validText(value, 256) {
					return fmt.Errorf("invalid pane reference")
				}
			}
			key := pane.Target.Key()
			if targets[key] {
				return fmt.Errorf("Runtime already has a pane; focus that pane instead")
			}
			targets[key], leaves[node.ID] = true, true
			panes = append(panes, pane)
			return nil
		}
		if len(node.Children) != 2 || (node.Direction != "horizontal" && node.Direction != "vertical") || math.IsNaN(node.Ratio) || node.Ratio < 0.1 || node.Ratio > 0.9 {
			return fmt.Errorf("split requires two children, horizontal or vertical direction and ratio 0.1..0.9")
		}
		for _, child := range node.Children {
			if err := visit(child, depth+1); err != nil {
				return err
			}
		}
		return nil
	}
	if v.Root != nil {
		if err := visit(v.Root, 1); err != nil {
			return nil, err
		}
	}
	if (v.FocusPane != "" && !leaves[v.FocusPane]) || (v.ReviewPane != "" && !leaves[v.ReviewPane]) {
		return nil, fmt.Errorf("focus and review must reference panes in this layout")
	}
	return panes, nil
}
