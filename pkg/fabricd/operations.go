package fabricd

import (
	"context"
	"encoding/json"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/aiomni/dune/internal/wire"
	"github.com/aiomni/dune/pkg/api"
)

const (
	maxRuntimeOperations = 64
	maxOperationBytes    = 512 * 1024
	maxOperationUpdates  = 1024
	operationRetention   = 15 * time.Minute
)

type operationRecord struct {
	status      api.AgentOperation
	output      []json.RawMessage
	bytes       int
	start, next int64
	incomplete  bool
	finished    time.Time
	done        chan struct{}
}

// operationLog owns only bounded result retention, independent of transports
// and scheduling. References belong to this Runtime and survive connector replacement.
type operationLog struct {
	mu      sync.Mutex
	records map[string]*operationRecord
	// PTY delivery receipts keep their existing pressure eviction policy;
	// managed ACP reserves unexpired task results until their completion TTL.
	evictCompletedOnCapacity bool
}

func (r *runtime) operationLog() *operationLog {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.operations == nil {
		r.operations = &operationLog{records: map[string]*operationRecord{}, evictCompletedOnCapacity: r.adapter == "pty"}
	}
	return r.operations
}

func (l *operationLog) expireLocked(now time.Time) {
	for ref, record := range l.records {
		if !record.finished.IsZero() && now.Sub(record.finished) >= operationRetention {
			delete(l.records, ref)
		}
	}
}

func (l *operationLog) create() (api.AgentOperation, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.expireLocked(time.Now())
	if len(l.records) >= maxRuntimeOperations {
		if l.evictCompletedOnCapacity {
			var oldest string
			for ref, record := range l.records {
				if !record.finished.IsZero() && (oldest == "" || record.finished.Before(l.records[oldest].finished)) {
					oldest = ref
				}
			}
			if oldest == "" {
				return api.AgentOperation{}, &api.Error{Code: "RESOURCE_EXHAUSTED", Detail: "Runtime operation capacity reached"}
			}
			delete(l.records, oldest)
		} else {
			return api.AgentOperation{}, &api.Error{Code: "SUBMISSION_CAPACITY_EXHAUSTED", Detail: "ordinary operation results capacity reached; unexpired results remain retained"}
		}
	}
	status := api.AgentOperation{Ref: wire.ID(), State: "pending"}
	l.records[status.Ref] = &operationRecord{status: status, done: make(chan struct{})}
	return status, nil
}

func (l *operationLog) usage() api.ACPOperationUsage {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.expireLocked(time.Now())
	out := api.ACPOperationUsage{
		Records: api.CapacityUsage{Used: len(l.records), Limit: maxRuntimeOperations},
		Bytes:   api.CapacityUsage{Limit: maxRuntimeOperations * maxOperationBytes},
		Updates: api.CapacityUsage{Limit: maxRuntimeOperations * maxOperationUpdates},
	}
	for _, record := range l.records {
		out.Bytes.Used += record.bytes
		out.Updates.Used += len(record.output)
		if record.finished.IsZero() {
			out.Unfinished++
		} else {
			out.Completed++
			if out.OldestCompletion == nil || record.finished.Before(*out.OldestCompletion) {
				completed := record.finished
				expiry := completed.Add(operationRetention)
				out.OldestCompletion, out.NextExpiry = &completed, &expiry
			}
		}
	}
	return out
}

func (l *operationLog) set(ref, state, reason, detail string) api.AgentOperation {
	l.mu.Lock()
	defer l.mu.Unlock()
	record := l.records[ref]
	if record == nil || record.status.Terminal() {
		return api.AgentOperation{}
	}
	if len(detail) > 4096 {
		end := 4096
		for !utf8.ValidString(detail[:end]) {
			end--
		}
		detail = detail[:end] + " [truncated]"
	}
	record.status.State, record.status.StopReason, record.status.Error = state, reason, detail
	if record.status.Terminal() {
		record.finished = time.Now()
		close(record.done)
	}
	return record.status
}

func (l *operationLog) append(ref string, update json.RawMessage) {
	l.mu.Lock()
	defer l.mu.Unlock()
	record := l.records[ref]
	if record == nil || record.status.Terminal() {
		return
	}
	record.next++
	if len(update) > maxOperationBytes {
		// Keep one contiguous retained range. A skipped event is a visible gap.
		record.output, record.bytes = nil, 0
		record.start, record.incomplete = record.next, true
		return
	}
	record.output = append(record.output, append(json.RawMessage(nil), update...))
	record.bytes += len(update)
	for record.bytes > maxOperationBytes || len(record.output) > maxOperationUpdates {
		record.bytes -= len(record.output[0])
		record.output[0] = nil
		record.output = record.output[1:]
		record.start++
		record.incomplete = true
	}
}

func (l *operationLog) markIncomplete(ref string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if record := l.records[ref]; record != nil {
		record.incomplete = true
	}
}

func (l *operationLog) findLocked(ref string) (*operationRecord, error) {
	l.expireLocked(time.Now())
	if record := l.records[ref]; record != nil {
		return record, nil
	}
	return nil, &api.Error{Code: "OPERATION_EXPIRED", Detail: "operation is absent, expired, or belongs to another Runtime"}
}

func (l *operationLog) wait(ctx context.Context, request api.AgentOperationWait) (api.AgentOperation, error) {
	if request.TimeoutMS < 0 || request.TimeoutMS > 30000 {
		return api.AgentOperation{}, &api.Error{Code: "INVALID_ARGUMENT", Detail: "timeout_ms must be 0..30000"}
	}
	l.mu.Lock()
	record, err := l.findLocked(request.Ref)
	if err != nil {
		l.mu.Unlock()
		return api.AgentOperation{}, err
	}
	status, done := record.status, record.done
	l.mu.Unlock()
	if !status.Terminal() && request.TimeoutMS > 0 {
		timer := time.NewTimer(time.Duration(request.TimeoutMS) * time.Millisecond)
		defer timer.Stop()
		select {
		case <-ctx.Done():
			return api.AgentOperation{}, ctx.Err()
		case <-timer.C:
		case <-done:
		}
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	record, err = l.findLocked(request.Ref)
	if err != nil {
		return api.AgentOperation{}, err
	}
	return record.status, nil
}

func (l *operationLog) read(request api.AgentOperationRead) (api.AgentOperationOutput, error) {
	if request.Position < 0 || request.Limit < 0 || request.Limit > 128 {
		return api.AgentOperationOutput{}, &api.Error{Code: "INVALID_ARGUMENT", Detail: "position must be nonnegative; limit must be 0..128"}
	}
	if request.Limit == 0 {
		request.Limit = 128
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	record, err := l.findLocked(request.Ref)
	if err != nil {
		return api.AgentOperationOutput{}, err
	}
	if request.Position > record.next {
		return api.AgentOperationOutput{}, &api.Error{Code: "INVALID_ARGUMENT", Detail: "position is beyond this operation's output"}
	}
	position := max(request.Position, record.start)
	end := min(record.next, position+int64(request.Limit))
	result := api.AgentOperationOutput{AgentOperation: record.status, Position: position, NextPosition: end, Incomplete: record.incomplete, Output: make([]json.RawMessage, 0, end-position)}
	for _, update := range record.output[position-record.start : end-record.start] {
		result.Output = append(result.Output, append(json.RawMessage(nil), update...))
	}
	return result, nil
}
