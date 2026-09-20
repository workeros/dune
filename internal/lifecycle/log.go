// Package lifecycle records bounded, best-effort process diagnostics. Durable
// admission and cleanup truth belongs to sessionregistry, never to this log.
package lifecycle

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/aiomni/dune/internal/launchgate"
)

const MaxBytes = 64 * 1024
const QueueSize = 64

// Entry deliberately has no free-form detail, request body or environment.
// Identifiers are generated Runtime/host/operation references, not caller keys.
type Entry struct {
	At                 time.Time `json:"at"`
	Kind               string    `json:"kind"`
	RuntimeID          string    `json:"runtime_id,omitempty"`
	RuntimeIncarnation string    `json:"runtime_incarnation,omitempty"`
	HostInstance       string    `json:"host_instance,omitempty"`
	OperationRef       string    `json:"operation_ref,omitempty"`
	ConnectorTerm      uint64    `json:"connector_term,omitempty"`
	Code               string    `json:"code,omitempty"`
	Count              uint64    `json:"count,omitempty"`
	Dropped            uint64    `json:"dropped,omitempty"`
}

type Usage struct {
	Bytes       int64  `json:"bytes"`
	ByteLimit   int    `json:"byte_limit"`
	Queued      int    `json:"queued"`
	QueueLimit  int    `json:"queue_limit"`
	Dropped     uint64 `json:"dropped"`
	WriteErrors uint64 `json:"write_errors"`
}

type Log struct {
	file        *os.File
	queue       chan Entry
	stop, done  chan struct{}
	once        sync.Once
	closed      atomic.Bool
	bytes       atomic.Int64
	dropped     atomic.Uint64
	writeErrors atomic.Uint64
	reset       bool // writer-owned after an incomplete file write
}

func Open(directory, name string) (*Log, error) {
	if name == "." || filepath.Base(name) != name || name == "" {
		return nil, fmt.Errorf("lifecycle log requires a local filename")
	}
	if err := launchgate.CheckDirectory(directory); err != nil {
		return nil, err
	}
	root, err := os.OpenRoot(directory)
	if err != nil {
		return nil, err
	}
	defer root.Close()
	f, err := root.OpenFile(name, os.O_CREATE|os.O_RDWR|os.O_APPEND|syscall.O_NOFOLLOW|syscall.O_NONBLOCK, 0600)
	if err != nil {
		return nil, err
	}
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		f.Close()
		return nil, fmt.Errorf("lifecycle log already has a writer: %w", err)
	}
	info, err := f.Stat()
	if err == nil {
		owner, ok := info.Sys().(*syscall.Stat_t)
		if !info.Mode().IsRegular() || info.Mode().Perm() != 0600 || !ok || int(owner.Uid) != os.Getuid() || owner.Nlink != 1 || info.Size() > MaxBytes {
			err = fmt.Errorf("lifecycle log must be a bounded private file")
		}
	}
	if err != nil {
		f.Close()
		return nil, err
	}
	l := &Log{file: f, queue: make(chan Entry, QueueSize), stop: make(chan struct{}), done: make(chan struct{})}
	l.bytes.Store(info.Size())
	if info.Size() > 0 {
		var last [1]byte
		_, err := f.ReadAt(last[:], info.Size()-1)
		l.reset = err != nil || last[0] != '\n'
	}
	go l.run()
	return l, nil
}

func label(value string) bool {
	return len(value) <= 128 && strings.Trim(value, "abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQRSTUVWXYZ0123456789_-.:/") == ""
}

// Record never waits for disk or a consumer. Overflow is explicit in Usage and
// subsequent entries. Log pressure cannot delay Agent I/O, admission or control.
func (l *Log) Record(entry Entry) {
	if l == nil {
		return
	}
	valid := entry.Kind != ""
	for _, value := range []string{entry.Kind, entry.RuntimeID, entry.RuntimeIncarnation, entry.HostInstance, entry.OperationRef, entry.Code} {
		valid = valid && label(value)
	}
	if !valid || l.closed.Load() {
		l.dropped.Add(1)
		return
	}
	entry.At = time.Now().UTC()
	select {
	case l.queue <- entry:
	default:
		l.dropped.Add(1)
	}
}

func (l *Log) Usage() Usage {
	if l == nil {
		return Usage{}
	}
	return Usage{Bytes: l.bytes.Load(), ByteLimit: MaxBytes, Queued: len(l.queue), QueueLimit: QueueSize, Dropped: l.dropped.Load(), WriteErrors: l.writeErrors.Load()}
}

func (l *Log) Close() {
	if l == nil {
		return
	}
	l.once.Do(func() { l.closed.Store(true); close(l.stop) })
	<-l.done
}

func (l *Log) run() {
	defer close(l.done)
	defer l.file.Close()
	for {
		select {
		case entry := <-l.queue:
			l.write(entry)
		case <-l.stop:
			for {
				select {
				case entry := <-l.queue:
					l.write(entry)
				default:
					return
				}
			}
		}
	}
}

func (l *Log) write(entry Entry) {
	entry.Dropped = l.dropped.Load()
	body, err := json.Marshal(entry)
	if err != nil || len(body) >= 1536 {
		l.dropped.Add(1)
		return
	}
	body = append(body, '\n')
	if l.reset || l.bytes.Load()+int64(len(body)) > MaxBytes {
		if err := l.file.Truncate(0); err != nil {
			l.writeErrors.Add(1)
			return
		}
		l.bytes.Store(0)
		l.reset = false
		reset, _ := json.Marshal(Entry{At: entry.At, Kind: "log_window_reset"})
		body = append(append(reset, '\n'), body...)
	}
	n, err := l.file.Write(body)
	l.bytes.Add(int64(n))
	if err != nil || n != len(body) {
		l.writeErrors.Add(1)
		// The next successful append starts a new complete JSON window instead
		// of concatenating onto an unconfirmed partial diagnostic entry.
		l.reset = true
	}
}
