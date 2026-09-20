# ACP conversation implementation and acceptance

Date: 2026-09-20. Requirement baseline: SandDance
`docs/dune-acp-history-requirements.md`, A01–A39. Implementation starts at
`5166c9b` and is delivered directly on `main` in vertical slices.

The local implementation and protocol tests are available. **Final acceptance
is pending a configured, load-capable real Agent.** Browser tests use controlled
HTTP/WebSocket fixtures; process tests use real SDK/Gateway/fabricd transports
and a controlled ACP child. Neither is evidence of vendor native history.
The separate ACP session-host lifecycle proposal is not implemented here.

## Evidence map

Test names below are directly runnable with `go test <package> -run '^<name>$'`.
The model tests are in `pkg/fabricd/conversation_test.go`, controller scheduling
in `pkg/fabricd/acp_queue_test.go`, notifications in
`pkg/fabricd/conversation_notify_test.go`. “Local” means the behavior was checked
in this environment; qualification in the last column is part of the result.

| Item | Local evidence | Result / boundary |
| --- | --- | --- |
| A01 | `tests/TestACPConversationReadWithoutSubscription`; `TestConversationToolPatchPreservesNullAndUnknownFields` | Input, merged answer and turn via processes without subscribers; tool current values via model tests. |
| A02 | `TestACPConversationReadWithoutSubscription` | New SDK connection reads by Runtime and state, without operation ref or subscription. |
| A03 | `web/e2e/acp-conversation.spec.ts` model restoration case | Reload and second browser preserve model; no extra control calls. Backend RPC journal separately proves read side effects absent. |
| A04 | `TestConversationPagingEvictionAndRecreatedTool`; SDK conversation test; Web latest-page gap tests | Fixed member upper bound, ascending order, new insertions excluded from old cursor. Web refresh retains a cursor into unread middle ranges. |
| A05 | tool patch test; browser model restoration case | Stable tool identity; get refreshes an older loaded page. |
| A06 | `web/src/components/acp-model.test.mjs` older-page case | Older response adds unseen entries without overwriting newer values. |
| A07 | `tests/TestACPConversationIndependentCursorsAcrossConnections` and invalid-cursor tests | Six parallel requests across two connections reuse the same cursor; older/end-page traversal does not consume it. Multiple deployed host Pods not exercised. |
| A08 | `TestConversationByteLimitedReadAndGetProgress`; oversized tests | Final JSON size bound; page/get advance, explicit unprocessed IDs. |
| A09 | `TestConversationPagingEvictionAndRecreatedTool` | Partly and fully evicted cursor windows are distinguished and terminate. |
| A10 | model reducer eviction test | Old response cannot revive entries below the current retention boundary. |
| A11 | queue separation, close/admission and permission/cancel tests | Pending body is absent; attempted sends and terminal/unknown facts remain distinct. |
| A12 | `TestConversationProtocolMergingAndImmutableSnapshots`; `TestConversationReplayAndLiveProtocolSemanticsMatch` | Role/channel/turn boundaries and image/resource structures retained. |
| A13 | `TestConversationDoesNotDeduplicateEchoOrRepeatedInput` | Two equal user inputs plus their uncorrelated echoes remain four messages. |
| A14 | tool patch and `TestConversationLateToolTerminalAndMissingTurn` | Explicit null/empty fields, late completion, unknown native status and missing turn are preserved. |
| A15 | paging/eviction test | Later update creates a partial entry with a new order; old native index is reclaimed. |
| A16 | `TestConversationReplayAndLiveProtocolSemanticsMatch` | Actual controller receive path exposes replay while loading, before matching result. |
| A17 | `tests/TestACPConversationGenerationBarriersThroughGateway` | Two equivalent in-flight loads share ref and one recorded RPC; cancelled waiter does not cancel it. |
| A18 | `TestACPLoadCoalescesOnlyAdjacentInflightOperations` | Different targets/configuration or intervening operations preserve queue order. |
| A19 | Gateway generation-barrier test | Permission holds the actual child prompt; queued and network-delayed c1 prompts both fail after same-native reload, without body or RPC. |
| A20 | `TestNativeSessionConfirmationSurvivesLaterSwitchAndFailedLoad`; open-outcome tests | Failed/unknown opens retain fragments, do not forge confirmed metadata. Native coverage remains unknown. |
| A21 | `TestACPRejectsCallbacksFromOldConnectionWithSameNativeID`; `TestACPConnectionReadIssuesAreIsolated`; `TestACPInFlightCallbacksCannotCrossLoadBoundary`; SDK RPC journal | Old callbacks, oversized output and read errors excluded during drain and after replacement. Already-admitted permissions/replies recheck the open boundary before changing state. |
| A22 | SDK subscription-then-read test; notification tests; reducer race test | Acknowledged subscription, committed values and interval invalidations tested. |
| A23 | `TestConversationNotificationUnionAndOverflow` | Merged revisions retain union; ID/byte overflow and conversation-wide state changes become full invalidation (also `TestConversationStateChangesInvalidateTheWholeModel`). |
| A24 | SDK reconnect and browser restoration | Current model read independently; browser deliberately resets to a recent window. |
| A25 | `TestManagedACPHistoryReplayIgnoresSlowDiagnostics`; subscriber isolation test | A TCP barrier pauses actual native replay while an unread raw observer is attached; another Runtime completes prompt/read before replay is released. No deployed network saturation test. |
| A26 | `TestManagedACPOfflinePermissions`; `TestACPInFlightCallbacksCannotCrossLoadBoundary` | Handled/cancelled request cannot be approved again; retained content and in-flight old callbacks do not create permissions for a new model. |
| A27 | `TestACPOpenOutcomeIsSettledBeforeExitAndSurvivesRetention` and independent store | Operation records expire separately; retained model and opening facts survive. |
| A28 | open-outcome tests; authorization/exit test; existing process restart regressions | Ordered tail/exit and forget verified; memory is not a cross-fabricd-restart transcript. |
| A29 | `pkg/access/TestConversationAuthorizationIdentityAndRetainedExit`; existing enterprise identity/access tests | All new reads/subscription carry fixed authenticated scope and exact Runtime, reject denial/revocation and stale binding; no new cross-host deployment claim. |
| A30 | `TestACPCredentialEchoIsRedactedFromOperationAndError`; inspector/stderr redaction tests | Injected MCP credential is absent from model, operation, state and diagnostics. No arbitrary-secret detector claim. |
| A31 | burst and sustained resource tests, read benchmark, repeated streaming, capacity and subscriber tests | 30 seconds / three rounds of sustained churn with bounded retained heap, indexes and responses, plus forget reclamation. Not a long-running production soak. |
| A32 | independent-cursor process test plus subscription/read tests | A subscriber and independent readers coexist; traversal/reuse changes neither model revision/retention nor actual Agent RPC journal. |
| A33 | authorization/exit test | Raw ACP and missing advertised capabilities return `UNSUPPORTED`. |
| A34 | unopened-controller check; replay-before-result; global eviction | No model, empty loading model, and evicted empty window have separate identity/state/flags. |
| A35 | reducer `new latest page never clears older entry refresh obligations`; browser older tool refresh and full invalidation gap cases | Newer recent-page revision does not cancel old-page get obligations or strand unread middle entries after gets finish. |
| A36 | Gateway barriers; host `TestAgentMessengerPreservesObservedConversationAfterSameNativeReload`; IM `TestPromptKeepsStoredGenerationAfterRuntimeRefresh` | Missing IDs fail; stale observed ID forwarded unchanged across consumers and rejected by fabricd. |
| A37 | authorization/exit test | Exited Runtime keeps native confirmation; forget removes directory entry and rejects old selectors. |
| A38 | `TestACPOpenOutcomeIsSettledBeforeExitAndSurvivesRetention` | Succeeded/failed/unknown opening outcome survives exit and operation/body cleanup. |
| A39 | Gateway barriers; immutable snapshot test; browser held-page case | Actual read/get results held in a transparent relay arrive as c1 after c2; newly acquired c1 reads fail; browser cannot switch backward. |

The transparent relay delays actual protocol traffic; it does not generate
responses or alter request fields. Its three barriers are request delivery before
admission, an actual child permission request before dispatch, and actual
fabricd read/get results before delivery to the SDK. The expected Agent journal
is exactly `initialize, session/new, session/prompt, initialize, session/load`.

## Resource measurement

Command:

```sh
DUNE_CONVERSATION_RESOURCE_TEST=1 go test ./pkg/fabricd -run '^TestConversationResourceEnvelope$' -v -count=1 -timeout=180s
go test ./pkg/fabricd -run '^$' -bench '^BenchmarkConversationBoundedRead$' -benchtime=200ms -benchmem -count=1
```

Machine: Apple M5 Pro, darwin/arm64, 18 logical CPUs, Go 1.27.1. Default
budgets were used: 8 MiB / 5,000 entries per model; 128 MiB aggregate;
256 KiB per entry, 64 KiB state, 512 KiB complete response, eight concurrent
network read/get slots. The model workload uses 20 writers, each with 8 warmup
and 400 distinct 64 KiB tool fields, plus eight readers performing 200 iterations
each. Input fields are independently allocated, not shared slices that would
understate retained heap. Half the iterations also fetch the page's IDs with get.

One recorded run at implementation commit `1e9b175` (the later notification-only fix does not change retention budgets):

| Metric | Measured value |
| --- | --- |
| Total workload time | 340.83 ms |
| Read iterations overlapping active writers | 832 / 1,600 |
| Retained encoded bytes | 134,160,771 |
| Retained entries / native index keys | 2,039 / 2,039 |
| Prefix evictions | 6,121 |
| Live heap after explicit GC, store still reachable | 156,914,768 bytes |
| Peak sampled heap (10 ms sampling) | 303,962,272 bytes |
| Process peak RSS (`getrusage`) | 467,795,968 bytes |
| Total allocated during workload | 3,294,345,640 bytes |
| Largest encoded response | 461,182 bytes |
| Read or read-plus-get + encoding p50 / p95 / p99 | 0.584 / 5.232 / 6.647 ms |

The final encoded budget is about 128 MiB, live heap about 150 MiB and process
peak RSS about 446 MiB. Encoded accounting is **not an RSS limit**. Allocation
churn and transient response/copy buffers matter; these numbers include this
test process and are not end-to-end network latency or an Agent memory budget.
The sub-second burst does not establish steady-state memory, vendor replay
throughput, long-duration fairness or a production sizing guarantee.

Fixed-window microbenchmark, including JSON encoding (setup excluded):

| Retained history | Method / returned entries | Time | Allocated bytes/op |
| --- | --- | --- | --- |
| 100 | read / 10 | 10.47 µs | 20,429 |
| 5,000 | read / 10 | 9.06 µs | 20,285 |
| 100 | get / 10 | 11.05 µs | 20,466 |
| 5,000 | get / 10 | 8.94 µs | 20,323 |
| 100 | read / 100 | 81.61 µs | 186,270 |
| 5,000 | read / 100 | 71.40 µs | 175,704 |
| 100 | get / 100 | 87.16 µs | 200,688 |
| 5,000 | get / 100 | 74.81 µs | 187,854 |

This supports cost proportional to the requested window, rather than cloning
the entire retained history. It does not promise the smaller timings at larger
history sizes: run order, allocator reuse and CPU scheduling influence results.

## Real Agent acceptance

`TestRealACPConversationLoad` is implemented but **skipped: environment not
configured**. It requires all of:

- `DUNE_REAL_AGENT=1`.
- `DUNE_REAL_ACP_COMMAND`: JSON argv for a load-capable ACP Agent, or a wrapper
  supplying an already configured isolated account environment.
- `DUNE_REAL_ACP_WORKDIR`: absolute dedicated acceptance directory.

```sh
go test ./tests -run '^TestRealACPConversationLoad$' -v -count=1 -timeout=180s
```

The test records Agent name/version from initialize, creates a native session,
asks for a unique harmless text marker, verifies user and Agent content, then
loads the same native session through a new Agent connection and checks both
contents again. It never substitutes the raw-ACP initialize-only test for load.
It stops the Runtime and removes its temporary working directory. The Agent's
own native session may remain in its configured isolated history directory.
No daily-use account/configuration is copied or changed by the test.

## Integration boundary

The adjacent SandDance `go test ./... -count=1 -timeout=300s` passed with a
**temporary modfile** replacing both `github.com/aiomni/dune` and
`github.com/aiomni/dune/im` with these local sources. Its source, lockfiles and
requirements were not changed. This checks the public Go boundary; it does not
upgrade deployed Runners or implement/validate SandDance's history UI.

A read-only public SDK example is in
[`samples/acp-conversation`](../samples/acp-conversation/main.go). Contract,
notification composition and prompt examples are in
[`acp-conversation.md`](acp-conversation.md).

Verification logs, final build identity and SHA-256 live under `.local/` and
`bin/`, outside source control. `bin/dune fabricd` runs the matching connection
service; this project does not distribute a separate default fabricd binary.

## Tracer Bullet commits

| Commit | Vertical slice |
| --- | --- |
| `4e8c679` | Public SDK → Gateway → fabricd retained model read, without subscribers. |
| `643bce3` | Bounded current values, stable paging/get, prefix eviction and diagnostics. |
| `8131c28` | Observed prompt generation through all consumers; ordered open outcome. |
| `04c1bde` | Fresh Agent connection for each explicit reopen, including same native ID. |
| `7e6cf1d` | Bounded invalidation streams and isolation from slow diagnostics. |
| `f1d3e77` | Dune Web restoration, older pages, current-entry refresh and bounded browser cache. |
| `4d4a087` | Actual request/response barriers, authorization, lifecycle and protocol acceptance. |
| `26e1056` | Browser fixture follows explicit terminal input ownership. |
| `1e9b175` | Resource tests, public SDK example, opt-in real load test and handshake rejection fixture. |
| `86e68f2` | Full invalidation for conversation state changes; retention after operation expiry. |
| `a9804e2` | Independent Runtime progresses while real child replay is paused. |

## Verification record

| Command / scope | Result |
| --- | --- |
| `make test TEST_FLAGS='-count=1 -timeout=600s -p=2'` | PASS, all main-module packages and all four IM packages; process suite 197.315 s. |
| `make check-go` | PASS, main and IM vet; final sample/fixture edits additionally checked with `go vet ./samples/mock-acp ./samples/acp-conversation ./tests`. |
| `make test-im-race TEST_FLAGS='-count=1 -timeout=300s'` | PASS, all four IM packages. |
| `go test -race ./pkg/fabricd -run '^TestConversation\|^TestACP' -count=1 -timeout=120s` | PASS after final notification fix, 10.386 s. |
| `go test -race ./pkg/access -run '^TestConversationAuthorizationIdentityAndRetainedExit$' -count=1` | PASS. |
| `go test -race ./pkg/gateway -count=1 -timeout=90s` | PASS after rejection-fixture correction. |
| `go test ./tests -run '^TestACPConversation\|^TestManagedACP' -count=1 -timeout=120s` | PASS, including real Gateway/fabricd processes; final replay-isolation enhancement also passed separately in 8.012 s. |
| `make web-check web-build` | PASS, existing bundle-size warnings remain. |
| `npm --prefix web test` | PASS, 8 tests. |
| `npm --prefix web run e2e` | PASS, 18 Chromium cases. Wide/narrow model-restoration screenshots inspected. |
| Adjacent SandDance Go suite with temporary local replacements | PASS; no dependency/source changes made there. |
| Opt-in resource test and read/get benchmarks | PASS within the workload boundaries recorded above. |
| `TestRealACPConversationLoad` | SKIP, Agent command/account environment not configured. |
| PostgreSQL, multi-host/Pod deployment and vendor Agent tests | Not exercised without their dedicated environment; skipped tests are not counted as passed deployments. |

Earlier full attempts exposed an obsolete prompt fixture, a package-level
180-second timeout, a Gateway rejection/connection-close test race, and one PTY
native initialization timeout under concurrent test startup. Prompt and Gateway
fixtures were corrected. PTY targeted race passed three times; the complete
suite then passed with two package workers and a 600-second package deadline.
The browser fixture failure was fixed by modeling terminal ownership and
explicitly acquiring input before typing. These were not silently discarded
failures. Subsequent small notification and replay-fixture edits were followed
by the affected race/process tests listed above.

The 512 KiB response maximum is below the current 4 MiB protobuf message limit.
Memory measurements use encoded responses and include serialization; network
buffers and an external Agent remain separate resource consumers.

## Sustained retention and independent readers follow-up

Additional focused checks after the full regression above:

```sh
go test ./tests -run '^TestACPConversationIndependentCursorsAcrossConnections$' -count=1 -timeout=90s
DUNE_CONVERSATION_RESOURCE_TEST=1 go test ./pkg/fabricd -run '^TestConversationSustainedResourceRetention$' -v -count=1 -timeout=90s
go vet ./tests ./pkg/fabricd
```

All passed. The cursor test uses actual SDK → Gateway → fabricd processes,
six parallel reads across two independent connections, an acknowledged model
subscription and a new turn inserted after the cursor was created. It verifies
every member of the original range exactly once, reuses the original cursor
after reaching the end, and checks unchanged model/retention facts and Agent RPC
traffic. This is local multi-connection evidence, not a multi-Pod deployment.

Sustained measurement uses the same machine/default budgets as above: 20 model
writers, independently allocated 64 KiB tool fields at 5 ms intervals, and eight
readers at 10 ms intervals. Each 10-second round stops its workers before GC,
then the next round continues using the same models, IDs and indexes. Pending
notifications remain unconsumed and are checked against their byte limit.

| Round | Writes | Reads | Retained entries / indexes | Cumulative evictions | Encoded bytes | Heap after GC |
| --- | --- | --- | --- | --- | --- | --- |
| 1 | 39,969 | 7,997 | 2,039 / 2,039 | 37,930 | 134,165,177 | 157,025,968 |
| 2 | 39,950 | 7,996 | 2,039 / 2,039 | 77,880 | 134,165,178 | 156,758,552 |
| 3 | 39,996 | 7,994 | 2,039 / 2,039 | 117,876 | 134,170,523 | 155,897,256 |

Every writer completed at least 1,997 updates per round; every reader completed
at least 999 reads. The largest encoded response was 461,219 bytes. Retained
heap stayed approximately 149–150 MiB despite cumulative churn. Removing all
models reclaimed the store's model count, retained bytes, entries and indexes.
The 30-second test adds repeated-eviction evidence; it does not establish
long-term production memory or latency guarantees.

## Review fixes: pagination gaps and stale connection failures

Both independently supplied review reproductions failed again on `c1689af`
before the fixes and passed afterward.

- `7f160d8`: Web latest-page refresh tracks the last read upper order. A jump
  from entries 1–10 to 251–300 adopts the new cursor, preserving access to
  11–250 after older dirty entries finish refreshing. Unit tests also cover
  overlapping/adjacent refreshes, multiple jumps, preexisting pagination,
  stale snapshots and eviction. The browser case reads all 300 entries through
  five earlier-page requests without issuing an ACP control action.
- `e37e49f`: oversized output checks connection ownership while holding the
  controller lock through model/operation mutation and notice publication.
  The same rule now applies to reader errors before stopping a Runtime.
  `TestACPConnectionReadIssuesAreIsolated` covers both issues while the old
  connection drains, after replacement, and on the current connection. It
  checks model revision/flags, operation completeness, notices and Runtime stop
  behavior. The raw ACP continuation test preserves omission recovery.

Checks performed for these fixes:

| Command | Result |
| --- | --- |
| `node --test /tmp/dune-main-review.obQ8wX/paging-gap.test.mjs` | PASS; original paging reproduction. |
| `go test -overlay /tmp/dune-main-review.obQ8wX/overlay.json ./pkg/fabricd -run '^TestReview' -count=1 -v` | PASS; original stale-output reproduction. |
| `make test TEST_FLAGS='-count=1 -timeout=600s -p=2'` | PASS, main and IM modules; process suite 201.762 seconds. |
| `go test -race ./pkg/fabricd ./pkg/access ./pkg/client ./internal/agentservice -count=1 -timeout=180s` | PASS. |
| `make check-go` | PASS, main and IM modules. |
| `npm --prefix web test` | PASS, 11 tests. |
| `make web-check web-build` | PASS; existing bundle-size warnings remain. |
| `npm --prefix web run e2e -- acp-conversation.spec.ts` | PASS, three Chromium cases. |

The real-Agent command, isolated account environment and work directory remain
unconfigured. These fixes do not change the pending native-load acceptance or
the resource/deployment qualification above.
