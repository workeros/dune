package fabricd

import (
	"testing"
	"time"

	"github.com/aiomni/dune/pkg/api"
)

func TestOrdinaryResultsRejectNewWorkUntilHostRetentionExpires(t *testing.T) {
	l := &operationLog{records: map[string]*operationRecord{}}
	var first, active api.AgentOperation
	for i := range maxRuntimeOperations {
		op, err := l.create()
		if err != nil {
			t.Fatal(err)
		}
		if i == 0 {
			first = op
		}
		if i == maxRuntimeOperations-1 {
			active = op
			continue
		}
		l.append(op.Ref, api.Payload("retained result"))
		l.set(op.Ref, "completed", "end_turn", "")
	}
	_, err := l.create()
	requireSubmissionCode(t, err, "SUBMISSION_CAPACITY_EXHAUSTED")
	usage := l.usage()
	if usage.Records.Used != maxRuntimeOperations || usage.Completed != maxRuntimeOperations-1 || usage.Unfinished != 1 || usage.Updates.Used != maxRuntimeOperations-1 || usage.Bytes.Used <= 0 || usage.NextExpiry == nil || usage.OldestCompletion == nil || usage.NextExpiry.Sub(*usage.OldestCompletion) != operationRetention {
		t.Fatal("incorrect retained-result diagnostics", usage)
	}
	if result, err := l.read(api.AgentOperationRead{Ref: first.Ref}); err != nil || len(result.Output) != 1 {
		t.Fatal("capacity pressure evicted an unexpired result", result, err)
	}
	l.mu.Lock()
	l.records[first.Ref].finished = time.Now().Add(-operationRetention)
	l.mu.Unlock()
	if _, err := l.create(); err != nil {
		t.Fatal("expired result did not release its slot", err)
	}
	if _, err := l.read(api.AgentOperationRead{Ref: first.Ref}); err == nil {
		t.Fatal("expired result still retained")
	}
	if result, err := l.wait(t.Context(), api.AgentOperationWait{Ref: active.Ref}); err != nil || result.State != "pending" {
		t.Fatal("retention clock expired active work", result, err)
	}
}

func TestPTYDeliveryReceiptsKeepExistingPressureEviction(t *testing.T) {
	r := &runtime{adapter: "pty"}
	l := r.operationLog()
	first, err := l.create()
	if err != nil {
		t.Fatal(err)
	}
	l.set(first.Ref, "delivered", "", "")
	for range maxRuntimeOperations - 1 {
		if _, err := l.create(); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := l.create(); err != nil {
		t.Fatal("completed PTY delivery blocked existing input behavior", err)
	}
	if _, err := l.wait(t.Context(), api.AgentOperationWait{Ref: first.Ref}); err == nil {
		t.Fatal("oldest completed PTY receipt was not evicted")
	}
	_, err = l.create()
	requireSubmissionCode(t, err, "RESOURCE_EXHAUSTED")
}
