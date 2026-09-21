package fabricd

import (
	"fmt"
	"github.com/aiomni/dune/internal/retainedprogram"
	"github.com/aiomni/dune/internal/wire"
	"github.com/aiomni/dune/pkg/api"
	"os"
	"path/filepath"

	"github.com/aiomni/dune/internal/process"
	"github.com/aiomni/dune/internal/sessionregistry"
)

func (d *Engine) confirmAbsentStartup(host sessionregistry.HostRecord) {
	if host.Phase == "failed" || host.PID <= 1 || host.Runtime.ACPHost == nil {
		return
	}
	if d.verifyHostResources(host) != nil {
		return
	}
	absent, err := process.Absent(host.BootID, host.PID, host.GroupID)
	if err != nil || !absent {
		return
	}
	code := "HOST_EXITED_BEFORE_ENTRY"
	if diagnostic := host.Runtime.ACPHost.Startup; diagnostic != nil {
		switch diagnostic.Phase {
		case "ready":
			code = "HOST_START_FAILED"
		case "host_validation":
			code = "HOST_VALIDATION_FAILED"
		case "agent_start":
			code = "AGENT_START_FAILED"
		case "agent_initialization":
			code = "AGENT_INITIALIZATION_FAILED"
		}
	}
	// Kernel absence complements the recorded new-session/host entry identity.
	// The transaction fences both a delayed host and a concurrent guardian spawn.
	_ = d.registry.FailHost(d.ctx, host.Target, host.Instance, code, func(current sessionregistry.HostRecord) error {
		absent, err := process.Absent(current.BootID, current.PID, current.GroupID)
		if err != nil {
			return err
		}
		if !absent {
			return fmt.Errorf("original host or Agent group may still be alive")
		}
		return nil
	})
}

func (d *Engine) recoverHostStartups() {
	hosts, err := d.registry.Hosts(d.ctx)
	if err != nil {
		return
	}
	for _, host := range hosts.Hosts {
		d.confirmAbsentStartup(host)
	}
}

// Explicit lifecycle requests can recover the earlier failed launches that had
// no independent host record. Discovery and submission.get never call this.
func (d *Engine) confirmUnregisteredStartup(target api.SubmissionTarget) bool {
	if d.lock == nil {
		return false
	}
	d.mu.Lock()
	launching := d.launching[target.RuntimeID] != ""
	d.mu.Unlock()
	if launching {
		return false
	}
	receipt, err := d.registry.PendingLaunch(d.ctx, target)
	if err != nil || receipt.Admission != api.SubmissionAccepted || receipt.Stage != "host_starting" || receipt.Runtime == nil || receipt.Runtime.Adapter != "acp" {
		return false
	}
	directory := filepath.Join(d.stateDir, "acp", "runtimes", target.RuntimeID)
	var boot sessionBootstrap
	if privateFile(filepath.Join(directory, "bootstrap.json"), 2*wire.MaxMessage, &boot) != nil {
		return false
	}
	reg := boot.Registration
	if reg.Target != target || boot.StateDir != d.stateDir || reg.Installation != installationID(d.stateDir) || boot.Launch.SubmissionKey != receipt.SubmissionKey || boot.Launch.OperationRef != receipt.OperationRef || reg.Runtime.ID != receipt.Runtime.ID || reg.Runtime.Incarnation != receipt.Runtime.Incarnation || reg.Runtime.Generation != receipt.Runtime.Generation {
		return false
	}
	socket, err := sessionSocketPath(reg)
	if err != nil {
		return false
	}
	var instance string
	if privateFile(filepath.Join(directory, "instance.json"), 256, &instance) != nil || instance != reg.Instance {
		return false
	}
	if retainedprogram.Verify(filepath.Join(directory, "program"), reg.Program) != nil {
		return false
	}
	resource, err := fileIdentity(directory, os.ModeDir)
	if err != nil {
		return false
	}
	bootID, err := process.BootID()
	if err != nil {
		return false
	}
	pid, err := d.acpTmux.ExitedHostProcess(target.RuntimeID, reg.Instance)
	if err != nil {
		return false
	}
	observed := *receipt.Runtime
	observed.ACPHost = &api.ACPHostInfo{Protocol: reg.Version, Instance: reg.Instance, ProgramSHA256: reg.Program.SHA256, ProgramBytes: reg.Program.Bytes}
	host := sessionregistry.HostRecord{Target: target, Instance: reg.Instance, BootID: bootID, PID: pid, Runtime: observed, Registration: api.Payload(reg), Resources: sessionregistry.CleanupResources{Directory: resource, Socket: sessionregistry.FileIdentity{Path: socket}, Instance: reg.Instance, TmuxSession: "acp-" + target.RuntimeID}}
	err = d.registry.ConfirmUnregisteredFailure(d.ctx, host, func() error {
		if _, err := os.Lstat(socket); !os.IsNotExist(err) {
			return fmt.Errorf("unregistered endpoint may still exist")
		}
		absent, err := process.Absent(bootID, pid, 0)
		if err != nil || !absent {
			return fmt.Errorf("original pane process absence is unconfirmed")
		}
		return nil
	})
	if err != nil {
		return false
	}
	_ = d.discoverSessions()
	return true
}
