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

The implementation is being delivered in vertical slices. Notification delivery,
strict prompt-generation preconditions, and
consumer migration are subsequent slices; this document is not a completion
report for the full conversation requirements.

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
accounting is not a claim about RSS; allocation and resource measurements are
part of the remaining acceptance work.

Model/paging/capacity regression: `go test -race ./pkg/fabricd -run '^TestConversation'`.
