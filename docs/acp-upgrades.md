# ACP connector upgrades

Run the **proposed executable** against the existing machine configuration:

```sh
/path/to/proposed/dune --config /path/to/config.yaml upgrade-check
```

The command prints `api.UpgradeReport` JSON. Exit 0 and `allowed: true` mean the
observed hosts use the target's supported IPC contract and their retained helper
programs are verifiable. The current supported range is exactly protocol **1**.
The report identifies the target's embedded build and includes original Runtime identities, host instances, protocol and
program digests. It does not include bootstrap environments or credentials.

This is a preview, not a reservation. It reads the independent SQLite registry
with a read-only connection and bounded deadline, never creates a registry,
starts tmux, connects a host, initializes an Agent, or advances a cleanup job.
The digest scan is bounded by the existing 16 live Runtime reservations and
256 MiB per retained program, with a 15-second check deadline.

`repair`, `upgrade` and direct `service install` all run the target executable's
check again under an exclusive private launch gate **before stopping fabricd**.
The gate remains held through service replacement. Service installation has a
45-second deadline; a pending launch makes it refuse immediately. Existing
queries, permissions, cancellation, stop, forget and already-admitted host work
do not take the gate. New launches use a shared gate until host publication,
releasing it before the attached observer stream.

A refused launch claims and durably rejects its caller-owned key with
`UPGRADE_IN_PROGRESS`. That key remains `not_accepted` after the gate opens and
across connector replacement. If the registry cannot persist a refusal, the
answer remains `unknown`; the refused request does not execute. Capacity errors
and gate file errors preserve the same original key as well.

The registry snapshot includes admitted original launches whose hosts have not
registered yet. Their planned IPC version is recorded before any launch side
effect. A launch still executing in the old connector holds the shared gate; a
late original host after connector death remains covered by its recorded version.
No launch/setup replay is part of preflight or replacement.

Common refusal codes:

| Code | Meaning / next action |
| --- | --- |
| `SESSION_PROTOCOL_UNSUPPORTED` | The identified original host or pending launch is outside the target's support range; use a supporting connector. |
| `HOST_PROGRAM_UNAVAILABLE` | The original retained program is missing, unsafe or has a different digest; restore that exact program before switching. |
| `REGISTRATION_INVALID`, `REGISTRY_UNAVAILABLE` | The independent evidence cannot be verified; investigate before switching. |
| `RUNTIME_DIRECTORY_INVALID`, `INSTANCE_MARKER_INVALID` | Original resource identity cannot be established. No adoption or cleanup is attempted. |
| `LAUNCH_IN_PROGRESS` | A launch or another switch holds the gate. Retry the upgrade after it completes; do not replay the launch. |
| `UPGRADE_CHECK_DEADLINE` | The bounded check could not complete. No service switch was attempted. |
| `LEGACY_CONNECTOR_REQUIRES_EXPLICIT_TRANSITION` | An active connector has no independent registry and may still own ACP in memory. |

Rollback uses the same procedure: execute the selected older distribution's
`upgrade-check` and `upgrade`. A supported rollback reconnects existing hosts;
it never replaces their Agent or controller. There is no force option that
silently stops incompatible sessions. Explicit `new`/`load` continues to use the
original host's retained program for its normal Agent replacement contract.

Each host retains an independent executable inode under its private Runtime
directory. Guardian and MCP stdio bridge use that same executable. Removing the
old installation release cannot remove these copies; only accepted `forget`
cleans them through the original resource plan. A retiring Runtime already has
admission sealed and its Agent confirmed ended, so preflight permits its program
to be absent while cleanup continues.

The first move from in-process ACP is an explicit transition boundary. The new
connector cannot preserve or reconstruct an old connector's controller memory.
If the independent registry is missing while `fabricd.lock` is held, preflight
refuses before stopping the old connector. Use the old public Runtime inventory
to identify affected sessions, finish or explicitly stop them, and stop that
connector before installation. Preflight cannot invent Runtime identities absent
from the old process's disk evidence. There is no compatibility adapter or
automatic termination for that first transition.

Local tests use real fabricd/host/Agent processes with isolated service-command
substitutes. They prove guard placement, program checks, sealed launch refusal,
and process continuity across installation switches. They do not prove native
systemd/LaunchAgent cleanup semantics, real-Agent behavior, or different-version
tmux client/server interoperability; those require the separate acceptance run.

## Actual running versions

`dune version` prints the executing command's embedded version, Git revision and
modified flag, Go/OS/architecture and supported protocol range as JSON. It needs
no machine configuration and makes no network connection. Release builds may
set `internal/buildinfo.Version` using Go's `-ldflags -X`; the VCS identity comes
from the compiled program, never a later checkout or the `current` symlink.

`dune --config /path/to/config.yaml version --runner` additionally uses the public
SDK/Gateway path to read `machine.info` and `runtime.list`. It reports the actual
connector PID, initialization time and embedded build; each original host's
embedded build, retained digest and process identities; and separate tmux client
and server versions. Cached server observations include their check time. Missing
or unavailable servers have explicit states, not an inferred installed version.
Concurrent machine-info queries share one pair of version probes at most every
five seconds, with independent two-second deadlines.

The tmux client uses `-V` only for its own version. Server version/PID come from
`#{version}`/`#{pid}` over the existing private socket, with `-N` prohibiting a
new server. No host attach, new/load, prompt or permission action is part of these
diagnostic reads. Version reporting does not claim cross-version IPC support
beyond the declared host range or the separately tested tmux distribution.
