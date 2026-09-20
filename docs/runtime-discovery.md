# Runtime discovery

`runtime.list` and `Client.List` return an `api.RuntimeList` object with `items`,
`complete`, and `issues`. An issue has a stable `code` and, when its exact original
identity is known, a `runtime`. The response never includes registration blobs,
bootstrap environment, tokens, or Agent command lines.

`complete: false` means some discovery evidence is unavailable. Healthy items
remain usable; missing items are not proof that those processes exited. A damaged
registration reports `REGISTRATION_INVALID`; an unsupported host protocol reports
`SESSION_PROTOCOL_UNSUPPORTED`. A live host that cannot answer reports
`SESSION_UNAVAILABLE` and retains its last observed state with
`availability: "unavailable"`. Successful probes set `last_confirmed_at` in UTC.
Only independent evidence that both the original host and its registered process
group are absent produces `state: "lost"`, `availability: "lost"` and
`stop_reason: "host_lost"`. Timeouts do not invent an exit code.

Discovery releases the Engine map lock before IPC. Concurrent list calls share
eight probe slots. One Runtime's overlapping probes coalesce; failed probes use
250 ms exponential backoff capped at 8 seconds, with up to 50% additional jitter.
Each probe has a two-second deadline. Connector startup attempts read-only
restoration for at most 30 seconds; subsequent reads can retry the same endpoint.
None of these paths starts an Agent, initializes ACP, loads a session or replays
a submission.

`agents.DirectoryPage` carries the same completeness and per-Runtime issues,
adding the original Runner ID. Completeness describes the current page's
discovery; `next_cursor` still controls pagination across Runners. A local host
issue does not disable an otherwise-ready Runner or hide its healthy siblings.
The Web workbench keeps missing cached targets marked unavailable, reconnects
only after observing them again, and deduplicates by their complete target.

Discovery reads the durable index at startup and rescans at most once per second
on list reads, unknown exact-target reads and bounded startup recovery. The scan
has a one-second context deadline and coalesces concurrent readers. An admitted
ACP launch without a registered host reports `HOST_REGISTRATION_PENDING` with its
original Runtime; a confirmed failed launch reports `LAUNCH_FAILED`. No scan
restarts setup or launches a missing host. A repaired registration can reconnect
its original instance. `REGISTRY_UNAVAILABLE` preserves previously known targets
and prevents absence from being treated as a stale Runtime.

Launches retain their own connection until initial publication. Discovery cannot
replace that connection or another known host identity. Cleanup completion and
scan publication are serialized, so a scan started before forget cannot reinsert
the retired Runtime. Classification of orphan filesystem/tmux artifacts remains
in the next slice; this is not the full lifecycle requirement's delivery claim.
