package fabric

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"path"
	"strings"

	"github.com/aiomni/dune/pkg/deployment"
)

// BootstrapPlatform selects the Dune archive installed into a provider
// resource. Providers must obtain these facts from the resource itself rather
// than assuming the host process platform.
type BootstrapPlatform struct {
	OS   string
	Arch string
}

func (p BootstrapPlatform) Valid() bool {
	return (p.OS == "linux" || p.OS == "darwin") && (p.Arch == "amd64" || p.Arch == "arm64")
}

// BootstrapPlan contains Dune-owned connector installation knowledge while
// leaving remote command execution and file inspection to the Provider. Script
// includes the one-shot enrollment token and must not be logged or persisted.
type BootstrapPlan struct {
	Script         string
	Root           string
	CompletionPath string
	Platform       BootstrapPlatform
}

const bootstrapRoot = "/tmp/dune-managed"

// BootstrapCompletionPath returns the deterministic marker inspected by a
// Provider's read-only ReconcileBootstrap implementation.
func BootstrapCompletionPath(actionID string) (string, error) {
	if !boundedIdentifier(actionID, 128) {
		return "", fmt.Errorf("invalid bootstrap action ID")
	}
	sum := sha256.Sum256([]byte(actionID))
	return path.Join(bootstrapRoot, hex.EncodeToString(sum[:]), "bootstrap.complete"), nil
}

// NewBootstrapPlan builds the provider-independent download, extraction,
// enrollment and fabricd launch script for one previously reserved action.
// Executing the plan is a mutation and must occur only for the original
// Bootstrap call; recovery uses CompletionPath and never re-executes Script.
func NewBootstrapPlan(call BootstrapCall, platform BootstrapPlatform) (BootstrapPlan, error) {
	if !platform.Valid() {
		return BootstrapPlan{}, fmt.Errorf("unsupported bootstrap platform")
	}
	if !boundedIdentifier(call.Action.ID, 128) || !boundedIdentifier(call.Action.RunnerID, 128) || strings.TrimSpace(call.Action.ResourceRef) == "" {
		return BootstrapPlan{}, fmt.Errorf("bootstrap requires action and resource identities")
	}
	if len(call.EnrollmentToken) != 64 {
		return BootstrapPlan{}, fmt.Errorf("bootstrap requires a one-shot enrollment token")
	}
	if _, err := hex.DecodeString(call.EnrollmentToken); err != nil || strings.ToLower(call.EnrollmentToken) != call.EnrollmentToken {
		return BootstrapPlan{}, fmt.Errorf("bootstrap requires a one-shot enrollment token")
	}
	if !boundedIdentifier(call.Version, 128) {
		return BootstrapPlan{}, fmt.Errorf("bootstrap requires a bounded Dune version")
	}
	urls, err := deployment.NewURLs(call.Endpoint, call.GatewayURL)
	if err != nil {
		return BootstrapPlan{}, fmt.Errorf("invalid bootstrap endpoint: %w", err)
	}
	completion, err := BootstrapCompletionPath(call.Action.ID)
	if err != nil {
		return BootstrapPlan{}, err
	}
	root := path.Dir(completion)
	archiveURL := urls.PublicURL + "api/v1/downloads/dune-" + platform.OS + "-" + platform.Arch + ".tar.gz"
	lines := []string{
		"set -eu",
		"umask 077",
		"root=" + shellQuote(root),
		"mkdir -p \"$root\"",
		"chmod 700 \"$root\"",
		"if [ ! -f \"$root/bootstrap.complete\" ]; then",
		"if [ -e \"$root/config.yaml\" ] || [ -L \"$root/config.yaml\" ]; then echo 'configuration already exists; inspect this bootstrap before retrying' >&2; exit 1; fi",
		"curl --fail --silent --show-error --proto '=http,https' --proto-redir '=http,https' --connect-timeout 10 --max-time 90 " + shellQuote(archiveURL) + " -o \"$root/dune.tar.gz\"",
		"curl --fail --silent --show-error --proto '=http,https' --connect-timeout 10 --max-time 90 " + shellQuote(archiveURL+".sha256") + " -o \"$root/dune.sha256\"",
		"expected=$(cut -d ' ' -f 1 \"$root/dune.sha256\"); case \"$expected\" in *[!0-9a-f]*|'') exit 1;; esac; [ ${#expected} -eq 64 ]",
		"if command -v sha256sum >/dev/null 2>&1; then actual=$(sha256sum \"$root/dune.tar.gz\"); else actual=$(shasum -a 256 \"$root/dune.tar.gz\"); fi; [ \"${actual%% *}\" = \"$expected\" ]",
		"tar -xzf \"$root/dune.tar.gz\" -C \"$root\"",
		"chmod 700 \"$root/dune\" \"$root/tmux\" \"$root/rg\"",
		"\"$root/dune\" --config \"$root/config.yaml\" enroll --site " + shellQuote(urls.PublicURL) + " --token " + shellQuote(call.EnrollmentToken) + " --runner-id " + shellQuote(call.Action.RunnerID),
		"nohup \"$root/dune\" --config \"$root/config.yaml\" fabricd >\"$root/fabricd.log\" 2>&1 </dev/null &",
		"pid=$!",
		"sleep 1",
		"kill -0 \"$pid\"",
		"printf '%s\\n' " + shellQuote(call.Action.ID) + " " + shellQuote(call.Version) + " >\"$root/bootstrap.complete.tmp\"",
		"mv \"$root/bootstrap.complete.tmp\" \"$root/bootstrap.complete\"",
		"fi",
	}
	return BootstrapPlan{Script: strings.Join(lines, "\n"), Root: root, CompletionPath: completion, Platform: platform}, nil
}

func shellQuote(value string) string {
	return "'" + strings.ReplaceAll(value, "'", "'\"'\"'") + "'"
}
