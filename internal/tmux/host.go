package tmux

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/aiomni/dune/internal/wire"
)

// CreateHost starts a protocol owner in this server's separate namespace.
// Only the executable and private bootstrap directory enter the pane command;
// Agent argv, environment and protocol bytes never enter tmux metadata or PTYs.
func (s *Server) CreateHost(id, instance, executable, directory string) error {
	if !wire.ValidID(id) || !wire.ValidID(instance) || !filepath.IsAbs(executable) || !filepath.IsAbs(directory) {
		return fmt.Errorf("invalid ACP host launch identity")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	command := "exec /usr/bin/env -i " + quote(executable) + " _acp_host " + quote(directory) + " </dev/null >/dev/null 2>/dev/null"
	_, err := s.run("new-session", "-d", "-s", "acp-"+id, "-e", "DUNE_ACP_INSTANCE="+instance, "-c", directory, command)
	return err
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
