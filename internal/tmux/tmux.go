// Package tmux owns Dune's private tmux server. tmux alone owns PTYs, screens
// and scrollback; Dune viewers are disposable attach-session clients.
package tmux

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/aiomni/dune/pkg/api"
	"github.com/creack/pty"
)

type Server struct {
	Binary, Socket, config string
	mu                     sync.Mutex
}
type Session struct {
	Server  *Server
	Runtime api.Runtime
}
type Capture struct {
	Content      string `json:"content"`
	Rows         int    `json:"rows"`
	Cols         int    `json:"cols"`
	HistoryLines int    `json:"history_lines"`
	HistoryLimit int    `json:"history_limit"`
}
type Pane struct {
	Dead          bool
	ExitCode, PID int
}

func PrivateDir(dir string) error {
	if !filepath.IsAbs(dir) || filepath.Clean(dir) == "/" {
		return fmt.Errorf("session directory must be an absolute private directory")
	}
	if err := os.MkdirAll(dir, 0700); err != nil {
		return err
	}
	st, err := os.Lstat(dir)
	if err != nil {
		return err
	}
	owner, ok := st.Sys().(*syscall.Stat_t)
	if !st.IsDir() || st.Mode().Perm()&0077 != 0 || !ok || int(owner.Uid) != os.Getuid() {
		return fmt.Errorf("session directory must be owned by this user, mode 0700, and not a symlink")
	}
	return nil
}
func Open(dir string) (*Server, error) {
	if err := PrivateDir(dir); err != nil {
		return nil, err
	}
	binary := os.Getenv("DUNE_TMUX")
	if binary == "" {
		exe, _ := os.Executable()
		binary = filepath.Join(filepath.Dir(exe), "tmux")
		if _, err := os.Stat(binary); err != nil {
			binary, err = exec.LookPath("tmux")
			if err != nil {
				return nil, fmt.Errorf("bundled tmux missing; reinstall Dune or set DUNE_TMUX: %w", err)
			}
		}
	}
	binary, err := filepath.Abs(binary)
	if err != nil {
		return nil, err
	}
	// Keep AF_UNIX paths short even for long workspace/config paths.
	root := filepath.Join("/tmp", fmt.Sprintf("dune-tmux-%d", os.Getuid()))
	if err = PrivateDir(root); err != nil {
		return nil, err
	}
	key := fmt.Sprintf("%x", sha256.Sum256([]byte(filepath.Clean(dir))))[:24]
	socket := filepath.Join(root, key)
	config := filepath.Join(dir, "tmux.conf")
	body := "set -g exit-empty off\nset -g destroy-unattached off\nset -g status off\nset -g mouse on\nset -g prefix None\nset -g default-shell /bin/sh\nset -g default-terminal xterm-256color\nset -g update-environment ''\nset -g history-limit 50000\nset -g window-size latest\nset -g remain-on-exit on\nset -g automatic-rename off\nset -g allow-rename off\n"
	if err = os.WriteFile(config, []byte(body), 0600); err != nil {
		return nil, err
	}
	return &Server{Binary: binary, Socket: socket, config: config}, nil
}
func (s *Server) args(args ...string) []string {
	return append([]string{"-S", s.Socket, "-f", s.config}, args...)
}
func clientEnv() []string {
	return []string{"HOME=" + os.Getenv("HOME"), "PATH=" + os.Getenv("PATH"), "TERM=xterm-256color", "LANG=en_US.UTF-8", "LC_CTYPE=en_US.UTF-8"}
}

type output struct {
	bytes.Buffer
	limit int
}

func (b *output) Write(p []byte) (int, error) {
	if b.Len()+len(p) > b.limit {
		return 0, fmt.Errorf("tmux output exceeds limit")
	}
	return b.Buffer.Write(p)
}
func (s *Server) run(args ...string) (string, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, s.Binary, s.args(args...)...)
	cmd.Env = clientEnv()
	out := &output{limit: 2 * 1024 * 1024}
	var stderr bytes.Buffer
	cmd.Stdout = out
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		if ctx.Err() != nil {
			return "", fmt.Errorf("tmux did not confirm operation: %w", ctx.Err())
		}
		return "", fmt.Errorf("tmux: %w: %s", err, strings.TrimSpace(stderr.String()))
	}
	return out.String(), nil
}
func quote(s string) string { return "'" + strings.ReplaceAll(s, "'", "'\"'\"'") + "'" }

// Resolve using the child environment and working directory, before creating a
// retained pane. A missing program is a start failure, not a successful runtime.
func executable(name, cwd string, env []string) error {
	paths := []string{name}
	if !strings.ContainsRune(name, '/') {
		path := "/usr/bin:/bin"
		for _, item := range env {
			if strings.HasPrefix(item, "PATH=") {
				path = strings.TrimPrefix(item, "PATH=")
			}
		}
		paths = nil
		for _, dir := range filepath.SplitList(path) {
			paths = append(paths, filepath.Join(dir, name))
		}
	}
	for _, path := range paths {
		if !filepath.IsAbs(path) {
			path = filepath.Join(cwd, path)
		}
		if st, err := os.Stat(path); err == nil && !st.IsDir() && st.Mode().Perm()&0111 != 0 {
			return nil
		}
	}
	return fmt.Errorf("executable not found or not executable: %s", name)
}
func (s *Server) Create(meta api.Runtime, argv, env []string, limit int) (*Session, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if limit == 0 {
		limit = 50000
	}
	if limit < 1 || limit > 200000 {
		return nil, fmt.Errorf("history limit must be 1..200000")
	}
	if len(argv) == 0 {
		return nil, fmt.Errorf("empty command")
	}
	if err := executable(argv[0], meta.WorkingDirectory, env); err != nil {
		return nil, err
	}
	// A single quoted shell argument prevents tmux's command separator parser
	// from interpreting arbitrary argv/env values. env -i prevents cross-session
	// inheritance from the long-lived server's original environment.
	words := []string{"exec", "/usr/bin/env", "-i", quote("TERM=xterm-256color")}
	for _, v := range env {
		if strings.HasPrefix(v, "TMUX=") || strings.HasPrefix(v, "TMUX_PANE=") || strings.HasPrefix(v, "PWD=") {
			continue
		}
		words = append(words, quote(v))
	}
	words = append(words, quote("PWD="+meta.WorkingDirectory))
	for _, v := range argv {
		words = append(words, quote(v))
	}
	b, _ := json.Marshal(meta)
	r := &Session{Server: s, Runtime: meta}
	_, err := s.run("start-server", ";", "set-option", "-g", "history-limit", strconv.Itoa(limit), ";", "new-session", "-d", "-s", r.name(), "-x", "80", "-y", "24", "-c", meta.WorkingDirectory, strings.Join(words, " "), ";", "set-option", "-t", r.target(), "@dune-runtime", base64.RawStdEncoding.EncodeToString(b))
	if err != nil {
		return nil, err
	}
	return r, nil
}
func (s *Server) Restore() ([]*Session, error) {
	if _, err := os.Stat(s.Socket); os.IsNotExist(err) {
		return nil, nil
	}
	out, err := s.run("list-sessions", "-F", "#{session_name} #{@dune-runtime}")
	if err != nil {
		if strings.Contains(err.Error(), "no server running") || strings.Contains(err.Error(), "no sessions") {
			return nil, nil
		}
		return nil, err
	}
	var result []*Session
	for _, line := range strings.Split(strings.TrimSpace(out), "\n") {
		name, data, ok := strings.Cut(line, " ")
		if !ok || !strings.HasPrefix(name, "dune-") {
			continue
		}
		b, e := base64.RawStdEncoding.DecodeString(data)
		if e != nil {
			return nil, fmt.Errorf("invalid tmux runtime metadata")
		}
		var meta api.Runtime
		if json.Unmarshal(b, &meta) != nil || meta.Adapter != "pty" || name != "dune-"+meta.ID || len(meta.ID) != 32 {
			return nil, fmt.Errorf("invalid tmux runtime identity")
		}
		result = append(result, &Session{Server: s, Runtime: meta})
	}
	return result, nil
}
func (s *Server) Close() error    { _, err := s.run("kill-server"); return err }
func (r *Session) name() string   { return "dune-" + r.Runtime.ID }
func (r *Session) target() string { return r.name() }
func (r *Session) pane() string   { return r.target() + ":0.0" }
func (r *Session) Inspect() (Pane, error) {
	out, err := r.Server.run("display-message", "-p", "-t", r.pane(), "#{pane_dead} #{pane_dead_status} #{pane_pid}")
	if err != nil {
		return Pane{}, err
	}
	f := strings.Fields(out)
	var p Pane
	if len(f) < 2 {
		return p, fmt.Errorf("invalid tmux pane state")
	}
	p.Dead = f[0] == "1"
	p.PID, _ = strconv.Atoi(f[len(f)-1])
	if p.Dead && len(f) == 3 {
		p.ExitCode, _ = strconv.Atoi(f[1])
	}
	return p, nil
}
func (r *Session) Destroy() error {
	_, err := r.Server.run("kill-session", "-t", r.target())
	if err != nil && (strings.Contains(err.Error(), "can't find session") || strings.Contains(err.Error(), "no server running")) {
		return nil
	}
	return err
}
func (r *Session) Capture() (Capture, error) {
	var c Capture
	out, err := r.Server.run("display-message", "-p", "-t", r.pane(), "#{pane_height} #{pane_width} #{history_size} #{history_limit}")
	if err != nil {
		return c, err
	}
	if _, err = fmt.Sscanf(out, "%d %d %d %d", &c.Rows, &c.Cols, &c.HistoryLines, &c.HistoryLimit); err != nil {
		return c, err
	}
	c.Content, err = r.Server.run("capture-pane", "-p", "-e", "-t", r.pane())
	return c, err
}
func (r *Session) History(action string) error {
	var args []string
	switch action {
	case "older":
		args = []string{"copy-mode", "-u", "-t", r.pane()}
	case "newer":
		args = []string{"send-keys", "-X", "-t", r.pane(), "page-down"}
	case "close":
		args = []string{"send-keys", "-X", "-t", r.pane(), "cancel"}
	default:
		return fmt.Errorf("history action must be older, newer or close")
	}
	_, err := r.Server.run(args...)
	return err
}

// Viewer owns only the tmux client PTY. Closing it never destroys the session.
type Viewer struct {
	File *os.File
	cmd  *exec.Cmd
	done chan struct{}
	once sync.Once
}

func (r *Session) Attach(observe bool) (*Viewer, error) {
	// Start the viewer at the pane's existing size. A temporary 80x24 attach
	// would reflow (and evict) retained history before the browser resizes it.
	size, err := r.Server.run("display-message", "-p", "-t", r.pane(), "#{pane_height} #{pane_width}")
	if err != nil {
		return nil, err
	}
	var rows, cols uint16
	if _, err = fmt.Sscanf(size, "%d %d", &rows, &cols); err != nil {
		return nil, err
	}

	args := []string{"attach-session", "-t", r.target()}
	if observe {
		args = append(args, "-r")
	}
	cmd := exec.Command(r.Server.Binary, r.Server.args(args...)...)
	cmd.Env = clientEnv()
	f, err := pty.StartWithSize(cmd, &pty.Winsize{Rows: rows, Cols: cols})
	if err != nil {
		return nil, err
	}
	v := &Viewer{File: f, cmd: cmd, done: make(chan struct{})}
	go func() { _ = cmd.Wait(); close(v.done) }()
	return v, nil
}
func (v *Viewer) Read(b []byte) (int, error) { return v.File.Read(b) }
func (v *Viewer) Write(b []byte) error {
	_ = v.File.SetWriteDeadline(time.Now().Add(5 * time.Second))
	_, err := v.File.Write(b)
	return err
}
func (v *Viewer) Resize(rows, cols uint16) error {
	if rows < 1 || rows > 200 || cols < 1 || cols > 400 {
		return fmt.Errorf("terminal size must be 1..400 columns by 1..200 rows")
	}
	return pty.Setsize(v.File, &pty.Winsize{Rows: rows, Cols: cols})
}
func (v *Viewer) Close() {
	v.once.Do(func() { _ = v.File.Close(); _ = v.cmd.Process.Kill(); <-v.done })
}

var _ io.Reader = (*Viewer)(nil)

// Panes probes all managed sessions in one command, independent of viewer count.
func (s *Server) Panes() (map[string]Pane, error) {
	out, err := s.run("list-panes", "-a", "-F", "#{session_name}\t#{pane_dead}\t#{pane_dead_status}\t#{pane_pid}")
	result := map[string]Pane{}
	if err != nil {
		if strings.Contains(err.Error(), "no server running") || strings.Contains(err.Error(), "no sessions") {
			return result, nil
		}
		return nil, err
	}
	for _, line := range strings.Split(strings.TrimSpace(out), "\n") {
		f := strings.Split(line, "\t")
		if len(f) != 4 || !strings.HasPrefix(f[0], "dune-") {
			continue
		}
		p := Pane{Dead: f[1] == "1"}
		p.ExitCode, _ = strconv.Atoi(f[2])
		p.PID, _ = strconv.Atoi(f[3])
		result[strings.TrimPrefix(f[0], "dune-")] = p
	}
	return result, nil
}
