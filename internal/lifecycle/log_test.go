package lifecycle

import (
	"bufio"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func privateLog(t *testing.T) (*Log, string) {
	t.Helper()
	dir := t.TempDir()
	if err := os.Chmod(dir, 0700); err != nil {
		t.Fatal(err)
	}
	l, err := Open(dir, "events.jsonl")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(l.Close)
	return l, filepath.Join(dir, "events.jsonl")
}

func TestLifecycleLogBoundsDiskQueueAndRejectsBodies(t *testing.T) {
	l, path := privateLog(t)
	for i := range 1500 {
		l.Record(Entry{Kind: "admission_receipt", RuntimeID: strings.Repeat("a", 32), OperationRef: strings.Repeat("b", 32), ConnectorTerm: uint64(i)})
		// Drain each batch to exercise disk rotation independently of overflow.
		if i%32 == 0 {
			for deadline := time.Now().Add(time.Second); len(l.queue) != 0 && time.Now().Before(deadline); {
				time.Sleep(time.Millisecond)
			}
		}
	}
	l.Record(Entry{Kind: "arbitrary prompt\nsecret body"})
	l.Close()
	usage := l.Usage()
	if usage.Bytes > MaxBytes || usage.WriteErrors != 0 || usage.Dropped == 0 {
		t.Fatal(usage)
	}
	data, err := os.ReadFile(path)
	if err != nil || len(data) > MaxBytes || !strings.Contains(string(data), "log_window_reset") || strings.Contains(string(data), "secret body") {
		t.Fatal(len(data), err)
	}
	scanner := bufio.NewScanner(strings.NewReader(string(data)))
	for scanner.Scan() {
		var entry Entry
		if json.Unmarshal(scanner.Bytes(), &entry) != nil || entry.At.IsZero() || entry.Kind == "" {
			t.Fatal("incomplete log entry", scanner.Text())
		}
	}
	if err := scanner.Err(); err != nil {
		t.Fatal(err)
	}
	if _, err := Open(filepath.Dir(path), "../escape"); err == nil {
		t.Fatal("accepted escaping path")
	}
	if err := os.Symlink(path, filepath.Join(filepath.Dir(path), "symlink")); err != nil {
		t.Fatal(err)
	}
	if _, err := Open(filepath.Dir(path), "symlink"); err == nil {
		t.Fatal("followed log symlink")
	}
}

func TestLifecycleLogSaturationDoesNotWaitForConsumer(t *testing.T) {
	// No writer is started. Only the 64-entry channel can accept diagnostics.
	l := &Log{queue: make(chan Entry, QueueSize)}
	for range 10000 {
		l.Record(Entry{Kind: "attach"})
	}
	if usage := l.Usage(); usage.Queued != QueueSize || usage.Dropped != 10000-QueueSize {
		t.Fatal(usage)
	}
}

func TestLifecycleLogReopenSealsTruncatedDiagnostic(t *testing.T) {
	l, path := privateLog(t)
	l.Close()
	if err := os.WriteFile(path, []byte(`{"kind":"partial`), 0600); err != nil {
		t.Fatal(err)
	}
	l, err := Open(filepath.Dir(path), filepath.Base(path))
	if err != nil {
		t.Fatal(err)
	}
	l.Record(Entry{Kind: "connector_started"})
	l.Close()
	data, err := os.ReadFile(path)
	if err != nil || strings.Contains(string(data), "partial") || !strings.Contains(string(data), "log_window_reset") {
		t.Fatal(string(data), err)
	}
}
