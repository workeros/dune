package fabricd

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"syscall"
	"time"

	"github.com/aiomni/dune/internal/launchgate"
	"github.com/aiomni/dune/internal/retainedprogram"
	"github.com/aiomni/dune/internal/sessionregistry"
	"github.com/aiomni/dune/pkg/api"
)

func upgradeReport() api.UpgradeReport {
	return api.UpgradeReport{CheckedAt: time.Now().UTC(), ProtocolMin: sessionProtocol, ProtocolMax: sessionProtocol, Hosts: []api.UpgradeHost{}, Issues: []api.RuntimeDiscoveryIssue{}}
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
	if err := launchgate.CheckDirectory(stateDir); os.IsNotExist(err) {
		report.Allowed = true
		return report
	} else if err != nil {
		add(nil, "STATE_DIRECTORY_INVALID")
		return report
	}
	ctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	registry, err := sessionregistry.OpenReadOnly(ctx, filepath.Join(stateDir, "registry"))
	if err != nil {
		code := "REGISTRY_UNAVAILABLE"
		if os.IsNotExist(err) {
			// Before this architecture an active connector could own an ACP
			// process in memory. Missing registry is never proof of no sessions.
			if legacyConnectorMayBeActive(stateDir) {
				code = "LEGACY_CONNECTOR_REQUIRES_EXPLICIT_TRANSITION"
			} else if _, artifactErr := os.Lstat(filepath.Join(stateDir, "acp")); os.IsNotExist(artifactErr) {
				report.Allowed = true
				return report
			}
		}
		add(nil, code)
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
		if runtime.ACPHost != nil {
			protocol = runtime.ACPHost.Protocol
		}
		report.Hosts = append(report.Hosts, api.UpgradeHost{Runtime: upgradeIdentity(*runtime), Protocol: protocol, Phase: issue.Code})
		if issue.Code != "LAUNCH_FAILED" && protocol != sessionProtocol {
			add(runtime, "SESSION_PROTOCOL_UNSUPPORTED")
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
		report.Hosts = append(report.Hosts, api.UpgradeHost{Runtime: upgradeIdentity(host.Runtime), Target: &host.Target, Instance: host.Instance, Protocol: reg.Version, ProgramSHA256: reg.Program.SHA256, Phase: phase})
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

func legacyConnectorMayBeActive(directory string) bool {
	f, err := os.OpenFile(filepath.Join(directory, "fabricd.lock"), os.O_RDONLY|syscall.O_NOFOLLOW|syscall.O_NONBLOCK, 0)
	if os.IsNotExist(err) {
		return false
	}
	if err != nil {
		return true
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil || !info.Mode().IsRegular() {
		return true
	}
	return syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB) != nil
}

// PrepareUpgrade holds the exclusive launch gate until the caller finishes
// switching services. A failed check always releases it. There is no force flag:
// ending an incompatible Runtime remains a separate explicit lifecycle action.
func PrepareUpgrade(ctx context.Context, stateDir string) (*os.File, api.UpgradeReport) {
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
	report = CheckUpgrade(ctx, stateDir)
	if !report.Allowed {
		gate.Close()
		return nil, report
	}
	return gate, report
}
