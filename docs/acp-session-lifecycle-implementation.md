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

Slice 8 browser recovery: managed ACP submissions now persist bounded recovery
selectors before sending (64 ordinary records and 512 separately reserved control
records, with a 4 MiB serialized cap). Records contain no task or permission
bodies. The Runtime panel can query the original receipt after page reload, then
read the original operation when retained. Unknown stays unknown, and the UI never
resubmits to discover an outcome. Removing a local tracking record is explicit and
does not send an Agent control request. Receipt updates validate the original
binding and complete Runtime identity.

`make web-check web-build`, `npm --prefix web test` (11 cases), and
`npm --prefix web run e2e -- agent-operations.spec.ts` (4 Chromium cases) pass.
The new browser test aborts the first prompt response, reloads, observes unknown
and later accepted/completed through the original key, and verifies exactly one
submission plus no prompt text in storage. Recovery screenshot inspected. Launch
recovery still uses its earlier single lastLaunch record and needs equivalent
multi-record UI; this slice only closes Runtime-action browser recovery.

Slice 9 IM continuity: removed the implicit replacement/load/new branch from
Backend.Attach. Stale lookup, lost/exited Runtime and temporarily unavailable host
now preserve the original selection and return an error. A new Runtime requires
explicit caller intent. PromptRequest now carries a mandatory caller submission
ID; Processor derives it from the persisted inbound binding/event identity. Direct
Backend callers supply and retain their own ID. This replaces the earlier prompt
ID derivation from session revision, which a direct caller could reuse across
separate turns. Launch/initial-new still derive stable IDs from their persisted
session revision before sending.

The first full IM regression exposed two direct calls using the same revision and
thus correctly deduplicating to the first answer. After making turn identity
explicit and migrating callers, `make test-im-race check-im` passes across channel,
duneagent, feishu and sqlite. The real local Gateway/Agent integration now checks
live attachment, independent turns, rejection of implicit replacement after stop,
and an explicit fresh start while the other conversation continues. Targeted
Backend race tests verify missing ID rejection and propagation of the caller's
original ID/conversation generation. No real Feishu or model service was contacted.

Slice 10 stop tracer bullet: Client.Stop and runtime.stop require the caller's
original Runtime submission key and return an admission receipt. Stop atomically
consumes its dedicated reservation together with accepted/stopping, without an
intermediate claim that caller cancellation could strand. It bypasses ordinary
transport caching and operation/key capacity. After admission the host completes
the stop independently of the calling stream. Only observed guardian-group exit
and durable result registration produce stage stopped. Duplicate stops return
the original receipt even after PTY removal from the active Runtime map.

Process ownership now serializes replacement spawn/publication against stopping.
A stop closes the current ownership pipe immediately; a concurrently spawned
replacement is published before ACP locks so it is also stopped and awaited.
Once stop is confirmed, an explicit open cannot spawn a new process. Host shutdown
drains request handlers and process confirmation before closing the registry.

AgentMessenger HTTP/MCP supports action stop for ACP and PTY. AgentConnection and
IM supply the stop ID before sending; IM polls that original receipt, and no longer
treats STALE_RUNTIME as successful stop. Web saves stop identity before sending,
reserves separate stop/forget tracking slots, and exposes all saved submissions
outside Runtime panels so removed/unavailable Runtimes remain queryable.

Targeted race tests cover cancelled atomic admission, 20 duplicate stop contenders,
ordinary key/cache exhaustion, caller cancellation during replacement spawn, a
blocked ACP input lock, and preventing spawn after confirmed stop. The real-process
TestStopReceiptSurvivesLostResponseAndConnectorRestart holds the actual stop result
before SDK delivery, cancels the caller, SIGKILLs/restarts fabricd, then reads the
original stopped receipt and checks that a neighboring Agent remains running.
It passes along with managed survival, permission, submission and explicit
generation-barrier process regressions. The host consumer test also stops by an
original Agent reference after its native session has explicitly changed.

The five Chromium agent-operations cases pass, including lost stop response,
Runtime removal and reload followed by original receipt lookup; screenshot
inspected. Web typecheck/build passes with existing bundle-size warnings.
`go test -race ./pkg/fabricd ./pkg/host ./internal/sessionregistry ./pkg/client
./pkg/access -count=1 -timeout=180s`, `make test-im-race check-im`, and affected
Go package vet checks pass. These checks include existing PTY regressions.

This slice does not complete L27/L51–L55. Durable forget dispatch, independent
cleanup resource records/recovery, loss proof after host death, protected IPC
stream scheduling under saturation, raw ACP hosting, pinned dependencies and
platform/service-manager acceptance remain outstanding. No business Runner was
restarted and no real Agent service was used.

Slice 11 independent loss evidence: a guardian now waits behind a launch gate
until the host has durably registered its group. Host death or uncertain commit
before that gate cannot execute the Agent. The independent registry records one
immutable host activation, kernel boot identity and current process generation
per reserved Runtime; an explicit replacement requires the previous group to
have exited. New group registration checks the same admission seal used by
cleanup. Terminal host state cannot be reversed into a new process.

Discovery reads this bounded host index, so missing Runtime files no longer hide
the original identity. A failed handshake preserves unavailable; independent
proof that both original host and group are absent yields lost and SESSION_LOST
for operation access. No persisted PID is signalled, and loss never manufactures
task success or replays the accepted request. The Web preserves the lost panel
and does not open another process or stream for it.

Process/registry race tests pass for no child before registration, uncertain
registration rejection, owner death between registration and gate release,
cross-instance/generation fencing, terminal state and cleanup seals, independent
capacity, and record reopen. Existing stop/restart and explicit generation-barrier
process tests pass. TestHostLossUsesIndependentEvidenceAndDoesNotReplay passes
with a real blocked mock Agent: a genuinely suspended host remains unavailable;
after SIGKILL and guardian cleanup, deleting the Runtime directory and restarting
fabricd still discovers lost, preserves the original accepted key and creates no
replacement. Its fault injection suspends the isolated tmux supervisor as well,
because tmux otherwise resumes stopped pane children. This does not touch a
business Runner or any shared tmux server.

The first broad host regression exposed transient EPERM from the macOS group
existence check during process reaping. EPERM now remains inconclusive within
the bounded wait; it never proves absence. The targeted host Messenger regression
then passes five runs. Nine workbench Chromium cases pass, including lost-panel
retention, and Web typecheck/build passes with existing bundle-size warnings.
The final `go test -race ./pkg/host ./pkg/fabricd ./internal/process
./internal/sessionregistry -count=1 -timeout=180s` and affected-package vet checks
pass. The Dune command builds for Linux/macOS amd64/arm64; these are compile
checks, not platform runtime or service-manager acceptance. Lost-panel screenshot
inspected. No real Agent service was called.

Remaining cleanup work: record fixed filesystem/tmux/socket resource identities,
atomically accept forget with the resource plan and namespace seal, fence and
resume only original idempotent cleanup, and retain completed independent receipts.
This evidence foundation does not mark L51/L52/L55 complete; raw ACP, complete
discovery issue reporting, stream control reservations, retained-host reclamation,
upgrade/binary pinning and target-platform service-manager acceptance also remain.

Slice 12 fixed cleanup admission and evidence: replaced ClaimControl plus
AcceptForget with one transaction that checks the original shared key before
lifecycle verification, verifies the exact host snapshot under the same database
boundary as group registration, consumes the preallocated forget slot, freezes
the resource plan and seals the Runtime namespace. Neither cancellation nor a
failed exit/loss proof consumes the cleanup reservation. Stop/forget can no
longer use the non-atomic control-claim path. A late host state publication also
checks the namespace seal.

Before its Agent start gate, the host registers the Runtime directory and socket
device/inode identities plus its instance marker. tmux receives the instance
marker atomically with session creation. Host retirement checks that marker and
targets an immutable tmux session ID inside a conditional server command, keeping
a reused session name or neighboring host intact. Runtime directory deletion and
socket retirement still need the checked-handle cleanup executor; recording these
identities alone is not filesystem cleanup acceptance.

Cleanup plans and three ordered checkpoints live outside the Runtime directory.
The public receipt now exposes confirmed/remaining cleanup steps without exposing
paths or process-control authority. Generic progress cannot bypass those steps.
Executor terms fence late checkpoint publication; physical actions must also
retain the existing fabricd.lock until drained. Only the final confirmed step
releases the live Runtime reservation. Read-only receipt/pending-plan queries do
not bind an executor or advance cleanup. Completed receipts, plans and identity
seals remain bounded by the retained Runtime pool.

Targeted tests cover ordinary evidence exhaustion, cancelled admission, twenty
contenders across two registry connections, shared-key conflicts before live/lost
verification, a paused loss proof racing group registration, each checkpoint
across registry reopen, stale executor publication, no generic completion bypass,
and tmux instance/name reuse. The broader registry race suite exposed a journal
open/unlink race: an optional SQLite journal can be unlinked after its descriptor
is opened. The validator now permits that already-unlinked optional descriptor
while still requiring the main database to remain linked and rejecting hardlinks.
The cross-process admission test then passed ten race-enabled repetitions.

The real-process connector survival, independent host loss, lost stop response,
and explicit ACP generation-barrier regressions pass together. The affected
fabricd/client/API and tmux race suites pass. This slice does not connect the
network forget request or run a cleanup recovery executor; those remain the next
tracer bullet, including filesystem path reuse and L51/L52/L55 process barriers.
No L51–L55 acceptance claim is made from registry-only tests. No business Runner
or real Agent service was used.

Final slice checks: `go test -race ./internal/sessionregistry -count=1
-timeout=60s` and affected-package `go vet` pass after the journal fix. The tmux
instance cleanup test also covers a missing marker, which must remain an identity
error rather than being reported as resource absence. It passes with race enabled.
Runtime launch now exclusively creates its directory and cannot overwrite an
existing instance marker/bootstrap; the connector-survival process test passes
again after that change.


Slice 13 public forget and recoverable physical cleanup: Client.Forget and the
identified runtime.forget route now reach independent admission directly. The
unidentified route and direct RemoveAll cleanup were removed. HTTP/MCP action
forget uses the original Agent selector without requiring its current native
session or an active Runtime entry. The Web saves the exact forget identity before
sending, permits cleanup of a confirmed lost Runtime, and preserves step/error
observations across reload and Runtime removal.

The executor retires the exact tmux instance, confirms original host/group absence,
then cleans the recorded socket and Runtime directory. Filesystem cleanup uses a
non-overwriting rename into a fixed quarantine, revalidates inode/instance evidence,
and operates through pinned directory handles. It does not follow project symlinks.
A host destructor no longer unlinks its old socket pathname. Interrupted emptying
resumes with the original instance marker; a crash after marker removal can only
remove the empty, matching quarantine. Resource identity changes retain accepted
with an unconfirmed step and do not authorize deletion of the replacement.

PTY uses the same key/receipt/seal namespace and reserved cleanup slot, with a
fixed ended-Runtime plan for its terminal and Dune-generated timeout/native-helper
caches. A durable stopped receipt permits cleanup after explicit PTY stop has
already removed the active entry and caches. The existing timeout/history and
installer-upgrade PTY test passes with the new caller-key contract. This preserves
PTY behavior; it does not establish raw ACP host continuity.

Startup recovery and a bounded 30-second scheduler resume only previously admitted
plans. Duplicate submissions and all reads do not schedule cleanup. A per-key
execution map prevents parallel execution within a connector; active work retains
fabricd.lock through shutdown, with durable executor terms fencing late checkpoints.
The scheduler and actions drain before the installation lock can pass to another
connector. Individual execution attempts have a 20-second deadline, and unfinished
steps retain honest error/stage evidence for later recovery.

New real-process tests run through Gateway and SDK against isolated test fabricd,
host and guardian processes. TestForgetResumesOnlyOriginalCleanupAfterProcessCrashes
SIGKILLs fabricd at six barriers: after acceptance, host retirement, IPC quarantine,
Runtime contents deletion, marker deletion, and physical directory removal before
its final checkpoint. Each replacement executor pauses before resumption while
three public queries return the unchanged original receipt. Releasing it completes
only the original plan; duplicate forget returns that receipt and project/native
history remains intact. A late host business request after acceptance is sealed.
TestForgetConfirmedLostHostNeedsNoStopOrReplacement uses a real mock ACP Agent:
an original accepted host key conflicts with forget both before and after host
SIGKILL; a new legal key cleans up without stop or a replacement host, and the
original operation's admission is unchanged. Both tests pass with race enabled.

Filesystem tests cover all quarantine/marker interruption boundaries, original
and quarantine path reuse, and external project symlinks. The engine-fencing test
suspends a real directory cleanup, requests Close, verifies a competing Open cannot
acquire the installation lock, then drains the old action and verifies the new
engine resumes the original plan. These pass with race enabled. Public host tests
also query completed cleanup after the original native session has changed.

Validation: affected sessionregistry/tmux/fabricd/access/client/host race suites
pass; targeted crash/loss and shutdown-lock tests pass after the final changes.
The existing connector-survival, host-loss, lost-stop-response and explicit
new/load generation-barrier process regressions pass together. Fifteen Chromium
workbench/agent-operation cases pass, including lost forget response, Runtime
removal, reload and original progress lookup without another send or Agent start;
the resulting screenshot was inspected. Web typecheck/build passes with the
existing bundle-size warnings. The main-module compile-only test sweep passes;
this is not a full main-module regression run.

These are local managed-ACP cleanup observations for L27/L51/L52/L55, not the full
L01–L55/platform acceptance audit. Protected IPC/connector stream scheduling and
all saturation dimensions in L53/L54 remain to be implemented and tested. Raw ACP
independent hosting/input integrity, full discovery issue reporting, retention,
pinned dependencies, upgrade/rollback preflight, remaining public-consumer recovery,
SandDance integration, four-platform runtime/service-manager tests and real Agent
evidence also remain. No business Runner or external Agent service was used.

Final checks for this slice: internal agentservice/agentmcp tests, Web unit tests
and affected-package vet pass. Dune builds for Linux/macOS amd64/arm64 using the
new platform-specific non-replacing rename operations; these are compile checks,
not Linux or service-manager runtime acceptance. The real-Agent/PostgreSQL/native
Agent acceptance environment switches remain unset.

## Slice 14 — protected transport capacity through the public route

Ordinary in-flight streams no longer occupy the first-message readers. A shared
wire admission component bounds undecoded first requests separately and computes
the execution class from the actual decoded operation/envelope and exact Runtime
selector. Permission/cancel, stop, forget, immediate reads and long polls each
have independent concurrency budgets. All classes still pass binding, policy,
target and business admission checks. First-message bodies remain bounded at
4 MiB with a five-second read deadline; excess intake is closed without claiming
business rejection. No unbounded reader goroutine or priority hint was introduced.

Ordinary concurrency stays 64 per SDK/Gateway connection and per fabricd/host
Engine, 512 globally in Gateway. Each protected class has 16 slots (128 globally
in Gateway); first-message readers have 8 (64 globally). Engine budgets span old
and new connections while accepted handlers drain. Gateway Drain/Disconnect still
wait for classified forwarding and callbacks. SDK reserves the class before
opening a stream and releases it on close/cancellation. The Yamux backlog now
covers these bounded reservations instead of enforcing the old ordinary-only
ceiling. Binding limits, machine.info and Gateway status expose the categories.

Long operation waits have a separate handler budget, so waiting for permission
cannot consume all current-state readers. The session proxy no longer holds its
connection mutex while opening or writing an ordinary stream; a separate stop
can be delivered while a large ordinary message is blocked at its Yamux window.
Cancellation closes that original stream; it does not redial or replay it.

Evidence added for this slice:

- Gateway exercises eight connections with 64 real forwarded streams each,
  rejects additional ordinary work at both connection/global limits, then
  fills permission/cancel/wait classes and still forwards reads, stop and forget.
  Forged control envelopes cannot borrow reservations, denied controls never
  reach fabricd, all classes drain, and unread first-message slots remain bounded.
- A process test starts eight independent managed hosts with eight existing
  observation slots apiece. It fills all 64 ordinary streams, SIGKILLs fabricd,
  reconnects to the same hosts and refills those streams. A second SDK confirms
  that the fabricd-wide ordinary gate also rejects excess work. Original receipt
  and state queries remain readable with 16 pending operation waits, eligible
  forget completes, a valid permission is answered, cancel reaches the original
  prompt and stop proves process exit. Repeated/invalid permission requests do
  not produce additional Agent responses. RPC/process logs confirm one Agent,
  one initialize/new, two original prompts and one cancel.
- A blocked 512 KiB IPC request exceeds its 256 KiB Yamux window; the same proxy
  delivers stop before consuming any of that request. Context cancellation then
  unblocks the original writer. The test would time out with the old mutex scope.

The new process and wire/Gateway tests pass with race enabled. This establishes
transport saturation behavior, not the complete L53/L54 matrix. Separate process
evidence for queue/results/retained-key/machine-registration exhaustion and
completed control evidence exhaustion is still required. Raw ACP host/input
integrity, discovery completeness and diagnostics, pinned dependencies and safe
upgrades, remaining consumer recovery/SandDance integration, full L01–L55 audit,
target-platform service-manager acceptance and real-Agent evidence remain open.

Final validation for slice 14: `go test -race ./internal/wire ./pkg/client
./pkg/gateway ./pkg/transport/... ./pkg/sdk ./pkg/fabricd ./pkg/host -count=1
-timeout=180s` passes (sdk and ws contain no tests). The affected-package vet run
passes. The existing original-process reconnect, host-loss, lost-stop-response
and explicit conversation-generation-barrier process tests pass together via
`go test -race ./tests` with those four test names selected. No Web files changed;
browser tests were not rerun. External Agent/PostgreSQL/native-Agent environment
switches remain unset; these results use isolated local mock Agents only.

## Slice 15 — independent ordinary-capacity evidence and diagnostics

machine.info now reports a single read-only registry snapshot of ordinary keys,
Runtime/identity reservations, and each control class's reserved/claimed/accepted/
rejected/completed evidence. Completed controls remain included in their occupied
class budget. acp.state reports the original host's queue, permission and operation
result usage, output bytes/update counts and original completion/expiry times.
Diagnostics cannot allocate submission records or turn an unavailable registry
into an empty-capacity report.

Operation results now keep their existing 64-record, 512 KiB/1024-update and
15-minute bounds while refusing new ordinary work when all unexpired slots are
occupied. The previous eager eviction of completed records under pressure did
not meet L53's explicit refusal at result saturation. Pending/running results
still have no completion TTL; connector restart does not change host timestamps.
This change applies to managed ACP; PTY delivery receipts retain their existing
oldest-completed pressure eviction, with a separate regression test.
Queue and result exhaustion return SUBMISSION_CAPACITY_EXHAUSTED with the affected
category. Durable rejection is saved before exposing not_accepted when space for
the original key was obtained; a full key ledger still cannot manufacture it.

TestEachOrdinaryCapacityPreservesOriginalControlsAcrossConnectorCrash exercises
four separate local process scenarios with unchanged production defaults:

- 32 pending prompts while result slots and ordinary keys remain available;
- 64 results (62 completed, two active/queued), with only one queued prompt;
- 4096 ordinary keys, including 4091 historical rejections prepared through the
  real registry ClaimKey/Reject API, with just three operation records;
- 16 actual registered hosts while ordinary key and operation budgets remain free.

Each scenario retains an active permission and a queued cancellable prompt,
SIGKILLs fabricd, reconnects to the original host, and compares the original
resource snapshot and completion clock. Repeated public queries do not change
durable counts. New ordinary work is refused in the corresponding dimension;
effective permission/cancel/stop and an already-ended Runtime's forget complete.
Agent process/RPC/history logs verify original identity, actual control delivery,
and absence of the refused prompt. Before/after class usage and hard limits are
included in test output. All four scenarios passed with race enabled, including
the default 4096-key scenario; historical evidence is a fixture, not a claim of
4091 historical Agent executions.

Registry unit coverage verifies consistent independent counts after reopening
and proves observations add no evidence. Operation tests verify a full result
table preserves its earliest unexpired output, allows new work only after an
actual completion TTL expires, and retains unfinished work. These pass with race.

L54 still needs a separate process test for completed permission/cancel evidence
at its budget, competing/expired submissions and another effective target's
reservation. This slice also does not complete raw ACP hosting/input, discovery,
dependency pinning/upgrades, remaining consumer recovery, SandDance integration,
full L01–L55 audit, platform service isolation or real-Agent acceptance.

Validation for slice 15: sessionregistry/client/access/host race suites and
affected-package vet pass. The initial full fabricd race run found an existing
search assertion that required all independent file errors even after a bounded
oversized-record abort. Commit cbe3e19 splits those cases; the test passes ten
race repetitions and the full fabricd race rerun passes. Targeted result-capacity
and PTY-policy regressions pass after explicitly retaining PTY's existing eviction
policy. Managed queue/history/offline-permission/generation-barrier/original-host
process regressions pass together. Agentservice, agentmcp and webapp also compile;
this compile-only check is not their full test suite. No real-Agent or external
service acceptance is claimed.

After the final policy split, the real-tmux
TestPTYOperationsShareBrowserInputAndExpireOnFabricdRestart process regression
also passes with race enabled.
