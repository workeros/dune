# ACP session lifecycle implementation

This work implements the reviewed SandDance requirement
`docs/dune-acp-session-lifecycle-requirements.md` (55 acceptance cases), against
Dune baseline `bd17acc`. The requirement describes the target, not capabilities
already shipped. Implementation uses tracer bullets: each slice must establish
an observable contract and its verification before the next slice expands it.

## Slices

1. Public submission identity, admission observations, and a persistent registry
   with atomic key ownership. Prove that a read cannot reject a late request,
   duplicate keys cannot acquire execution twice, and ownership survives reopen.
2. Caller-owned submission IDs through SDK, host, Gateway, and fabricd. Prove
   receipt lookup after an interrupted response over the public network route.
3. A tmux-hosted ACP process and private IPC. Move the existing controller into
   that process and preserve one real Agent across connector restart.
4. Managed queue, model, permission, and operation continuity; controller fencing
   and bounded control reservations. Exercise concurrent clients and faults.
5. Raw ACP complete-message admission, ordered writes, bounded output resumption,
   and permanent input failure after an unrecoverable partial write.
6. Discovery availability, independent forget admission and cleanup recovery,
   deadlines, budgets, and all public consumers.
7. Upgrade preflight, pinned dependencies, release artifacts, platform and real
   Agent acceptance, and SandDance public-boundary integration.

Each completed slice is committed separately; a slice may require several
commits. A passing unit test is not evidence of process survival. L01–L55 remain
unverified until the corresponding execution evidence is recorded below.

## Decisions

- Transport request IDs and connection epochs do not identify business submits.
  Callers create `submission_id` before their first send.
- Launch keys bind an owner and complete Runner binding; Runtime keys also bind
  the original Runtime incarnation and generation. Forget shares that namespace.
- A durable key claim only prevents competing execution. It is not an admission
  receipt. Queries of an uncommitted claim remain `unknown`.
- Keep the existing ACP protocol/model implementation. Explicit new/load retains
  its process replacement and old-callback isolation independently of IPC reconnect.
- Persist minimum identity and admission evidence, not Agent prompts or secrets.
- Ordinary capacity cannot consume stop, cancel, permission, or cleanup reserves.

## Verification record

Initial environment: macOS arm64, Go 1.27.1, bundled `bin/tmux` and protobuf tools
available. `DUNE_REAL_AGENT`, `DUNE_TEST_POSTGRES`, and `DUNE_NATIVE_CODEX_HOME`
were not configured. No business Runner has been restarted.

Slice 1 foundation: `go test -race ./pkg/api ./internal/sessionregistry -count=1
-timeout=90s` passes. Tests cover caller key validation and error preservation,
read-before-admission, uncommitted claims after reopen, irreversible rejection,
scope isolation, cross-receiver conflicts, concurrent SQLite connections, three
real processes claiming one key, and unsafe registry paths. Test directories are
explicitly made private; the implementation does not relax its 0700 requirement.

The registry currently provides bounded minimum evidence and exclusive claims.
It is not yet connected to public execution, and protected control reservations,
safe evidence reclamation, host IPC, and lifecycle cleanup are subsequent slices.
These foundation tests do not mark any L01–L55 end-to-end case passed.

Slice 2 read path: `Client.QuerySubmission` follows SDK → Gateway → fabricd →
the independent registry. It validates the caller's complete key, preserves that
key and context/structured errors, bypasses transport response caching, and does
not require an active Runtime. Hosted access checks owner, complete binding and
outer Runtime selectors before permitting the read.

`go test ./tests -run '^TestSubmissionQueryAcrossFabricdRestart$' -count=1
-timeout=90s -v` passes with a real Gateway and replacement fabricd process after
SIGKILL. The test seeds admission evidence directly in the registry, then proves
the public read returns the same receipt after restart without creating an Agent.
This is a receipt-read tracer bullet, not Agent survival or public write admission.

Targeted race checks for QuerySubmission, submission authorization, and the
engine state lock pass. `go vet ./pkg/api ./internal/sessionregistry ./pkg/client
./pkg/access ./pkg/fabricd` passes.
The full `go test -race ./pkg/client ./pkg/access ./pkg/fabricd -count=1
-timeout=180s` also passes (including existing authorization, ACP and PTY tests).

Slice 2 write path: `Client.Submit` carries a caller-owned Runtime key through
Gateway authorization to the existing ACP queue. The queue persists admission
before dispatch, including two keys sharing an in-flight load operation. Exact
duplicates return the original operation, changed requests conflict, and durable
validation rejection returns `not_accepted` with its original structured code.
SDK errors retain the original key and any validated admission receipt.

`go test -race ./pkg/api ./pkg/client ./pkg/fabricd ./pkg/access -count=1
-timeout=180s` passes. `go test ./tests -run
'^Test(ManagedSubmissionAdmissionThroughGateway|SubmissionQueryAcrossFabricdRestart)$'
-count=1 -timeout=100s -v` passes. The mock RPC log contains exactly one new and
one prompt after duplicate and rejected submissions. Unit coverage verifies
shared loads and that failed admission persistence dispatches no Agent RPC.

The feature branch still has the old consumer entry points pending migration;
this is an intermediate slice, not a compatibility commitment. Agent lifetime
is still owned by fabricd at this point. Protected controls, launch admission,
independent host ownership, and L01–L55 process acceptance remain outstanding.

Slice 3 process tracer bullet: managed ACP now runs inside `_acp_host`, hosted
by a separate private tmux server under the installation's `acp` directory.
The host reuses the original controller, queue, model, operations and process
guardian. The connector stores only an IPC proxy. Agent argv/environment use
a private bootstrap file removed by the host; tmux receives no protocol data.
The Agent has anonymous stdin/stdout pipes and separately consumed stderr.

Private Yamux IPC validates installation, machine, host instance and full Runtime
identity. A persistent connector control term fences older connectors; takeover
is serialized with business admission. Read-only handshakes do not take control.
Engine.Close closes proxies while leaving independent hosts alive. Discovery
reopens the original endpoint without initialize/new/load. Unreachable proxies
keep their last lifecycle description with `availability: unavailable`.

`go test -race ./pkg/access ./pkg/fabricd -count=1 -timeout=180s` passes.
`go test ./pkg/host ./tests -run
'Test(ACPConversation|ManagedACPOriginal|ManagedSubmission)' -count=1
-timeout=180s` passes (pkg/host had no matching tests).
`TestManagedACPOriginalProcessAcrossConnectorRestart` uses real tmux, Gateway,
host, Agent and replacement fabricd processes. After SIGKILL, blocked and queued
prompts finish while fabricd is absent; their original receipts and operations
remain readable. A subsequent SIGTERM/restart preserves the same permission and
model. A single Agent process log confirms genuine pipes and no extra
initialize/new/load. Existing explicit load/generation barrier tests pass.
`TestSessionHostControlFencesOlderConnectorsAndProbesAreReadOnly` and private-file
checks pass under race detection. Client/tmux race checks also pass.

Still pending: raw ACP ownership, independent launch/forget admission (the
existing forget execution is only routed cleanup at this slice), shared machine
quotas, protected control evidence, complete discovery diagnostics, fixed binary
dependencies, service-manager/platform tests and public consumer contract migration.
No full L01–L55 completion claim is made by this process tracer bullet.

Slice 4 admission-resource foundation: the independent registry now separates
ordinary keys from permission/cancel reservations and per-Runtime stop/forget
slots. Defaults are 4,096 ordinary keys and 4,096 retained reservations per
permission/cancel class (configuration range 1..65,536). Stop and forget are
reserved separately when creating a Runtime resource record. The resource
ledger limits live ACP hosts to 16 (16 × 8 MiB = 128 MiB model/stream budget)
and retains at most 256 Runtime identity records. These are target resource
limits; the next slice wires every launch/control to this ledger.

Launch admission can atomically reserve its Runtime identity and controls before
side effects. Forget admission atomically seals that Runtime and records cleaning;
late unaccepted claims cannot commit across the seal. Confirmed completion
releases its live-host slot while retaining the original key and identity closure.
Progress and Runtime association survive registry reopen; reading never advances
cleanup. Completed control evidence is not freed to admit another control key.

`go test -race ./internal/sessionregistry -count=1 -timeout=90s` passes. New tests
exhaust ordinary and per-control limits, preserve protected controls and reads,
reject 100 invalid control targets without consuming valid reservations, preserve
cross-operation key conflicts, and reopen cleanup evidence before completion.
Public control/launch dispatch is not yet migrated, so this is evidence for the
registry contract, not a passed L51–L55 network/process acceptance claim.

Slice 5 launch tracer bullet: `Client.Start` now requires `api.StartRequest` with
an explicit caller-owned key and returns `api.StartResult`. No implicit-ID
startup overload remains. Launcher, HTTP/MCP requests, Web, IM AgentConnection
and repository test callers now supply the ID before calling. The Web retains
its latest launch identity in sessionStorage before the network send; a complete
pending-submission UI remains part of the consumer slice.

A single independent registry transaction admits the frozen launch, reserves
its Runtime identity and stop/forget capacity, and precedes optional worktree
creation, setup and Agent launch. Worktree creation stays separately authorized
at the Gateway. Confirmed worktree and stage facts survive connector restart;
Agent setup errors retain their live bounded diagnostic payload. The independent
host can publish its own started checkpoint when the original response is lost.
Duplicate launches return ALREADY_SUBMITTED plus the original receipt, without
repeating worktree/setup or Launcher initialization.

`go test -race ./pkg/access ./pkg/host ./pkg/fabricd ./pkg/client
./internal/sessionregistry -count=1 -timeout=180s` passes. Targeted contracts
passed again after removing obsolete Agent branches from Profile prepare.
`TestLaunchAdmissionSurvivesMissingFirstResponse` passes for caller cancellation
before startup completion and SIGKILL during setup: the first branch retains one
Agent across connector replacement; the second retains accepted/setup without
replaying setup or fabricating Agent startup. The test waits for Agent readiness
before reading the mock process marker (process spawn confirmation precedes mock
main execution). Profile/PTY, prefixed Web launch and managed ACP regressions pass.
`go test ./duneagent -count=1 -timeout=90s` in im passes; full IM integration is
still outstanding. `make web-check web-build` passes with bundle-size warnings.

Remaining integration work includes all Runtime actions/control reservations,
independent cleanup recovery, raw ACP, full discovery/loss classification, pinned
runtime dependencies, platform manager/upgrade acceptance and SandDance consumers.

Slice 6 managed submission/control tracer bullet: ACPSubmit now takes the caller's
complete key. Prompt/OpenSession, initial Launcher new, IM AgentAction, and Web
ordinary submits supply caller-owned IDs; initial admission snapshots retain
receipts across local errors and optional waits. IM derives IDs from its persisted
session key/revision and action before sending. Web saves its latest submission
ID and original agent_ref before sending; a complete recovery UI remains pending.

Permissions reserve their response before publication; prompts reserve their
exact cancel target before queue admission. Controls validate their live target
before consuming a reservation, persist admission before stdin, and record written
or input_unrecoverable independently of ordinary operation slots. Duplicate lookup
precedes lifecycle validation, so completed targets still return the original
receipt. Ended unused permissions/queued prompts release only unused reservations.
State/model/operation reads bypass the ordinary transport cache and use bounded
independent concurrency.

`go test -race ./internal/sessionregistry ./pkg/fabricd ./pkg/client ./pkg/host
./pkg/access -count=1 -timeout=180s` passes. Additional targeted race tests pass
for cancelled SDK submits preserving identity, reads with a full pending transport
cache, ended-control release, and ordinary evidence/operation exhaustion with
40 invalid answers and 20 concurrent duplicate answers producing one permission
response plus one independently reserved cancel. Completed controls cannot fund
new controls by discarding their evidence.

The real-process `TestManagedACPOriginalProcessAcrossConnectorRestart` now also
uses ACPControl and QuerySubmission after connector restart, checking written
permission evidence and duplicate identity. It passes along with submission,
managed offline permissions, history replay and generation-barrier regressions.
IM duneagent tests and Web typecheck/build pass (existing bundle-size warnings).
The old direct acp.action network route is still awaiting removal together with
Web controls/history migration; stop/forget admission, raw ACP, discovery/upgrade
and complete L01–L55 acceptance remain outstanding. These are intermediate
commits, not a completed lifecycle delivery.

Slice 7 public managed submission route: removed direct acp.action from fabricd
and host IPC operation allowlists/capabilities. All managed network actions use
the caller-key envelope; access checks still inspect the actual business action.
Web permissions/cancel/history now use AgentMessenger.Submit, with the exact
active prompt reference for cancel. Host/HTTP/MCP expose Submit and QuerySubmission
using original agent_ref + submission_id. Receipt lookup bypasses native-session
validation, and host pre-service cancellation retains caller identity.

The initial host race run exposed remaining references to the removed transport
capability; those callsites now select submission.acp while preserving the business
authorization vocabulary. `go test -race ./pkg/host -count=1 -timeout=180s` passes
after that fix. Access/fabricd/internal agentmcp/webapp race suites pass; added
access tests reject unidentified actions. The public host test proves one execution
per key, old-key lookup after explicit native replacement, local cancellation
identity retention and cross-owner rejection. Generation barriers, process restart,
shared operations and managed permissions process tests pass. The old cancellation
test now supplies its exact observed operation_ref as required by the new schema.

`make web-check web-build` and `npm --prefix web run e2e -- agent-operations.spec.ts`
pass (3 Chromium cases). Browser controls save caller identity before network send;
normal operation querying remains tied to original operation references. Complete
multi-submission persistence/recovery UI is still pending; the current browser
retains only its most recent raw submission identity in sessionStorage.
