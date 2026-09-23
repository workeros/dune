package fabric

import (
	"path"
	"strings"
)

// ConnectorStartPlan restores the two registered service jobs in the original
// bootstrap installation. It neither enrolls nor changes the selected release.
type ConnectorStartPlan struct {
	Script string
	Root   string
}

// NewConnectorStartPlan uses the original Bootstrap Action ID. Completion only
// confirms service repair; the provider must separately observe Gateway routing.
func NewConnectorStartPlan(bootstrapActionID string) (ConnectorStartPlan, error) {
	completion, err := BootstrapCompletionPath(bootstrapActionID)
	if err != nil {
		return ConnectorStartPlan{}, err
	}
	return newConnectorStartPlan(path.Dir(completion), bootstrapActionID), nil
}

func newConnectorStartPlan(root, actionID string) ConnectorStartPlan {
	lines := []string{
		"set -eu",
		"umask 077",
		"root=" + shellQuote(root),
		"action_id=" + shellQuote(actionID),
		"[ -d \"$root\" ] || { echo 'connector installation missing' >&2; exit 1; }",
		"[ -f \"$root/bootstrap.complete\" ] || { echo 'connector bootstrap marker missing' >&2; exit 1; }",
		"marker_id=$(sed -n '1p' \"$root/bootstrap.complete\")",
		"marker_version=$(sed -n '2p' \"$root/bootstrap.complete\")",
		"[ \"$marker_id\" = \"$action_id\" ] && [ -n \"$marker_version\" ] || { echo 'connector bootstrap identity mismatch' >&2; exit 1; }",
		"exec \"$root/installation/recovery/dune\" repair-services --root \"$root/installation\"",
	}
	return ConnectorStartPlan{Script: strings.Join(lines, "\n") + "\n", Root: root}
}
