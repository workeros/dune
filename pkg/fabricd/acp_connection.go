package fabricd

import (
	"context"
	"sync"
	"time"

	"github.com/aiomni/dune/internal/process"
	"github.com/aiomni/dune/pkg/api"
)

// The caller holds a.mu through any mutation or publication for this output.
// A new model is already visible while the previous connection is draining.
func (a *acpController) acceptsOutputLocked(connection *process.Process) bool {
	return !a.reconnecting && (connection == nil || a.connection == connection)
}

// Explicit opens on a used connection get a new process/stdio connection.
// ACP session/update has no generation field, so draining/reusing the old
// connection cannot safely distinguish late updates when a native ID is reused.
func (a *acpController) reconnect() error {
	r := a.r
	r.mu.Lock()
	old, drained, start := r.p, r.acpReadDone, r.acpStart
	stopped := r.stopped
	r.mu.Unlock()
	if stopped || start == nil {
		return &api.Error{Code: "UNSUPPORTED", Detail: "cannot establish an isolated ACP connection for this open"}
	}
	old.Close()
	<-old.Done
	groupCtx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	err := old.WaitGroupExit(groupCtx)
	cancel()
	if err != nil {
		return &api.Error{Code: "RESULT_UNKNOWN", Detail: "previous ACP process group exit is unconfirmed: " + err.Error()}
	}
	if drained != nil {
		<-drained
	}
	r.spawnMu.Lock()
	r.mu.Lock()
	stopped = r.stopped
	r.mu.Unlock()
	if stopped {
		r.spawnMu.Unlock()
		return &api.Error{Code: "RUNTIME_STOPPED", Detail: "Runtime stopped before explicit session open"}
	}
	next, err := start()
	if err != nil {
		r.spawnMu.Unlock()
		return &api.Error{Code: "ACP_OPEN_FAILED", Detail: "start isolated ACP connection: " + err.Error()}
	}
	// Publish ownership before waiting on any ACP lock. Stop can close this
	// process even when an old input writer or initialization is blocked.
	r.mu.Lock()
	r.p = next
	stopped = r.stopped
	r.mu.Unlock()
	r.spawnMu.Unlock()
	if stopped {
		next.Close()
		next.Output.Close()
		next.Stderr.Close()
		return &api.Error{Code: "RUNTIME_STOPPED", Detail: "Runtime stopped during explicit session open"}
	}
	a.inputMu.Lock()
	a.mu.Lock()
	r.mu.Lock()
	if r.stopped {
		r.mu.Unlock()
		a.mu.Unlock()
		a.inputMu.Unlock()
		next.Close()
		next.Output.Close()
		next.Stderr.Close()
		return &api.Error{Code: "RESULT_UNKNOWN", Detail: "Runtime stopped during explicit session open"}
	}
	a.connection, a.reconnecting = next, false
	r.mu.Unlock()
	a.mu.Unlock()
	a.inputMu.Unlock()
	r.runProcess(next)
	if err := a.initializeConnection(); err != nil {
		return err
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.active != nil && a.active.request.Action == "load" && !a.state.CanLoad {
		return &api.Error{Code: "UNSUPPORTED", Detail: "new ACP connection does not support session/load"}
	}
	return nil
}

func (r *runtime) runProcess(proc *process.Process) {
	drained := make(chan struct{})
	r.mu.Lock()
	r.acpReadDone = drained
	r.mu.Unlock()
	var group sync.WaitGroup
	group.Add(1)
	go func() {
		if r.raw != nil {
			defer group.Done()
			r.raw.stdout.consume(proc.Output)
			return
		}
		if r.acp == nil {
			r.read(proc.Output, "data", &group)
			return
		}
		defer group.Done()
		defer proc.Output.Close()
		r.readACPConnection(proc.Output, proc)
	}()
	if proc.Stderr != nil {
		group.Add(1)
		go func() {
			if r.raw != nil {
				defer group.Done()
				r.raw.stderr.consume(proc.Stderr)
				return
			}
			r.read(proc.Stderr, "stderr", &group)
		}()
	}
	go func() {
		<-proc.Done
		ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
		groupErr := proc.WaitGroupExit(ctx)
		cancel()
		group.Wait()
		close(drained)
		if groupErr != nil {
			// EOF/guardian death alone cannot establish that the group exited.
			return
		}
		if r.acp != nil {
			r.acp.mu.Lock()
			current := r.acp.connection == proc && !r.acp.reconnecting
			if current {
				r.acp.closedWithExitLocked(&proc.Exit)
			}
			r.acp.mu.Unlock()
			if !current {
				return
			}
		}
		r.finish(proc.Exit)
		proc.Close()
	}()
}
