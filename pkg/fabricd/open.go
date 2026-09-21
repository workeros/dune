// Package fabricd provides the Dune execution engine and private ACP host.
// Connectors route persistent ACP operations to their independent host; HTTP,
// identity and product storage remain outside this package.
package fabricd

import (
	"context"
	"fmt"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"
	"time"

	"github.com/aiomni/dune/internal/agentintegration"
	"github.com/aiomni/dune/internal/lifecycle"
	"github.com/aiomni/dune/internal/mcpbridge"
	"github.com/aiomni/dune/internal/process"
	"github.com/aiomni/dune/internal/sessionregistry"
	"github.com/aiomni/dune/internal/tmux"
	"github.com/aiomni/dune/pkg/api"
)

// Open restores persistent runtimes and exclusively locks stateDir for this
// engine's lifetime. An existing directory must be owned by this user and mode
// 0700; a missing directory is created privately. The host must dispatch
// RunHelper before its normal startup
// and provide tmux beside its executable, on PATH, or through DUNE_TMUX.
// Close releases the lock without destroying tmux sessions.
func Open(ctx context.Context, stateDir string) (_ *Engine, err error) {
	return openWithCleanupBarrier(ctx, stateDir, nil)
}

// The private barrier permits process acceptance tests to suspend cleanup at
// exact durable/physical boundaries without introducing a production RPC hook.
func openWithCleanupBarrier(ctx context.Context, stateDir string, barrier func(api.SubmissionKey, string) error) (_ *Engine, err error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if stateDir == "" {
		return nil, fmt.Errorf("state directory required")
	}
	stateDir, err = filepath.Abs(stateDir)
	if err != nil {
		return nil, err
	}
	if err := tmux.PrivateDir(stateDir); err != nil {
		return nil, err
	}
	d := newEngine(ctx)
	d.cleanupBarrier = barrier
	defer func() {
		if err != nil {
			d.Close()
		}
	}()
	d.lock, err = os.OpenFile(filepath.Join(stateDir, "fabricd.lock"), os.O_CREATE|os.O_RDWR|syscall.O_NOFOLLOW, 0600)
	if err != nil {
		return nil, err
	}
	if err := syscall.Flock(int(d.lock.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		return nil, fmt.Errorf("fabricd already running: %w", err)
	}
	d.tmux, err = tmux.Open(stateDir)
	if err != nil {
		return nil, err
	}
	d.stateDir = stateDir
	d.sessionTerm, err = nextSessionTerm(stateDir)
	if err != nil {
		return nil, err
	}
	d.acpTmux, err = tmux.Open(filepath.Join(stateDir, "acp"))
	if err != nil {
		return nil, err
	}
	d.registry, err = sessionregistry.Open(ctx, filepath.Join(stateDir, "registry"), sessionregistry.Options{})
	if err != nil {
		return nil, err
	}
	if err := d.discoverSessions(); err != nil {
		return nil, err
	}
	d.events, err = lifecycle.Open(stateDir, "connector-events.jsonl")
	if err != nil {
		return nil, err
	}
	d.recordLifecycle("connector_started", nil, "", "", d.sessionTerm)
	sessions, err := d.tmux.Restore()
	if err != nil {
		return nil, err
	}
	for _, session := range sessions {
		m := session.Runtime
		r := &runtime{id: m.ID, inc: m.Incarnation, title: m.Title, cwd: m.WorkingDirectory, projectID: m.ProjectID, directoryID: m.DirectoryID, adapter: "pty", tmux: session, subs: map[*subscription]bool{}, done: make(chan struct{})}
		state, err := session.TimeoutState()
		if err != nil {
			return nil, err
		}
		r.readTimeoutState(state)
		r.readNativeSession()
		d.runtimes[m.ID] = r
	}
	d.cleaner, err = process.NewCleaner()
	if err != nil {
		return nil, err
	}
	go d.watchTmux()
	go d.expire(d.ctx)
	d.recoverHostStartups()
	if err := d.recoverCleanups(); err != nil {
		return nil, err
	}
	d.active.Add(1)
	go d.runCleanupRecovery()
	d.active.Add(1)
	go d.restoreSessionConnections()
	return d, nil
}

// RunHelper dispatches the private child commands for process cleanup, temporary
// uploads, and persistent PTY deadlines. Hosts call it with os.Args[1:] before
// parsing their own flags and exit with code when handled is true.
func RunHelper(args []string) (code int, handled bool) {
	if len(args) == 0 {
		return 0, false
	}
	switch args[0] {
	case mcpbridge.Command:
		ctx, cancel := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
		defer cancel()
		if len(args) != 1 {
			fmt.Fprintln(os.Stderr, "MCP bridge accepts only launch configuration")
			return 1, true
		}
		config := api.AgentMCP{URL: os.Getenv(mcpbridge.URLEnv), Token: os.Getenv(mcpbridge.TokenEnv)}
		if config.Token == "" && os.Getenv(agentintegration.SessionDirEnv) != "" {
			var err error
			config, err = agentintegration.AwaitMCP(ctx, os.Getenv(agentintegration.SessionDirEnv))
			if err != nil {
				fmt.Fprintln(os.Stderr, "Agent MCP configuration was not confirmed")
				return 1, true
			}
		}
		if err := mcpbridge.Run(ctx, config.URL, config.Token, os.Stdin, os.Stdout); err != nil {
			fmt.Fprintln(os.Stderr, err)
			return 1, true
		}
		return 0, true
	case agentintegration.SessionCommand:
		// Native hooks are observational: no stdout, event echo or policy
		// decision. A missing receipt must not block the Agent's own lifecycle.
		if len(args) == 1 {
			ctx, cancel := context.WithTimeout(context.Background(), time.Second)
			defer cancel()
			done := make(chan struct{})
			go func() {
				_ = agentintegration.Report(ctx, os.Getenv(agentintegration.SessionDirEnv), os.Getenv("CODEX_THREAD_ID"), os.Stdin)
				close(done)
			}()
			select {
			case <-done:
			case <-ctx.Done():
			}
		}
		return 0, true
	case "_acp_host":
		if len(args) != 2 {
			return 1, true
		}
		if err := runSessionHost(args[1]); err != nil {
			return 1, true
		}
		return 0, true
	case "_cleanup":
		return process.CleanupGuard(), true
	case "_pty_guard":
		return process.PTYGuard(args[1:]), true
	case "_guard":
		return process.Guard(args[1:]), true
	default:
		return 0, false
	}
}
