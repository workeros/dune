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
read-only as described below; this is not the full lifecycle requirement's
delivery claim.

Discovery also classifies artifacts inside this installation's ACP namespace:

| Code | Meaning and cleanup boundary |
| --- | --- |
| `REGISTRATION_FILE_MISSING` / `REGISTRATION_FILE_INVALID` | The independently registered host remains a candidate; the local cache file is missing, unsafe or inconsistent. A verified live IPC connection can still serve it. |
| `INSTANCE_MARKER_INVALID` / `RUNTIME_DIRECTORY_INVALID` | The directory/marker no longer matches its registered identity. Do not infer ownership from its current pathname. |
| `RUNTIME_DIRECTORY_MISSING` | The independently registered target is retained; its original directory is absent. |
| `IPC_SOCKET_MISSING` / `IPC_SOCKET_REPLACED` | The expected endpoint pathname is missing or differs from the registered inode/type/owner. Existing verified IPC may still work. |
| `REGISTRATION_TEMPORARY_FILE` | An atomic-write temporary file exists. Discovery cannot tell whether it is abandoned and never deletes it. |
| `UNREGISTERED_RUNTIME_ARTIFACT` / `UNREGISTERED_HOST_PANE` | A local artifact lacks verified registration. It grants no Runtime identity or cleanup authority. |
| `HOST_PANE_IDENTITY_MISMATCH` | A pane name matches a recorded target but its instance marker differs or is missing. It must not be adopted or removed as that target. |
| `TMUX_DISCOVERY_UNAVAILABLE` / `ARTIFACT_SCAN_INCOMPLETE` | The bounded scan could not obtain all evidence. Healthy Runtime observations remain in the response. |

Unregistered artifacts carry an opaque `artifact_ref` for stable correlation;
filenames, pane command lines and artifact contents are not exported. Each scan
reads at most 256 root entries, 32 entries per registered Runtime directory and
256 pane descriptions, with 64 KiB of tmux output. Artifact issues are capped at
128 plus one explicit truncation issue. Registration files are capped at 64 KiB.
All metadata reads occur through verified private files/pinned directory handles.
No scan deletes, renames, adopts or starts an artifact. Explicit forget uses only
the independent original cleanup plan; its known quarantine is excluded while
retirement is in progress. Sockets in the shared per-user IPC directory without
an association to this installation are not assigned or removed by guesswork.

ACP Runtime descriptions include `acp_host`: the planned IPC protocol before
registration, then the original host instance, retained program SHA-256/size,
host/Agent/guardian-group PIDs, host start time, current connector availability,
control term and most recent attach time. PIDs are diagnostic observations only.
A read-only probe does not change attach facts; a stale connector's detach cannot
overwrite its successor. Unavailable responses retain last-confirmed facts and
must not be read as a fresh process check.

Each ACP Runtime owns a private `program` copy outside releases. Host startup and
every explicit Agent replacement verify its SHA-256, size and private file
properties. The host runs that copy, so its guardian and MCP stdio bridge also
resolve the retained executable. Copying uses a distinct inode, exclusive
creation and file/directory synchronization; release overwrite or deletion
cannot change it. One program is limited to 256 MiB; the existing 16-Runtime
reservation bounds retained program data at 4 GiB per installation. Explicit
forget removes the original copy after stopping its owner. The implemented
[upgrade preflight](acp-upgrades.md) checks the original hosts before switching
connectors; platform service-manager acceptance remains a separate check.
