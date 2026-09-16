package fabric

import (
	"path"
	"strings"
)

// ConnectorStartPlan starts the connector from an existing bootstrap
// installation. Script needs no enrollment token and must be run on the
// original resource after its persistent disk is available.
type ConnectorStartPlan struct {
	Script string
	Root   string
}

// NewConnectorStartPlan builds a remote command for the original Bootstrap
// Action ID. A successful command means fabricd is running, not that Gateway
// has observed it online. The script supports Linux and macOS.
func NewConnectorStartPlan(bootstrapActionID string) (ConnectorStartPlan, error) {
	completion, err := BootstrapCompletionPath(bootstrapActionID)
	if err != nil {
		return ConnectorStartPlan{}, err
	}
	return newConnectorStartPlan(path.Dir(completion), bootstrapActionID), nil
}

func newConnectorStartPlan(root, actionID string) ConnectorStartPlan {
	// The outer shell selects the platform lock command. The lock covers both
	// process discovery and launch, so concurrent calls cannot launch twice.
	// Keep the lock file: removing it would allow waiters to lock different
	// inodes. Linux flock -o keeps its descriptor out of fabricd.
	outer := []string{
		"set -eu",
		"umask 077",
		"root=" + shellQuote(root),
		"action_id=" + shellQuote(actionID),
		"if [ ! -d \"$root\" ]; then echo \"connector installation missing: $root\" >&2; exit 1; fi",
		"case \"$(uname -s)\" in",
		"  Linux) command -v flock >/dev/null 2>&1 || { echo 'connector start requires flock on Linux' >&2; exit 1; }; lock=flock; lock_args='-x -o' ;;",
		"  Darwin) command -v lockf >/dev/null 2>&1 || { echo 'connector start requires lockf on macOS' >&2; exit 1; }; lock=lockf; lock_args=-k ;;",
		"  *) echo 'unsupported connector start platform' >&2; exit 1 ;;",
		"esac",
	}
	inner := []string{
		"set -eu",
		"umask 077",
		"root=" + shellQuote(root),
		"action_id=" + shellQuote(actionID),
		"log=$root/fabricd.log",
		"fail() { echo \"$1; fabricd log: $log\" >&2; exit 1; }",
		"[ -f \"$root/bootstrap.complete\" ] || fail 'connector bootstrap marker missing'",
		"[ -f \"$root/config.yaml\" ] || fail 'connector config missing'",
		"[ -x \"$root/dune\" ] || fail 'connector executable missing or not executable'",
		"marker_id=$(sed -n '1p' \"$root/bootstrap.complete\")",
		"marker_version=$(sed -n '2p' \"$root/bootstrap.complete\")",
		"[ -n \"$marker_id\" ] || fail 'connector bootstrap marker empty'",
		"[ \"$marker_id\" = \"$action_id\" ] || fail 'connector bootstrap identity mismatch'",
		"[ -n \"$marker_version\" ] || fail 'connector bootstrap marker has no version'",
		"expected=\"$root/dune --config $root/config.yaml fabricd\"",
		"if LC_ALL=C ps -ww -e -o command= | grep -F -x -q -- \"$expected\"; then exit 0; fi",
		"nohup \"$root/dune\" --config \"$root/config.yaml\" fabricd >\"$log\" 2>&1 </dev/null &",
		"pid=$!",
		"sleep 1",
		"kill -0 \"$pid\" 2>/dev/null || fail 'connector process exited during startup'",
		"LC_ALL=C ps -ww -p \"$pid\" -o command= | grep -F -x -q -- \"$expected\" || fail 'connector process identity changed during startup'",
	}
	outer = append(outer, "$lock $lock_args \"$root/connector-start.lock\" /bin/sh -c "+shellQuote(strings.Join(inner, "\n")))
	return ConnectorStartPlan{Script: strings.Join(outer, "\n") + "\n", Root: root}
}
