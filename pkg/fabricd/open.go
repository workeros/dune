// Package fabricd provides the Dune execution engine. It owns PTY/ACP, files,
// Git, ports, and runtime lifecycle, without HTTP, identity, or product storage.
package fabricd

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"syscall"

	"github.com/aiomni/dune/internal/process"
	"github.com/aiomni/dune/internal/tmux"
)

// Open restores persistent runtimes and exclusively locks stateDir for this
// engine's lifetime. An existing directory must be owned by this user and mode
// 0700; a missing directory is created privately. The host must dispatch
// RunHelper before its normal startup
// and provide tmux beside its executable, on PATH, or through DUNE_TMUX.
// Close releases the lock without destroying tmux sessions.
func Open(ctx context.Context, stateDir string) (_ *Engine, err error) {
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
	sessions, err := d.tmux.Restore()
	if err != nil {
		return nil, err
	}
	for _, session := range sessions {
		m := session.Runtime
		d.runtimes[m.ID] = &runtime{id: m.ID, inc: m.Incarnation, title: m.Title, cwd: m.WorkingDirectory, adapter: "pty", tmux: session, subs: map[*subscription]bool{}, done: make(chan struct{})}
	}
	d.cleaner, err = process.NewCleaner()
	if err != nil {
		return nil, err
	}
	go d.watchTmux()
	go d.expire(d.ctx)
	return d, nil
}

// RunHelper dispatches the dedicated child commands used to clean process groups
// and temporary uploads after connector death. Hosts call it with os.Args[1:]
// before parsing their own flags and exit with code when handled is true.
func RunHelper(args []string) (code int, handled bool) {
	if len(args) == 0 {
		return 0, false
	}
	switch args[0] {
	case "_cleanup":
		return process.CleanupGuard(), true
	case "_guard":
		return process.Guard(args[1:]), true
	default:
		return 0, false
	}
}
