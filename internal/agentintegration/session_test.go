package agentintegration

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"golang.org/x/sys/unix"
)

func sessionFixture(t *testing.T, agent string) (string, Binding) {
	t.Helper()
	dir := t.TempDir()
	if err := os.Chmod(dir, 0700); err != nil {
		t.Fatal(err)
	}
	binding := Binding{RuntimeID: "runtime", Incarnation: "instance", Agent: agent, Version: "test"}
	if err := Prepare(dir, binding); err != nil {
		t.Fatal(err)
	}
	return dir, binding
}

func event(id, cwd string) string {
	value, _ := json.Marshal(map[string]string{"hook_event_name": "SessionStart", "session_id": id, "cwd": cwd, "transcript_path": "/private/native.jsonl"})
	return string(value)
}

func TestSessionReceiptsSurviveReadersAndDeduplicate(t *testing.T) {
	dir, binding := sessionFixture(t, "claude")
	if _, err := Read(dir, binding.RuntimeID, binding.Incarnation); !os.IsNotExist(err) {
		t.Fatal("a requested identity was published before confirmation", err)
	}
	a, b := uuid.NewString(), uuid.NewString()
	for _, value := range []struct {
		id, cwd  string
		sequence int64
	}{{a, "/first", 1}, {a, "/first", 1}, {b, "/second", 2}, {a, "/first", 3}, {a, "/changed", 4}} {
		if err := Report(t.Context(), dir, "", strings.NewReader(event(value.id, value.cwd))); err != nil {
			t.Fatal(err)
		}
		// Read reopens files just as a newly started fabricd does.
		native, err := Read(dir, binding.RuntimeID, binding.Incarnation)
		if err != nil || native.ID != value.id || native.Cwd != value.cwd || native.Sequence != value.sequence || native.Source != "pty-session-start" || !native.ResumeSupported {
			t.Fatal(native, err)
		}
	}
	if _, err := Read(dir, "replacement", binding.Incarnation); err == nil {
		t.Fatal("receipt crossed Runtime identity")
	}
	if _, err := Read(dir, binding.RuntimeID, "replacement"); err == nil {
		t.Fatal("receipt crossed Runtime incarnation")
	}
	if err := Prepare(dir, binding); err == nil {
		t.Fatal("replaced existing hook binding")
	}
	data, _ := os.ReadFile(filepath.Join(dir, "session.json"))
	if strings.Contains(string(data), "native.jsonl") {
		t.Fatal("saved private transcript path")
	}
	info, _ := os.Stat(filepath.Join(dir, "session.json"))
	if info.Mode().Perm() != 0600 {
		t.Fatal("receipt permissions", info.Mode())
	}
}

func TestSessionHookRejectsOtherEventsAndNestedAgents(t *testing.T) {
	for _, agent := range []string{"claude", "codex"} {
		t.Run(agent, func(t *testing.T) {
			dir, binding := sessionFixture(t, agent)
			id := uuid.NewString()
			for _, input := range []string{
				strings.Replace(event(id, "/src"), "SessionStart", "Stop", 1),
				strings.Replace(event(id, "/src"), "{", `{"agent_id":"child",`, 1),
				strings.Replace(event(id, "/src"), "{", `{"cursor_version":"1",`, 1),
			} {
				if err := Report(t.Context(), dir, "", strings.NewReader(input)); err != nil {
					t.Fatal(err)
				}
			}
			if agent == "codex" {
				if err := Report(t.Context(), dir, "parent", strings.NewReader(event(id, "/src"))); err != nil {
					t.Fatal(err)
				}
			}
			if _, err := Read(dir, binding.RuntimeID, binding.Incarnation); !os.IsNotExist(err) {
				t.Fatal("irrelevant hook published a native identity", err)
			}
			for _, input := range []string{event("latest", "/src"), event(id, "relative"), event(id, "/bad\npath"), "{", event(id, "/src") + "{}", strings.Repeat(" ", 64*1024+1)} {
				if err := Report(t.Context(), dir, "", strings.NewReader(input)); err == nil {
					t.Fatal("invalid input accepted")
				}
			}
		})
	}
}

func TestSessionHookConcurrentWritersAndBoundedLock(t *testing.T) {
	dir, binding := sessionFixture(t, "codex")
	var wg sync.WaitGroup
	for range 12 {
		wg.Go(func() {
			if err := Report(t.Context(), dir, "", strings.NewReader(event(uuid.NewString(), "/src"))); err != nil {
				t.Error(err)
			}
		})
	}
	wg.Wait()
	native, err := Read(dir, binding.RuntimeID, binding.Incarnation)
	if err != nil || native.Sequence != 12 {
		t.Fatal("lost a concurrent confirmation", native, err)
	}
	lock, err := os.OpenFile(filepath.Join(dir, "session.lock"), os.O_RDWR, 0600)
	if err != nil {
		t.Fatal(err)
	}
	defer lock.Close()
	if err := unix.Flock(int(lock.Fd()), unix.LOCK_EX); err != nil {
		t.Fatal(err)
	}
	defer unix.Flock(int(lock.Fd()), unix.LOCK_UN)
	ctx, cancel := context.WithTimeout(t.Context(), 30*time.Millisecond)
	defer cancel()
	if err := Report(ctx, dir, "", strings.NewReader(event(uuid.NewString(), "/src"))); err != context.DeadlineExceeded {
		t.Fatal("hook did not honor bounded lock wait", err)
	}
}

func TestSessionHookDoesNotResetCorruptionOrRecreateRemovedDirectory(t *testing.T) {
	dir, binding := sessionFixture(t, "claude")
	if err := os.WriteFile(filepath.Join(dir, "session.json"), []byte("broken"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := Report(t.Context(), dir, "", strings.NewReader(event(uuid.NewString(), "/src"))); err == nil {
		t.Fatal("corruption reset the sequence")
	}
	if err := os.RemoveAll(dir); err != nil {
		t.Fatal(err)
	}
	if err := Report(t.Context(), dir, "", strings.NewReader(event(uuid.NewString(), "/src"))); !os.IsNotExist(err) {
		t.Fatal(err)
	}
	if err := Prepare(dir, binding); !os.IsNotExist(err) {
		t.Fatal("hook context recreated retired directory", err)
	}
}
