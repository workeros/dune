package fabricd

import (
	"fmt"

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
