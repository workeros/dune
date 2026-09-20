package fabricd

import (
	"context"
	"os"
	"sync"
	"time"

	"github.com/aiomni/dune/internal/buildinfo"
	"github.com/aiomni/dune/internal/tmux"
	"github.com/aiomni/dune/pkg/api"
)

func (d *Engine) connectorInfo() api.ConnectorInfo {
	return api.ConnectorInfo{Build: buildinfo.Current(), PID: os.Getpid(), StartedAt: d.startedAt, Incarnation: d.inc, ProtocolMin: sessionProtocol, ProtocolMax: sessionProtocol}
}

// At most one pair of probes runs per Engine, at most once every five seconds.
// Probe subprocesses have independent two-second deadlines. They never hold
// the Runtime/Engine state lock or delay another request waiting for that lock.
func (d *Engine) tmuxVersions(ctx context.Context) []api.TmuxVersion {
	if !d.versionsMu.TryLock() {
		return []api.TmuxVersion{{Namespace: "pty", ServerState: "checking"}, {Namespace: "acp", ServerState: "checking"}}
	}
	defer d.versionsMu.Unlock()
	if time.Since(d.versionsAt) < 5*time.Second {
		return append([]api.TmuxVersion(nil), d.versions...)
	}
	versions := []api.TmuxVersion{{Namespace: "pty", ServerState: "absent"}, {Namespace: "acp", ServerState: "absent"}}
	var probes sync.WaitGroup
	for i, server := range []*tmux.Server{d.tmux, d.acpTmux} {
		if server == nil {
			continue
		}
		probes.Go(func() {
			info := server.Versions(ctx)
			info.Namespace = versions[i].Namespace
			versions[i] = info
		})
	}
	probes.Wait()
	d.versions, d.versionsAt = versions, time.Now()
	return append([]api.TmuxVersion(nil), versions...)
}

// HostProtocolRange is the current executable's supported live host contract.
func HostProtocolRange() (minimum, maximum int) { return sessionProtocol, sessionProtocol }
