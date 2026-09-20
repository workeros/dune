package fabricd

import (
	"io"
	"sync"

	"github.com/aiomni/dune/internal/wire"
	"github.com/aiomni/dune/pkg/api"
)

// Each pipe has its own fixed ring. Readers copy a bounded page while holding
// this lock; a slow network subscriber never holds the pipe reader.
type rawOutputWindow struct {
	mu     sync.Mutex
	data   []byte
	next   uint64
	closed bool
}

func newRawOutputWindow(limit int) *rawOutputWindow {
	return &rawOutputWindow{data: make([]byte, limit)}
}

func (w *rawOutputWindow) append(data []byte) {
	w.mu.Lock()
	defer w.mu.Unlock()
	end := w.next + uint64(len(data))
	if len(data) > len(w.data) {
		w.next += uint64(len(data) - len(w.data))
		data = data[len(data)-len(w.data):]
	}
	start := int(w.next % uint64(len(w.data)))
	n := copy(w.data[start:], data)
	copy(w.data, data[n:])
	w.next = end
}

func (w *rawOutputWindow) describeLocked() api.RawACPWindow {
	oldest := uint64(0)
	if w.next > uint64(len(w.data)) {
		oldest = w.next - uint64(len(w.data))
	}
	return api.RawACPWindow{Oldest: oldest, Next: w.next, LimitBytes: len(w.data), Closed: w.closed}
}
func (w *rawOutputWindow) window() api.RawACPWindow {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.describeLocked()
}

func (w *rawOutputWindow) consume(reader io.ReadCloser) {
	defer reader.Close()
	defer func() { w.mu.Lock(); w.closed = true; w.mu.Unlock() }()
	buf := make([]byte, wire.ChunkSize)
	for {
		n, err := reader.Read(buf)
		if n > 0 {
			w.append(buf[:n])
		}
		if err != nil {
			return
		}
	}
}

func (a *rawACP) read(request api.RawACPRead) (api.RawACPOutput, error) {
	output := api.RawACPOutput{StreamID: a.id, Channel: request.Channel, Offset: request.Offset, Next: request.Offset}
	if request.StreamID != a.id {
		return output, &api.Error{Code: "STALE_STREAM", Detail: "output belongs to another raw ACP stream"}
	}
	window := a.stdout
	if request.Channel == "stderr" {
		window = a.stderr
	} else if request.Channel != "stdout" {
		return output, &api.Error{Code: "INVALID_ARGUMENT", Detail: "raw output channel must be stdout or stderr"}
	}
	limit := request.MaxBytes
	if limit == 0 {
		limit = api.RawACPMaxReadBytes
	}
	if limit < 1 || limit > api.RawACPMaxReadBytes {
		return output, &api.Error{Code: "INVALID_ARGUMENT", Detail: "raw output read limit must be 1..262144 bytes"}
	}
	window.mu.Lock()
	defer window.mu.Unlock()
	output.Window = window.describeLocked()
	if request.Offset < output.Window.Oldest {
		return output, &api.Error{Code: "STREAM_GAP", Detail: "requested bytes precede the retained window", Payload: api.Payload(output)}
	}
	if request.Offset > window.next {
		return output, &api.Error{Code: "INVALID_ARGUMENT", Detail: "raw output offset is ahead of the original stream"}
	}
	count := min(uint64(limit), window.next-request.Offset)
	output.Data = make([]byte, int(count))
	start := int(request.Offset % uint64(len(window.data)))
	n := copy(output.Data, window.data[start:])
	copy(output.Data[n:], window.data)
	output.Next += count
	return output, nil
}
