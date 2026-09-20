package fabricd

import (
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"syscall"

	"github.com/aiomni/dune/internal/tmux"
	"github.com/aiomni/dune/internal/wire"
	"github.com/aiomni/dune/pkg/api"
)

const sessionProtocol = 1

// Registration is discovery evidence, never execution authority. A matching
// live IPC handshake is required before the connector calls this Runtime.
type sessionRegistration struct {
	Target       api.SubmissionTarget `json:"target"`
	Version      int                  `json:"version"`
	Installation string               `json:"installation"`
	Machine      string               `json:"machine"`
	Instance     string               `json:"instance"`
	Runtime      api.Runtime          `json:"runtime"`
}

type sessionBootstrap struct {
	Launch       api.SubmissionReceipt `json:"launch"`
	Registration sessionRegistration   `json:"registration"`
	StateDir     string                `json:"state_dir"`
	Profile      api.Profile           `json:"profile"`
	Environment  []string              `json:"environment"`
}

func installationID(stateDir string) string {
	return fmt.Sprintf("%x", sha256.Sum256([]byte(filepath.Clean(stateDir))))
}

func sessionSocket(reg sessionRegistration) (string, error) {
	if reg.Target.Validate() != nil || reg.Target.RuntimeID != reg.Runtime.ID || reg.Target.RuntimeIncarnation != reg.Runtime.Incarnation || reg.Target.RuntimeGeneration != reg.Runtime.Generation || reg.Target.MachineID != reg.Machine || !wire.ValidID(reg.Instance) || !wire.ValidID(reg.Runtime.ID) || !wire.ValidID(reg.Runtime.Incarnation) || reg.Runtime.Generation != 1 || reg.Version != sessionProtocol || len(reg.Installation) != 64 || reg.Machine == "" {
		return "", fmt.Errorf("invalid ACP host registration")
	}
	root := filepath.Join("/tmp", fmt.Sprintf("dune-acp-%d", os.Getuid()))
	if err := tmux.PrivateDir(root); err != nil {
		return "", err
	}
	key := installationID(reg.Installation + "/" + reg.Instance)
	return filepath.Join(root, key[:32]), nil
}

func privateFile(path string, limit int, value any) error {
	f, err := os.OpenFile(path, os.O_RDONLY|syscall.O_NOFOLLOW, 0)
	if err != nil {
		return err
	}
	defer f.Close()
	st, err := f.Stat()
	if err != nil {
		return err
	}
	owner, ok := st.Sys().(*syscall.Stat_t)
	if !st.Mode().IsRegular() || st.Mode().Perm()&0077 != 0 || !ok || int(owner.Uid) != os.Getuid() || owner.Nlink != 1 || st.Size() > int64(limit) {
		return fmt.Errorf("unsafe private session file")
	}
	body, err := io.ReadAll(io.LimitReader(f, int64(limit)+1))
	if err != nil || len(body) > limit {
		return fmt.Errorf("private session file exceeds limit")
	}
	return json.Unmarshal(body, value)
}

func savePrivateFile(path string, value any) error {
	data, err := json.Marshal(value)
	if err != nil {
		return err
	}
	file, err := os.CreateTemp(filepath.Dir(path), ".pending-")
	if err != nil {
		return err
	}
	defer os.Remove(file.Name())
	if _, err = file.Write(data); err == nil {
		err = file.Sync()
	}
	if closeErr := file.Close(); err == nil {
		err = closeErr
	}
	if err == nil {
		err = os.Rename(file.Name(), path)
	}
	if err != nil {
		return err
	}
	directory, err := os.Open(filepath.Dir(path))
	if err != nil {
		return err
	}
	defer directory.Close()
	return directory.Sync()
}

// The engine holds fabricd.lock before allocating its durable control term.
// A delayed connector with a smaller term can never reclaim a surviving host.
func nextSessionTerm(stateDir string) (uint64, error) {
	path := filepath.Join(stateDir, "acp-control-term.json")
	var term uint64
	if err := privateFile(path, 64, &term); err != nil && !os.IsNotExist(err) {
		return 0, err
	}
	if term == ^uint64(0) {
		return 0, fmt.Errorf("ACP control term exhausted")
	}
	term++
	return term, savePrivateFile(path, term)
}
