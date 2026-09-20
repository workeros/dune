package tmux

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"os"
	"strings"

	"github.com/aiomni/dune/internal/wire"
	"github.com/aiomni/dune/pkg/api"
)

// CleanupDirectories are Dune's generated configuration/timeout caches. They
// never include the working directory or the Agent's native history directory.
func (r *Session) CleanupDirectories() []string {
	paths := []string{}
	if r.timed {
		paths = append(paths, r.timeoutDir())
	}
	if r.native {
		paths = append(paths, r.nativeDir())
	}
	return paths
}

func (r *Session) markCleanupDirectory(path string) error {
	body, _ := json.Marshal(r.Runtime.Incarnation)
	return os.WriteFile(path+"/instance.json", body, 0600)
}

// RetireTerminal only retires the matching tmux session, leaving directory
// cleanup to the independently recorded plan. exitedOnly is required by forget.
func (s *Server) RetireTerminal(runtime api.Runtime, exitedOnly bool) error {
	if !wire.ValidID(runtime.ID) || runtime.Incarnation == "" || runtime.Generation == 0 {
		return fmt.Errorf("complete terminal identity required")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	find := func() (string, string, error) {
		rows, err := s.run("list-sessions", "-F", "#{session_name}\t#{session_id}\t#{@dune-runtime}")
		if err != nil {
			if _, statErr := os.Lstat(s.Socket); os.IsNotExist(statErr) {
				return "", "", nil
			}
			return "", "", err
		}
		for _, row := range strings.Split(strings.TrimSuffix(rows, "\n"), "\n") {
			fields := strings.Split(row, "\t")
			if fields[0] != "dune-"+runtime.ID {
				continue
			}
			if len(fields) != 3 || len(fields[1]) < 2 || fields[1][0] != '$' || strings.Trim(fields[1][1:], "0123456789") != "" {
				return "", "", fmt.Errorf("invalid retained terminal identity")
			}
			body, err := base64.RawStdEncoding.DecodeString(fields[2])
			var meta runtimeMetadata
			if err != nil || json.Unmarshal(body, &meta) != nil || meta.ID != runtime.ID || meta.Incarnation != runtime.Incarnation || meta.Generation != runtime.Generation {
				return "", "", &api.Error{Code: "CLEANUP_IDENTITY_CHANGED", Detail: "terminal resource no longer matches its original Runtime"}
			}
			return fields[1], fields[2], nil
		}
		return "", "", nil
	}
	id, metadata, err := find()
	if err != nil || id == "" {
		return err
	}
	condition := "#{==:#{@dune-runtime}," + metadata + "}"
	if exitedOnly {
		condition = "#{&&:" + condition + ",#{pane_dead}}"
	}
	if _, err := s.run("if-shell", "-F", "-t", id, condition, "kill-session -t "+id); err != nil {
		return err
	}
	id, _, err = find()
	if err != nil {
		return err
	}
	if id != "" {
		return &api.Error{Code: "CLEANUP_UNCONFIRMED", Detail: "terminal retirement was not confirmed"}
	}
	return nil
}
