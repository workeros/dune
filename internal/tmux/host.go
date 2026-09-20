package tmux

import (
	"fmt"
	"path/filepath"

	"github.com/aiomni/dune/internal/wire"
)

// CreateHost starts a protocol owner in this server's separate namespace.
// Only the executable and private bootstrap directory enter the pane command;
// Agent argv, environment and protocol bytes never enter tmux metadata or PTYs.
func (s *Server) CreateHost(id, executable, directory string) error {
	if !wire.ValidID(id) || !filepath.IsAbs(executable) || !filepath.IsAbs(directory) {
		return fmt.Errorf("invalid ACP host launch identity")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	command := "exec /usr/bin/env -i " + quote(executable) + " _acp_host " + quote(directory) + " </dev/null >/dev/null 2>/dev/null"
	_, err := s.run("new-session", "-d", "-s", "acp-"+id, "-c", directory, command)
	return err
}

func (s *Server) DestroyHost(id string) error {
	if !wire.ValidID(id) {
		return fmt.Errorf("invalid ACP host identity")
	}
	_, err := s.run("kill-session", "-t", "=acp-"+id)
	return err
}
