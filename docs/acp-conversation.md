# Managed ACP conversation model

fabricd owns the current managed ACP conversation independently of subscribers
and operation-result retention. `pkg/api` contains the public types; both
`pkg/client` and the default `pkg/sdk` expose the same execution methods.

```go
state, err := client.ACPState(ctx, runtime)
// Handle err and a nil state.Conversation before choosing a generation.
page, err := client.ReadACPConversation(ctx, runtime, api.ACPConversationRead{
    ConversationID: state.Conversation.ID,
})
older, err := client.ReadACPConversation(ctx, runtime, api.ACPConversationRead{
    Cursor: page.NextCursor, // only when page.HasMore
})
```

Calls follow the existing authorized Gateway route. Read/get never dispatch an
ACP control request. A new/load operation creates a new conversation ID; an old
ID returns `CONVERSATION_CHANGED`. Model revisions and entry orders are decimal
JSON strings and must be compared as integers.

Entries are tagged objects with exactly one `message`, `tool`, `turn`, or
`activity` payload. Message content retains ACP content blocks. Tool `fields`
preserve field presence, explicit nulls and structured ACP values. UI rendering
is the caller's responsibility. The model describes retained current values,
not a log of every token or a promise of complete native history.

`ReadACPConversation` selects a recent window, then paginates backward with a
stateless cursor. Each page is ascending by insertion order. The cursor fixes
the member upper bound, while entry values may change between pages.
`GetACPConversationEntries` refreshes selected IDs and distinguishes evicted and
unknown IDs. Entry revisions prevent late responses from replacing newer data.

Default retention is 8 MiB / 5,000 entries per Runtime and 128 MiB across the
engine. Entries are bounded to 256 KiB and read responses to 512 KiB. Read limits
default to 50 and cap at 200; explicit zero is invalid. At most eight conversation
read/get responses are in flight per engine. Capacity eviction removes a
contiguous insertion-order prefix and does not depend on readership.

Implementation, resource measurements and A01–A39 evidence are recorded in
[the acceptance report](acp-conversation-acceptance.md). Final acceptance still
requires a configured load-capable real Agent; local subprocess and browser
fixtures do not replace that validation.

Validation of the initial slice:

```sh
go test ./tests -run '^TestACPConversationReadWithoutSubscription$' -count=1 -v
```

This runs SDK → Gateway → fabricd with a controllable ACP subprocess, disconnects
the initial subscriber, reads recorded input/output, checks stable read/get and
cursor identities, then loads native history into a fresh generation. Its RPC
journal verifies that independent reads add no Agent calls. It is not a real
Agent acceptance test.

Oversized text retains bounded ACP content blocks for the prefix plus a separate
`message.tail` after an explicitly omitted gap. Further chunks update the tail
under the same entry ID. Tools preserve explicit null/empty values; oversized
fields use structured omission markers including the original JSON type.
UTF-8 and JSON escaping count toward the final encoded entry budget. Session
state is limited to 64 KiB. These limits are currently fixed (the defaults are
also hard maxima), advertised in Runner binding limits.

`machine.info.acp_conversations` reports model/entry/encoded-byte counts,
evictions, omitted updates, read bytes/time and in-flight loads. Encoded-byte
accounting is not a claim about RSS; the acceptance report gives measured
encoded bytes, heap, RSS, allocation churn and read costs.

Model/paging/capacity regression: `go test -race ./pkg/fabricd -run '^TestConversation'`.

Managed prompts now require `expected_conversation_id`. It is the ID the caller
observed when preparing its request; SDK/HTTP/host/MCP forwarding never fills or
refreshes it. Discover it through `acp.state.conversation.conversation_id` or
`runtime.conversation_id`. The accepted new/load operation retains its own
`conversation_id`, independently of later Runtime changes. IM stores this ID
with its native session and passes it unchanged with subsequent messages.

```go
operation, err := client.ACPSubmit(ctx, runtime, api.ACPAction{
    Action: "prompt", Text: "Continue checking the files",
    ExpectedConversationID: state.Conversation.ID,
})
```

Missing IDs return `INVALID_ARGUMENT`; obsolete IDs return
`CONVERSATION_CHANGED` at admission or a failed queued operation with the same
`error_code` at dispatch. No sent message is recorded for rejected prompts.
Adjacent equivalent in-flight loads share one operation reference; intervening
operations retain queue order. RPC results commit synchronously in the input
reader before process exit can change phase, preserving the independent opening
outcome after exit and operation-log expiration.

Each explicit new/load after the first open establishes a fresh initialized
Agent process/stdio connection within the same managed Runtime. This isolates
untagged same-native-ID notifications. Runtime identity, queue order, operation
records, MCP configuration and last-confirmed metadata remain owned by the
controller; old connection callbacks cannot update the new model. Oversized
output and read errors also check connection ownership under the controller
lock before changing completeness, publishing a notice or stopping the Runtime.
This includes the interval where a new model is visible while old output drains.
Permission requests and RPC replies already admitted by the input reader recheck
that boundary under the state lock after parsing. Malformed lines recheck it
before stopping the Runtime as well.
The Agent must support native load across processes. A failure to establish the
new connection fails the open without replaying a prompt. Normal read/state/
subscribe operations never use this path. The configured Runtime timeout is
not extended by reopening a native session.

`SubscribeACPConversation(ctx, runtime)` acknowledges an ordinary observation
stream (`runtime.attach` with `observe:true, conversation:true`). After it
returns, read the required pages. `acp_conversation_changed` carries a covered
`(previous_revision, revision]` interval and the union of changed IDs; at more
than 200 IDs or 16 KiB it uses `invalidates_all`. Generations and uncovered
intervals also force full invalidation. The stream contains live controller
state and exit/error events, without raw diagnostic chunks. Raw `Attach`
remains available for the protocol inspector. The capability is
`acp.conversation.changed`.

Notification publishers and individual subscribers each retain one bounded
pending interval. Slow raw diagnostics close with `SLOW_CONSUMER`; even load
replay never waits for a subscriber. Model changes caused by global eviction
also notify the affected Runtime. A latest-page read only satisfies IDs present
in that response; it cannot clear updates to previously loaded older entries.

Dune Web now renders retained model entries. It reads a recent page after an
ordinary model subscription, fetches earlier pages on demand, and refreshes
changed entries by ID. Each entry is merged by its own decimal-string revision;
older pages can add unseen entries, but cannot overwrite newer values or revive
entries below the known eviction boundary. Responses from an obsolete request
generation cannot select an older conversation. A recent-page response never
clears a pending update to an older loaded tool.

The Web cache tracks the upper order read from recent pages separately from
metadata discovered by state/get. If a refreshed recent page skips past that
order, its cursor becomes the next backward-read position so the unread middle
remains reachable. Overlapping or adjacent refreshes preserve existing paging
progress. Traversal may revisit cached entries, which merge by entry ID.

On reconnect the Web view intentionally starts from a fresh recent window. It
does not claim to have synchronized unrequested older pages. Its cache is bounded
to 4 MiB / 1,000 entries, separate from the server budget. Explicit native history
loading remains a control action. ACP Stream opens a separate diagnostic stream
only while visible; raw chunks never append to the model-based chat view.

SDK state/read/get calls respect caller cancellation with a maximum ten-second
read deadline. They check advertised capabilities and return `UNSUPPORTED` when
absent. Concurrent read/get admission can return `RESOURCE_EXHAUSTED`; canceling
a read or operation wait never cancels an Agent task. Ordinary subscriptions
have caller-controlled lifetime. Notifications have no artificial batching delay:
the store wakes its publisher on commit and coalesces unsent intervals while
publisher/subscriber work is pending. Scheduling/network delay is not a fixed
100 ms delivery guarantee.

A standalone read-only example uses only public Dune packages:

```sh
go run ./samples/acp-conversation -gateway "$DUNE_GATEWAY" -target "$DUNE_TARGET" -runtime runtime.json -limit 20
```

Provide the existing authorized token through `DUNE_TOKEN`. `runtime.json` is the
full exact Runtime obtained from discovery. For a private TLS certificate, pass
`-ca /absolute/path/to/ca.pem`. Use `-cursor <next_cursor>` for an earlier page;
use `-conversation <observed_id> -entries e-1,e-2` to refresh entries. ID refresh
never silently adopts a new generation. The sample never opens or loads a native
session and never downloads referenced content.

For subscription composition, wait for `SubscribeACPConversation` to return,
then call state/read. Keep the subscription receiving while requests are in
flight. In the same conversation, compare individual entry revisions; the page's
overall revision only updates its model description and retention boundary.
Maintain pending invalidations for loaded entries absent from that page. On a
gap or `invalidates_all`, refresh the ranges/IDs your application retained. On a
new target or conversation, invalidate old request contexts before accepting
new responses. A delayed c1 response must never select c1 after c2 was selected.

The current implementation retains in-memory content only while fabricd and its
Runtime remain available. Agent exit keeps the model readable; explicit stop or
forget removes it. The later session-host/reconnection proposal changes this
lifetime boundary and is a separate implementation.
