// Package runnerupgrade executes durable upgrade jobs independently of fabricd.
// It owns no sessions and never restores shared-state snapshots.
package runnerupgrade

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"github.com/aiomni/dune/internal/statecontract"
	"github.com/aiomni/dune/pkg/api"
	"github.com/aiomni/dune/pkg/upgrade"
	"io"
	"os/exec"
	"time"
)

// checkProgram executes only Dune's fixed read-only preflight command. Output
// and time are bounded; diagnostics never expose stderr or configuration bytes.
func checkProgram(ctx context.Context, executable, configPath string, expected upgrade.Manifest) (api.UpgradeReport, error) {
	ctx, cancel := context.WithTimeout(ctx, 20*time.Second)
	defer cancel()
	output := &boundedOutput{limit: 256 << 10}
	command := exec.CommandContext(ctx, executable, "--config", configPath, "upgrade-check")
	command.Stdout = output
	command.Stderr = io.Discard
	err := command.Run()
	var report api.UpgradeReport
	if output.exceeded || json.Unmarshal(output.Bytes(), &report) != nil {
		return report, issue("UPGRADE_CHECK_INVALID")
	}
	if err != nil || !report.Allowed {
		return report, issue("UPGRADE_CHECK_REJECTED")
	}
	if report.StateContract != statecontract.ID() || report.Program.SHA256 != expected.ProgramSHA256() || report.Program.Build.OS != expected.Platform.OS || report.Program.Build.Arch != expected.Platform.Arch {
		return report, issue("STATE_CONTRACT_UNSUPPORTED")
	}
	return report, nil
}

type boundedOutput struct {
	bytes.Buffer
	limit    int
	exceeded bool
}

func (b *boundedOutput) Write(p []byte) (int, error) {
	if len(p) > b.limit-b.Len() {
		b.exceeded = true
		return 0, fmt.Errorf("preflight output exceeds limit")
	}
	return b.Buffer.Write(p)
}
func issue(code string) error {
	return &api.Error{Code: code, Detail: "Runner upgrade could not complete this stage"}
}
