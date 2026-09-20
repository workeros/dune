# ACP lifecycle diagnostics

Read `machine.info` for connector version/process facts, submission/control
capacities, stream capacities, tmux versions and connector log usage. Read the
exact original Runtime for `acp_host` version, process, attach and log facts.
`acp.state` additionally reports the original managed queue, permission and
operation-result budgets; raw state reports its input queue and output windows.
The independent `submission.get` route remains the source for admitted work and
cleanup progress even after a host or its Runtime directory is gone.

`dune --config FILE version --runner` reads the version/Runtime diagnostics
through the public SDK/Gateway path. See [upgrade checks](acp-upgrades.md) for
target-vs-running version semantics and the initial legacy transition boundary.

Each installation stores connector lifecycle events at
`<session_dir>/connector-events.jsonl`. Each original host stores its events at
`<session_dir>/acp/runtimes/<runtime_id>/host-events.jsonl`. Files are private,
mode 0600, and opened without following a final symlink or accepting hard links.
The original file descriptor remains pinned while the process runs. Forget
removes the host log only through its existing accepted Runtime-directory plan;
connector-side cleanup results remain available afterward.

Both log kinds have a **64 KiB file limit**, a **64-entry memory queue** and at
most **1535 bytes per encoded entry**. Before the next write exceeds the file
limit, the writer starts a new window with `log_window_reset`. After restart it
also resets a truncated final entry. One disk writer drains each queue; producers
never wait for a file write or log reader. Saturation drops diagnostics and
increments `dropped`; write failures increment `write_errors`. The public
`lifecycle_log` fields report these counters and the exact hard limits. Logging
is best effort and does not acknowledge business success or replace the registry.

Events include host start/exit, final Agent exit, observed admission receipts,
launch/stop acceptance, fabricd host attach/detach, control takeover/rejection,
handshake/protocol rejection, the first managed/raw output gap, and accepted
cleanup checkpoints/results. Continuous connector Gateway attach/detach/failure
and cleanup-recovery diagnostics use the same bounded connector log. A repeated
`admission_receipt` for the same operation can be a duplicate queryable submission;
it never means the Agent executed again. Window rotation or process crash can
remove diagnostic history; use original submission queries for business evidence.

The schema contains event names, generated Runtime/host/operation references,
numeric terms/counts and bounded error codes. It has no request-body, environment,
token, prompt, tool-body or raw-permission field. Gap logging records a boundary
observation; the latest output/model state supplies current retained positions.
Version and diagnostic reads perform no Agent control RPC and cannot adopt or
clean up an unregistered artifact.
