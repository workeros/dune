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
target-vs-running version semantics and the current host requirements.

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

Installed services additionally use `<config_directory>/<service_name>-diagnostics/service-events.jsonl`
in a separately created 0700 directory, with the same 64 KiB bound, private-file checks, single-writer lock and reset
markers. It records `service_starting` before reading configuration and
`service_exit` with `FAILED` or `STOPPED` on a normal error/return. Repeated startup
failure cannot grow a flat log indefinitely. A SIGKILL or panic may leave only
the start record; absence of an exit event is not an exit-status claim.

Generated launchd/systemd definitions route stdout/stderr to the null device;
continuous connection/host diagnostics use the bounded lifecycle files. Raw
startup error strings and panic dumps are not retained by the installed service.
Use service-manager status for the process exit status. For a startup problem,
after confirming the connector is stopped, foreground `dune --config FILE fabricd`
prints the full error to the invoking terminal. Logs redirected by an operator
or external service-manager implementation have that operator's retention policy.
No existing operator log files are automatically deleted or adopted.

## Startup evidence

An accepted `profile.start` receipt retains the caller's submission ID, complete
launch target, operation reference and reserved Runtime identity. Its
`runtime.acp_host.startup` contains a fixed phase, optional error code and
`confirmation_timeout` observation. No raw exception, bootstrap, environment,
prompt or ACP response is stored in this diagnostic.

| Phase | Evidence |
| --- | --- |
| `host_pending` | Original resources prepared; host entry is not confirmed |
| `host_validation` | The original process entered its one-use registration gate |
| `agent_start` | Host validated; guardian/Agent startup is in progress |
| `agent_initialization` | Managed Agent process started; initialize is outstanding |
| `ready` | Raw stdio owner is available, or managed initialize succeeded |

Managed startup reaches `stage=started` after initialize succeeds. Raw startup
publishes the stdio owner; the caller still owns raw initialize/new/load/prompt.
The connector's unchanged ten-second confirmation window can end before either
host entry or managed initialize completes. It returns `RESULT_UNKNOWN`, keeps
`stage=host_starting`, and sets `confirmation_timeout` when the registry can
record that observation. The original host can still complete after this point.
Query the original key; do not submit another launch/setup/initialize/prompt.

Confirmed failures retain `admission=accepted` and use `stage=failed` with
`HOST_EXITED_BEFORE_ENTRY`, `HOST_VALIDATION_FAILED`, `HOST_START_FAILED`,
`AGENT_START_FAILED` or `AGENT_INITIALIZATION_FAILED`. A returning original host
can publish failure only after its Agent group is absent. Connector recovery
requires the independently recorded original PID/kernel boot and absence of
both host and Agent group. The proof runs in the same transaction that fences
host entry, registration and guardian group admission. A timeout, failed IPC
probe, missing directory, or tmux observation alone cannot close startup.

Use `runtime.stop` and `runtime.forget` with new lifecycle submission keys and the
**original complete Runtime target**. A failed launch remains queryable after
cleanup and its live Runtime capacity is released only when forget completes.
For the earlier launches that predate preparation records, only an explicit
stop/forget can collect the original private bootstrap, instance marker, retained
program and exited marked pane. Kernel absence plus a transaction proving no
host has registered is required before recording failure and using the ordinary
fenced cleanup plan. Missing/conflicting evidence stays unknown. Reads never
adopt or delete these artifacts, and no accepted launch is executed again.
