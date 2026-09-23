package fabricd

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"time"

	"github.com/aiomni/dune/internal/buildinfo"
	"github.com/aiomni/dune/internal/launchgate"
	"github.com/aiomni/dune/internal/retainedprogram"
	"github.com/aiomni/dune/internal/runningprogram"
	"github.com/aiomni/dune/internal/sessionregistry"
	"github.com/aiomni/dune/internal/statecontract"
	"github.com/aiomni/dune/pkg/api"
)

func upgradeReport() api.UpgradeReport {
	return api.UpgradeReport{StateContract: statecontract.ID(), TargetBuild: buildinfo.Current(), CheckedAt: time.Now().UTC(), ProtocolMin: sessionProtocol, ProtocolMax: sessionProtocol, Hosts: []api.UpgradeHost{}, Issues: []api.RuntimeDiscoveryIssue{}}
}

func upgradeIdentity(runtime api.Runtime) api.Runtime {
	return api.Runtime{ID: runtime.ID, Incarnation: runtime.Incarnation, Generation: runtime.Generation, Adapter: runtime.Adapter}
}

// CheckUpgrade observes the independent registry and retained dependencies. It
// neither attaches to a host nor sends an Agent RPC, starts tmux, writes config,
// creates a registry or advances cleanup. The caller runs the TARGET executable.
func CheckUpgrade(ctx context.Context, stateDir string) api.UpgradeReport {
	report := upgradeReport()
	add := func(runtime *api.Runtime, code string) {
		if runtime != nil {
			identity := upgradeIdentity(*runtime)
			runtime = &identity
		}
		report.Issues = append(report.Issues, api.RuntimeDiscoveryIssue{Runtime: runtime, Code: code})
	}
	if ctx.Err() != nil {
		add(nil, "UPGRADE_CHECK_DEADLINE")
		return report
	}
	program, err := runningprogram.Inspect(ctx)
	if err != nil {
		add(nil, "RUNNING_PROGRAM_UNVERIFIABLE")
		return report
	}
	report.Program = program
	if err := launchgate.CheckDirectory(stateDir); os.IsNotExist(err) {
		report.Allowed = true
		return report
	} else if err != nil {
		add(nil, "STATE_DIRECTORY_INVALID")
		return report
	}
	ctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	registryDirectory := filepath.Join(stateDir, "registry")
	registry, err := sessionregistry.OpenReadOnly(ctx, registryDirectory)
	if err != nil {
		if os.IsNotExist(err) {
			_, registryErr := os.Lstat(registryDirectory)
			_, artifactErr := os.Lstat(filepath.Join(stateDir, "acp"))
			if os.IsNotExist(registryErr) && os.IsNotExist(artifactErr) {
				report.Allowed = true
				return report
			}
		}
		add(nil, "REGISTRY_UNAVAILABLE")
		return report
	}
	defer registry.Close()
	discovery, err := registry.Hosts(ctx)
	if err != nil {
		add(nil, "REGISTRY_UNAVAILABLE")
		return report
	}
	for _, issue := range discovery.Issues {
		if issue.Runtime == nil || (issue.Code != "HOST_REGISTRATION_PENDING" && issue.Code != "LAUNCH_FAILED") {
			add(issue.Runtime, issue.Code)
			continue
		}
		runtime := issue.Runtime
		protocol := 0
		contract, digest := "", ""
		if runtime.ACPHost != nil {
			protocol = runtime.ACPHost.Protocol
			contract, digest = runtime.ACPHost.StateContract, runtime.ACPHost.ProgramSHA256
		}
		report.Hosts = append(report.Hosts, api.UpgradeHost{Runtime: upgradeIdentity(*runtime), Protocol: protocol, StateContract: contract, ProgramSHA256: digest, Phase: issue.Code})
		if issue.Code != "LAUNCH_FAILED" && protocol != sessionProtocol {
			add(runtime, "SESSION_PROTOCOL_UNSUPPORTED")
		}
		if issue.Code != "LAUNCH_FAILED" && (contract != statecontract.ID() || runtime.ACPHost == nil || (retainedprogram.Identity{SHA256: digest, Bytes: runtime.ACPHost.ProgramBytes}).Validate() != nil) {
			add(runtime, "HOST_STATE_CONTRACT_UNVERIFIABLE")
		}
	}
	engine := &Engine{stateDir: stateDir}
	for _, host := range discovery.Hosts {
		var reg sessionRegistration
		if json.Unmarshal(host.Registration, &reg) != nil {
			add(&host.Runtime, "REGISTRATION_INVALID")
			continue
		}
		phase := host.Phase
		if host.Retiring {
			phase = "retiring"
		}
		report.Hosts = append(report.Hosts, api.UpgradeHost{StateContract: reg.StateContract, Runtime: upgradeIdentity(host.Runtime), Target: &host.Target, Instance: host.Instance, Protocol: reg.Version, ProgramSHA256: reg.Program.SHA256, Phase: phase})
		if reg.StateContract != statecontract.ID() {
			add(&host.Runtime, "HOST_STATE_CONTRACT_UNVERIFIABLE")
			continue
		}
		if reg.Version != sessionProtocol {
			add(&host.Runtime, "SESSION_PROTOCOL_UNSUPPORTED")
			continue
		}
		if engine.verifyHostResources(host) != nil {
			add(&host.Runtime, "REGISTRATION_INVALID")
			continue
		}
		// Cleanup already sealed all admissions and proved the original Agent
		// ended. Its pinned program may legitimately have been removed.
		if !host.Retiring {
			if code := checkUpgradeProgram(ctx, stateDir, host, reg); code != "" {
				add(&host.Runtime, code)
			}
		}
	}
	if ctx.Err() != nil {
		add(nil, "UPGRADE_CHECK_DEADLINE")
	}
	report.Allowed = len(report.Issues) == 0
	return report
}

func checkUpgradeProgram(ctx context.Context, stateDir string, host sessionregistry.HostRecord, reg sessionRegistration) string {
	for _, path := range []string{filepath.Join(stateDir, "acp"), filepath.Join(stateDir, "acp", "runtimes"), host.Resources.Directory.Path} {
		if launchgate.CheckDirectory(path) != nil {
			return "RUNTIME_DIRECTORY_INVALID"
		}
	}
	root, err := os.OpenRoot(host.Resources.Directory.Path)
	if err != nil {
		return "RUNTIME_DIRECTORY_INVALID"
	}
	defer root.Close()
	info, err := root.Stat(".")
	if err != nil || !matchesFile(info, host.Resources.Directory, os.ModeDir) {
		return "RUNTIME_DIRECTORY_INVALID"
	}
	var instance string
	if privateRootFile(root, "instance.json", 256, &instance) != nil || instance != host.Instance {
		return "INSTANCE_MARKER_INVALID"
	}
	if retainedprogram.VerifyIn(ctx, root, "program", reg.Program) != nil {
		return "HOST_PROGRAM_UNAVAILABLE"
	}
	return ""
}

// PrepareUpgrade holds the exclusive launch gate until the caller finishes
// switching services. A failed check always releases it. There is no force flag:
// ending an incompatible Runtime remains a separate explicit lifecycle action.
func PrepareUpgrade(ctx context.Context, stateDir string) (*launchgate.Gate, api.UpgradeReport) {
	report := upgradeReport()
	if err := os.MkdirAll(stateDir, 0700); err != nil {
		report.Issues = append(report.Issues, api.RuntimeDiscoveryIssue{Code: "STATE_DIRECTORY_INVALID"})
		return nil, report
	}
	gate, err := launchgate.Acquire(stateDir, true)
	if err != nil {
		code := "LAUNCH_GATE_UNAVAILABLE"
		if errors.Is(err, launchgate.ErrBusy) {
			code = "LAUNCH_IN_PROGRESS"
		}
		report.Issues = append(report.Issues, api.RuntimeDiscoveryIssue{Code: code})
		return nil, report
	}
	seal, sealErr := launchgate.ReadSeal(stateDir)
	if sealErr != nil || seal != nil {
		gate.Close()
		report.Issues = append(report.Issues, api.RuntimeDiscoveryIssue{Code: "UPGRADE_RECOVERY_REQUIRED"})
		return nil, report
	}
	report = CheckUpgrade(ctx, stateDir)
	if !report.Allowed {
		gate.Close()
		return nil, report
	}
	return gate, report
}
