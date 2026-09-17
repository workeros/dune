// Package agentintegration contains the process-local plumbing used by native
// PTY Agent hooks. Application identities and recovery records stay in the host.
package agentintegration

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/aiomni/dune/pkg/api"
	"github.com/google/uuid"
	"golang.org/x/sys/unix"
)

const SessionCommand = "__agent-session"
const SessionDirEnv = "DUNE_AGENT_SESSION_DIR"

// Binding is private Runner metadata, created before launching the native CLI.
// The hook never accepts a Runtime identity from the Agent's JSON event.
type Binding struct {
	RuntimeID   string `json:"runtime_id"`
	Incarnation string `json:"incarnation"`
	Agent       string `json:"agent"`
	Version     string `json:"version,omitempty"`
}

type receipt struct {
	Binding
	Native api.NativeSession `json:"native"`
}

// Prepare creates an exclusive context in an already-created private directory.
// Replacing a context would let an old process report for a new Runtime.
func Prepare(dir string, binding Binding) error {
	if !validBinding(binding) {
		return fmt.Errorf("invalid native Agent binding")
	}
	root, err := privateRoot(dir)
	if err != nil {
		return err
	}
	defer root.Close()
	lock, err := root.OpenFile("session.lock", os.O_CREATE|os.O_EXCL|os.O_RDWR, 0600)
	if err != nil {
		return err
	}
	if err := lock.Close(); err != nil {
		return err
	}
	f, err := root.OpenFile("binding.json", os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
	if err != nil {
		return err
	}
	err = json.NewEncoder(f).Encode(binding)
	return errors.Join(err, f.Close())
}

// Report consumes only SessionStart identity fields, never prompt or transcript
// contents. It produces no model-facing output and does not contact the host.
// A local file lock serializes concurrent hook processes across fabricd restarts.
func Report(ctx context.Context, dir, inheritedThread string, input io.Reader) error {
	root, err := privateRoot(dir)
	if err != nil {
		return err
	}
	defer root.Close()
	var binding Binding
	if err := readJSON(root, "binding.json", &binding); err != nil || !validBinding(binding) {
		return fmt.Errorf("native Agent binding unavailable")
	}
	data, err := io.ReadAll(io.LimitReader(input, 64*1024+1))
	if err != nil || len(data) > 64*1024 {
		return fmt.Errorf("native hook input exceeds limit or cannot be read")
	}
	var event struct {
		Event      string `json:"hook_event_name"`
		SessionID  string `json:"session_id"`
		Cwd        string `json:"cwd"`
		Transcript string `json:"transcript_path"`
		AgentID    string `json:"agent_id"`
		Cursor     string `json:"cursor_version"`
	}
	if json.Unmarshal(data, &event) != nil {
		return fmt.Errorf("invalid native hook event")
	}
	if event.Event != "SessionStart" || event.AgentID != "" || event.Cursor != "" {
		return nil
	}
	if binding.Agent == "codex" && inheritedThread != "" && inheritedThread != event.SessionID {
		return nil // A nested Agent must not replace its parent's native identity.
	}
	if !validSessionID(event.SessionID) || !validPath(event.Cwd) || !validPath(event.Transcript) {
		return fmt.Errorf("native SessionStart requires an exact ID, cwd and transcript path")
	}
	lock, err := root.OpenFile("session.lock", os.O_RDWR, 0600)
	if err != nil {
		return err
	}
	defer lock.Close()
	if err := acquire(ctx, lock); err != nil {
		return err
	}
	defer unix.Flock(int(lock.Fd()), unix.LOCK_UN)
	var previous receipt
	err = readJSON(root, "session.json", &previous)
	if err != nil && !os.IsNotExist(err) {
		return err // Corruption must not reset the confirmation sequence.
	}
	if err == nil && (previous.Binding != binding || !validNative(previous.Native) || previous.Native.AgentVersion != binding.Version) {
		return fmt.Errorf("native session receipt does not match its Runtime")
	}
	if previous.Native.ID == event.SessionID && previous.Native.Cwd == event.Cwd {
		return nil
	}
	if previous.Native.Sequence == math.MaxInt64 {
		return fmt.Errorf("native session sequence exhausted")
	}
	native := api.NativeSession{ID: event.SessionID, Cwd: event.Cwd, Sequence: previous.Native.Sequence + 1,
		Source: "pty-session-start", AgentVersion: binding.Version, ResumeSupported: true}
	return writeReceipt(root, receipt{Binding: binding, Native: native})
}

// Read returns only the trusted fields consumed by fabricd. The exact transcript
// path is deliberately not saved: recovery uses the CLI's native resume command.
func Read(dir, id, incarnation string) (*api.NativeSession, error) {
	root, err := privateRoot(dir)
	if err != nil {
		return nil, err
	}
	defer root.Close()
	var value receipt
	if err := readJSON(root, "session.json", &value); err != nil {
		return nil, err
	}
	if !validBinding(value.Binding) || value.RuntimeID != id || value.Incarnation != incarnation || !validNative(value.Native) || value.Native.AgentVersion != value.Version {
		return nil, fmt.Errorf("invalid native session receipt")
	}
	return &value.Native, nil
}

func validBinding(b Binding) bool {
	return text(b.RuntimeID, 128) && text(b.Incarnation, 128) && (b.Agent == "claude" || b.Agent == "codex") && (b.Version == "" || text(b.Version, 256))
}

func validNative(n api.NativeSession) bool {
	return validSessionID(n.ID) && validPath(n.Cwd) && n.Sequence > 0 && n.Source == "pty-session-start" && n.ResumeSupported
}

func validSessionID(id string) bool {
	parsed, err := uuid.Parse(id)
	return err == nil && parsed != uuid.Nil && parsed.String() == id
}

func validPath(path string) bool { return filepath.IsAbs(path) && text(path, 4096) }

func text(value string, limit int) bool {
	return value != "" && len(value) <= limit && utf8.ValidString(value) && !strings.ContainsFunc(value, unicode.IsControl)
}

func privateRoot(dir string) (*os.Root, error) {
	if !filepath.IsAbs(dir) {
		return nil, fmt.Errorf("native Agent directory must be absolute")
	}
	info, err := os.Lstat(dir)
	if err != nil {
		return nil, err
	}
	if !info.IsDir() || info.Mode().Perm()&0077 != 0 {
		return nil, fmt.Errorf("native Agent directory must be private")
	}
	if stat, ok := info.Sys().(*syscall.Stat_t); !ok || int(stat.Uid) != os.Getuid() {
		return nil, fmt.Errorf("native Agent directory belongs to another user")
	}
	return os.OpenRoot(dir)
}

func readJSON(root *os.Root, name string, value any) error {
	f, err := root.Open(name)
	if err != nil {
		return err
	}
	defer f.Close()
	data, err := io.ReadAll(io.LimitReader(f, 16*1024+1))
	if err != nil || len(data) > 16*1024 {
		return fmt.Errorf("native Agent record exceeds limit or cannot be read")
	}
	return json.Unmarshal(data, value)
}

func writeReceipt(root *os.Root, value receipt) error {
	name := ".session-" + uuid.NewString()
	f, err := root.OpenFile(name, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
	if err != nil {
		return err
	}
	defer root.Remove(name)
	err = json.NewEncoder(f).Encode(value)
	if err = errors.Join(err, f.Close()); err != nil {
		return err
	}
	return root.Rename(name, "session.json")
}

func acquire(ctx context.Context, f *os.File) error {
	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		err := unix.Flock(int(f.Fd()), unix.LOCK_EX|unix.LOCK_NB)
		if err == nil {
			return nil
		}
		if !errors.Is(err, unix.EWOULDBLOCK) && !errors.Is(err, unix.EINTR) {
			return err
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(10 * time.Millisecond):
		}
	}
}
