package fabricd

import (
	"encoding/json"
	"fmt"
	"os"
	goruntime "runtime"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"github.com/aiomni/dune/pkg/api"
)

// Opt-in measurement keeps routine regressions small. The workload uses the
// actual defaults and encoded response path, without Agent/vendor resources.
func TestConversationResourceEnvelope(t *testing.T) {
	if os.Getenv("DUNE_CONVERSATION_RESOURCE_TEST") != "1" {
		t.Skip("set DUNE_CONVERSATION_RESOURCE_TEST=1 for resource measurement")
	}
	const models, writes, readers, readsPerReader = 20, 400, 8, 200
	store := newConversationStore()
	slots := make([]*conversationSlot, models)
	for i := range slots {
		slots[i] = store.register(fmt.Sprint(i), "inc", nil)
		slots[i].begin(api.ACPAction{Action: "load", Cwd: "/tmp"})
	}
	body := api.Payload(strings.Repeat("x", 64*1024))
	// Each update owns an independently allocated field, like decoded ACP input.
	// Reusing body directly would count 128 MiB while retaining one 64 KiB slice.
	update := func(slot *conversationSlot, id string) {
		slot.update(map[string]json.RawMessage{"sessionUpdate": api.Payload("tool_call"), "toolCallId": api.Payload(id), "rawOutput": append(json.RawMessage(nil), body...)}, "")
	}
	for i, slot := range slots {
		for n := 0; n < 8; n++ {
			update(slot, fmt.Sprintf("warm-%d-%d", i, n))
		}
	}
	var peakHeap atomic.Uint64
	var writersActive atomic.Int64
	writersActive.Store(models)
	var readsDuringWrites atomic.Uint64
	var baseline goruntime.MemStats
	goruntime.ReadMemStats(&baseline)
	samplerDone := make(chan struct{})
	go func() {
		tick := time.NewTicker(10 * time.Millisecond)
		defer tick.Stop()
		for {
			select {
			case <-samplerDone:
				return
			case <-tick.C:
				var usage goruntime.MemStats
				goruntime.ReadMemStats(&usage)
				for old := peakHeap.Load(); usage.HeapAlloc > old && !peakHeap.CompareAndSwap(old, usage.HeapAlloc); old = peakHeap.Load() {
				}
			}
		}
	}()
	var group sync.WaitGroup
	start := make(chan struct{})
	durations := make([]time.Duration, 0, readers*readsPerReader)
	var measure sync.Mutex
	maxResponse := 0
	began := time.Now()
	for i, slot := range slots {
		group.Add(1)
		go func() {
			defer group.Done()
			<-start
			defer writersActive.Add(-1)
			for n := 0; n < writes; n++ {
				update(slot, fmt.Sprintf("tool-%d-%d", i, n))
			}
		}()
	}
	for worker := 0; worker < readers; worker++ {
		group.Add(1)
		go func() {
			defer group.Done()
			<-start
			for n := 0; n < readsPerReader; n++ {
				slot := slots[(worker+n)%models]
				if writersActive.Load() > 0 {
					readsDuringWrites.Add(1)
				}
				before := time.Now()
				page, err := slot.read(api.ACPConversationRead{ConversationID: slot.describe().ID})
				if err != nil {
					t.Error(err)
					return
				}
				// Alternate read and get, including response encoding in latency.
				encoded := api.Payload(page)
				if n%2 == 1 && len(page.Entries) > 0 {
					ids := make([]string, len(page.Entries))
					for i, entry := range page.Entries {
						ids[i] = entry.ID
					}
					values, err := slot.get(api.ACPConversationGet{ConversationID: page.Conversation.ID, EntryIDs: ids})
					if err != nil {
						t.Error(err)
						return
					}
					encoded = api.Payload(values)
				}
				if len(encoded) > api.MaxACPConversationResponseBytes {
					t.Error("response budget exceeded")
				}
				measure.Lock()
				durations = append(durations, time.Since(before))
				maxResponse = max(maxResponse, len(encoded))
				measure.Unlock()
			}
		}()
	}
	close(start)
	group.Wait()
	elapsed := time.Since(began)
	close(samplerDone)
	usage := store.statistics()
	store.mu.Lock()
	indexes := 0
	for _, slot := range slots {
		indexes += len(slot.model.index)
		if len(slot.model.index) > len(slot.model.entries) {
			t.Error("native index outlived retained entries")
		}
	}
	store.mu.Unlock()
	if usage.EncodedBytes > api.MaxACPConversationsBytes || usage.Evictions == 0 {
		t.Fatal("workload failed to exercise bounded eviction", usage)
	}
	if readsDuringWrites.Load() < readers*readsPerReader/4 {
		t.Fatal("workload did not overlap readers and writers", readsDuringWrites.Load())
	}
	goruntime.GC()
	var memory goruntime.MemStats
	goruntime.ReadMemStats(&memory)
	var process syscall.Rusage
	_ = syscall.Getrusage(syscall.RUSAGE_SELF, &process)
	rss := process.Maxrss
	if goruntime.GOOS != "darwin" {
		rss *= 1024
	}
	sort.Slice(durations, func(i, j int) bool { return durations[i] < durations[j] })
	t.Logf("os=%s arch=%s go=%s CPUs=%d models=%d writes_per_model=%d field_bytes=65536 readers=%d reads=%d reads_during_writes=%d elapsed=%s encoded_bytes=%d entries=%d indexes=%d evictions=%d heap_after_gc=%d total_allocated=%d peak_sampled_heap=%d process_peak_rss=%d max_response=%d read_or_read_plus_get_p50=%s read_or_read_plus_get_p95=%s read_or_read_plus_get_p99=%s", goruntime.GOOS, goruntime.GOARCH, goruntime.Version(), goruntime.NumCPU(), models, writes, readers, len(durations), readsDuringWrites.Load(), elapsed, usage.EncodedBytes, usage.Entries, indexes, usage.Evictions, memory.HeapAlloc, memory.TotalAlloc-baseline.TotalAlloc, peakHeap.Load(), rss, maxResponse, durations[len(durations)/2], durations[len(durations)*95/100], durations[len(durations)*99/100])
	goruntime.KeepAlive(store)
}

// Compare a fixed response window against different retained histories. Setup
// is excluded; measured allocations include the final JSON response encoding.
func BenchmarkConversationBoundedRead(b *testing.B) {
	for _, retained := range []int{100, 5000} {
		for _, count := range []int{10, 100} {
			for _, method := range []string{"read", "get"} {
				b.Run(fmt.Sprintf("history_%d/%s_%d", retained, method, count), func(b *testing.B) {
					slot := newConversationStore().register("benchmark", "inc", nil)
					slot.begin(api.ACPAction{Action: "new", Cwd: "/work"})
					for n := 0; n < retained; n++ {
						slot.update(map[string]json.RawMessage{"sessionUpdate": api.Payload("vendor_event"), "body": api.Payload(strings.Repeat("x", 1024))}, "")
					}
					id := slot.describe().ID
					ids := make([]string, count)
					for i := range ids {
						ids[i] = fmt.Sprintf("e-%d", retained-i)
					}
					b.ReportAllocs()
					b.ResetTimer()
					responseBytes := 0
					for n := 0; n < b.N; n++ {
						if method == "read" {
							page, err := slot.read(api.ACPConversationRead{ConversationID: id, Limit: &count})
							if err != nil {
								b.Fatal(err)
							}
							responseBytes = len(api.Payload(page))
						} else {
							values, err := slot.get(api.ACPConversationGet{ConversationID: id, EntryIDs: ids})
							if err != nil {
								b.Fatal(err)
							}
							responseBytes = len(api.Payload(values))
						}
					}
					b.ReportMetric(float64(responseBytes), "response_bytes")
				})
			}
		}
	}
}
