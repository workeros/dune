package fabricd

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	goruntime "runtime"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/aiomni/dune/pkg/api"
)

// Opt-in sustained churn complements the burst/latency benchmark. Each round
// quiesces workers before GC so later rounds reveal retained heap/index growth.
func TestConversationSustainedResourceRetention(t *testing.T) {
	if os.Getenv("DUNE_CONVERSATION_RESOURCE_TEST") != "1" {
		t.Skip("set DUNE_CONVERSATION_RESOURCE_TEST=1 for resource measurement")
	}
	const models, readers, rounds = 20, 8, 3
	const roundDuration = 10 * time.Second
	store := newConversationStore()
	slots := make([]*conversationSlot, models)
	for i := range slots {
		slots[i] = store.register(fmt.Sprint(i), "inc", nil)
		slots[i].begin(api.ACPAction{Action: "load", Cwd: "/work"})
	}
	body := api.Payload(strings.Repeat("x", 64*1024))
	var sequence atomic.Uint64
	lastEvictions := uint64(0)
	for round := 1; round <= rounds; round++ {
		ctx, cancel := context.WithTimeout(t.Context(), roundDuration)
		var writers, reading sync.WaitGroup
		writeCounts, readCounts := make([]int, models), make([]int, readers)
		var maxResponse atomic.Uint64
		for i, slot := range slots {
			writers.Go(func() {
				tick := time.NewTicker(5 * time.Millisecond)
				defer tick.Stop()
				for {
					select {
					case <-ctx.Done():
						return
					case <-tick.C:
					}
					// Each input is independently owned, as in decoded protocol messages.
					slot.update(map[string]json.RawMessage{"sessionUpdate": api.Payload("tool_call"), "toolCallId": api.Payload(fmt.Sprintf("tool-%d", sequence.Add(1))), "rawOutput": append(json.RawMessage(nil), body...)}, "")
					writeCounts[i]++
				}
			})
		}
		for i := 0; i < readers; i++ {
			reading.Go(func() {
				tick := time.NewTicker(10 * time.Millisecond)
				defer tick.Stop()
				for {
					select {
					case <-ctx.Done():
						return
					case <-tick.C:
					}
					slot := slots[(i+readCounts[i])%models]
					page, err := slot.read(api.ACPConversationRead{ConversationID: slot.describe().ID})
					if err != nil {
						t.Error(err)
						return
					}
					encoded := api.Payload(page)
					bytes := uint64(len(encoded))
					for previous := maxResponse.Load(); bytes > previous && !maxResponse.CompareAndSwap(previous, bytes); previous = maxResponse.Load() {
					}
					if bytes > api.MaxACPConversationResponseBytes {
						t.Error("response budget exceeded")
						return
					}
					readCounts[i]++
				}
			})
		}
		writers.Wait()
		reading.Wait()
		cancel()
		minWrites, minReads, totalWrites, totalReads := writeCounts[0], readCounts[0], 0, 0
		for _, count := range writeCounts {
			minWrites = min(minWrites, count)
			totalWrites += count
		}
		for _, count := range readCounts {
			minReads = min(minReads, count)
			totalReads += count
		}
		if minWrites == 0 || minReads == 0 {
			t.Fatal("sustained workload starved a worker", writeCounts, readCounts)
		}
		usage := store.statistics()
		if usage.EncodedBytes > api.MaxACPConversationsBytes || usage.Evictions <= lastEvictions {
			t.Fatal("sustained workload did not enforce/turn over retention", usage)
		}
		lastEvictions = usage.Evictions
		indexes := 0
		store.mu.Lock()
		for _, slot := range slots {
			indexes += len(slot.model.index)
			if len(slot.model.index) != len(slot.model.entries) || slot.model.bytes > api.MaxACPConversationBytes {
				t.Error("tool index or model budget escaped retention")
			}
			if slot.pending != nil && len(api.Payload(slot.pending)) > api.MaxACPConversationNotificationBytes {
				t.Error("unconsumed notification exceeded budget")
			}
		}
		store.mu.Unlock()
		goruntime.GC()
		var memory goruntime.MemStats
		goruntime.ReadMemStats(&memory)
		t.Logf("round=%d seconds=10 models=%d readers=%d writes=%d min_writes=%d reads=%d min_reads=%d encoded_bytes=%d entries=%d indexes=%d cumulative_evictions=%d heap_after_gc=%d heap_objects=%d max_response_bytes=%d", round, models, readers, totalWrites, minWrites, totalReads, minReads, usage.EncodedBytes, usage.Entries, indexes, usage.Evictions, memory.HeapAlloc, memory.HeapObjects, maxResponse.Load())
		goruntime.KeepAlive(store)
	}
	for _, slot := range slots {
		slot.remove()
	}
	if usage := store.statistics(); usage.Models != 0 || usage.Entries != 0 || usage.EncodedBytes != 0 || len(store.slots) != 0 {
		t.Fatal("forget did not reclaim sustained model state", usage)
	}
}
