package tmux

import (
	"context"
	"crypto/sha256"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/aiomni/dune/internal/wire"
)

type HostPane struct {
	ID, Instance, Reference string
}

// HostPanes is a bounded read of this installation's separate ACP server. Pane
// markers are discovery hints only; neither they nor PIDs grant execution rights.
func (s *Server) HostPanes(ctx context.Context) ([]HostPane, error) {
	if _, err := os.Lstat(s.Socket); os.IsNotExist(err) {
		return nil, nil
	}
	out := &output{limit: 64 * 1024}
	if err := s.runOutputContext(ctx, out, "list-sessions", "-F", "#{session_name}\t#{DUNE_ACP_INSTANCE}"); err != nil {
		if strings.Contains(err.Error(), "no sessions") {
			return nil, nil
		}
		return nil, fmt.Errorf("ACP pane discovery unavailable")
	}
	var result []HostPane
	for _, row := range strings.Split(strings.TrimSuffix(out.String(), "\n"), "\n") {
		if row == "" {
			continue
		}
		if len(result) == 256 {
			return result, fmt.Errorf("ACP pane discovery limit exceeded")
		}
		digest := sha256.Sum256([]byte(row))
		pane := HostPane{Reference: fmt.Sprintf("pane:%x", digest[:16])}
		fields := strings.Split(row, "\t")
		if len(fields) == 2 && strings.HasPrefix(fields[0], "acp-") && wire.ValidID(strings.TrimPrefix(fields[0], "acp-")) {
			pane.ID = strings.TrimPrefix(fields[0], "acp-")
			if wire.ValidID(fields[1]) {
				pane.Instance = fields[1]
			}
		}
		result = append(result, pane)
	}
	return result, nil
}

// CreateHost starts a protocol owner in this server's separate namespace.
// Only the executable and private bootstrap directory enter the pane command;
// Agent argv, environment and protocol bytes never enter tmux metadata or PTYs.
func (s *Server) CreateHost(id, instance, executable, directory string) (int, error) {
	if !wire.ValidID(id) || !wire.ValidID(instance) || !filepath.IsAbs(executable) || !filepath.IsAbs(directory) {
		return 0, fmt.Errorf("invalid ACP host launch identity")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	// Keep the slave terminal open independently of stdio. Closing every slave
	// descriptor makes Linux tmux close the pane and SIGHUP its foreground group
	// before the host can start. Descriptor 3 carries no Agent protocol data.
	command := "exec /usr/bin/env -i " + quote(executable) + " _acp_host " + quote(directory) + " 3</dev/tty </dev/null >/dev/null 2>/dev/null"
	out, err := s.run("new-session", "-d", "-P", "-F", "#{pane_pid}", "-s", "acp-"+id, "-e", "DUNE_ACP_INSTANCE="+instance, "-c", directory, command)
	if err != nil {
		return 0, err
	}
	pid, err := strconv.Atoi(strings.TrimSpace(out))
	if err != nil || pid <= 1 {
		return 0, fmt.Errorf("ACP pane process identity unavailable")
	}
	return pid, nil
}

// DestroyHost checks the instance marker and addresses the server's immutable
// session ID. The condition and kill run on one server connection, so neither
// a reused session name nor a restarted server can redirect the old cleanup.
func (s *Server) DestroyHost(id, instance string) error {
	if !wire.ValidID(id) || !wire.ValidID(instance) {
		return fmt.Errorf("invalid ACP host identity")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	session, err := s.hostSession(id, instance)
	if err != nil || session == "" {
		return err
	}
	condition := "#{==:#{DUNE_ACP_INSTANCE}," + instance + "}"
	if _, err := s.run("if-shell", "-F", "-t", session, condition, "kill-session -t "+session); err != nil {
		return err
	}
	remaining, err := s.hostSession(id, instance)
	if err != nil {
		return err
	}
	if remaining != "" {
		return fmt.Errorf("ACP tmux host retirement is unconfirmed")
	}
	return nil
}

func (s *Server) hostSession(id, instance string) (string, error) {
	rows, err := s.run("list-sessions", "-F", "#{session_name}\t#{session_id}\t#{DUNE_ACP_INSTANCE}")
	if err != nil {
		if _, statErr := os.Lstat(s.Socket); os.IsNotExist(statErr) {
			return "", nil
		}
		return "", err
	}
	for _, row := range strings.Split(strings.TrimSuffix(rows, "\n"), "\n") {
		fields := strings.Split(row, "\t")
		if fields[0] != "acp-"+id {
			continue
		}
		if len(fields) != 3 || fields[2] != instance || len(fields[1]) < 2 || fields[1][0] != '$' || strings.Trim(fields[1][1:], "0123456789") != "" {
			return "", fmt.Errorf("ACP tmux host resource identity changed")
		}
		return fields[1], nil
	}
	return "", nil
}

// ExitedHostProcess observes the exact marked pane. It is not failure proof by
// itself; callers must also prove kernel absence and fence host registration.
func (s *Server) ExitedHostProcess(id, instance string) (int, error) {
	if !wire.ValidID(id) || !wire.ValidID(instance) {
		return 0, fmt.Errorf("invalid ACP host identity")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	session, err := s.hostSession(id, instance)
	if err != nil || session == "" {
		return 0, fmt.Errorf("original ACP pane is not observable")
	}
	out, err := s.run("list-panes", "-t", session, "-F", "#{pane_pid}\t#{pane_dead}")
	if err != nil {
		return 0, err
	}
	fields := strings.Split(strings.TrimSpace(out), "\t")
	if len(fields) != 2 || fields[1] != "1" {
		return 0, fmt.Errorf("original ACP pane exit is unconfirmed")
	}
	pid, err := strconv.Atoi(fields[0])
	if err != nil || pid <= 1 {
		return 0, fmt.Errorf("original ACP pane process is unavailable")
	}
	return pid, nil
}
