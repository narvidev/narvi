# test/resilience

Automated replay of the known failure scenarios that are this design's
differentiator — §9.3 (`docs/TECHNICAL_PLAN.md`), the phase-2 exit gate,
not an afterthought:

> These run as automated scenarios against a real (or provider-faked)
> stack.

This is the definitive scenario-by-scenario index for that exit
criterion: for each of the 12 original §9.3 scenarios plus the 4 new
warm-boot-class scenarios Step 42 (§19.2/§19.4/§19.5/§19.7) adds below,
exactly one of the following is true, and stated plainly rather than
overstated —

- **covered** — a real test proves it, cited by exact function name and
  file path (verified by grepping the repo while writing this doc, not
  carried over from memory);
- **covered with an accepted gap** — a real test proves the reachable
  part of it; the remainder is a deliberate, named, user-confirmed
  decision, not an oversight;
- **reserved for a follow-up PR** — the harness this PR builds is ready
  for it, but the scenario itself isn't written yet;
- **deferred to a later phase** — it genuinely cannot be built yet,
  because what it exercises doesn't exist yet (named by exact Step/PR
  number).

## The harness

`harness_test.go` builds a real, reusable "mini control plane" — one
throwaway Postgres (via testcontainers, every embedded migration
applied), the real Postgres store types, and a real `wshub.Hub` — wired
together with only exported APIs, the same way `cmd/control-plane/main.go`
itself wires them. `Harness.NewRegistry` constructs a real
`sessionactor.Registry` sharing that same pool/hub, callable more than
once per test to simulate more than one "pod" against the same database
(a rolling restart, a fresh replica picking a session back up). This is
deliberately a more realistic, less white-box style than the existing
internal-package integration tests — appropriate for scenarios that
genuinely span multiple packages, not one package's own internals in
isolation.

It was originally kept intentionally minimal: built only as far as
scenario #12 (this PR's own first addition) actually needed.
`Harness.NewRegistry` still wires commander/provider/sourceControl as nil,
which remains sufficient for scenario #12 and #3 (neither ever exercises
spawn/dispatch/push). A follow-up PR (§9.3 #5's plain-SPAWN race variant,
#7) has since extended it exactly as predicted here: #5's variant turned
out to belong in `internal/app/sessionactor/dispatch_integration_test.go`
instead (see that scenario's own entry below for why), so it needed no
harness change at all; #7 needed a real, live commander, added as a
genuinely new, separate constructor —
`Harness.NewRegistryWithCommander` — rather than changing `NewRegistry`'s
own existing signature, so scenario #12's (and #3's) own simplest case,
and every existing caller of `NewRegistry`, stayed completely unaffected.
`Harness.Events` (a plain `*postgres.EventStore`, mirroring Sessions/Turns/
Sandboxes/Timers above) was added alongside it, for #7's own events-table
dedupe assertion.

## The 12 scenarios

### #1 — kill the CP pod mid-turn

> Kill the CP pod mid-turn → actor rehydrates, turn resumes or
> fails-with-reason; no stuck `processing`.

**Status: covered, with a scoped-and-documented half.** The "or" in the
scenario's own wording is a real disjunction, and only one branch is
built: turn resume onto a still-live sandbox mid-turn is real machinery
only as of scenario #2 below, not a rehydration-after-pod-kill path — no
turn-resume-across-a-pod-kill mechanism exists anywhere in this codebase
(confirmed: `domain/turn`'s own transition table has no edge out of
`Processing` except `Completed`/`Failed`/`Cancelled`). This is an
existing, already-documented scoping decision from when the test was
written (Step 21), not something reopened here.

One branch of it now resumes. When the pod is killed between a turn's
dispatch commit and the write of its prompt, the turn is `Processing` on
a sandbox that never received the prompt; since Step 226 (scenario #22
below), a sandbox whose agent advertises prompt receipts is sent the
same prompt again on its same-gen reconnect to the next pod, and the
turn completes. An agent without that capability is sent nothing, and
its turn still fails with a reason, at `turn_deadline`.

- `TestResilience_KillPodMidTurn_TurnFailsWithReason_NoStuckProcessing`
  — `internal/app/sessionactor/resilience_killpod_integration_test.go`
- `TestResilience_KillPodAfterDispatchCommit_CapableAgent_PromptResentOnSameGenReconnect`
  and `TestResilience_KillPodAfterDispatchCommit_IncapableAgent_NotResent_EndsAtTurnDeadline`
  — `internal/app/sessionactor/promptreceipt_killpod_integration_test.go`

### #2 — kill the sandbox mid-turn

> Kill the sandbox mid-turn → suspect → grace → respawn+resume with same
> conversation id.

**Status: covered**, end to end, literally as described: dispatch onto a
Ready sandbox at gen 1, force it through `Suspect`, let `terminal_grace`
elapse without recovery (`Suspect` → `Failed` → immediate respawn at gen
2), the new sandbox connects and boots, and the SAME still-`Processing`
turn is re-dispatched to gen 2 carrying the session's own recorded
`opencode_conversation_id`.

- `TestResilience_KillSandboxMidTurn_SuspectGraceRespawnReenqueueSameConversation`
  — `internal/app/sessionactor/resilience_turnrecovery_integration_test.go`

### #3 — slow boot

> Slow boot (inject 5-min delay in deps install) → boot_progress keeps
> session alive; no false kill.

**Status: covered.** A real, sustained-survival end-to-end test now exists,
built on this package's own harness with a short `platform.Timeouts`
override (only `FirstConnectBudget`/`SteadyHeartbeatBudget`/
`InactivityMinCheckInterval` matter for this code path): a sandbox seeded
in `Booting`, driven through 4 real rounds of (sleep ~half the steady
budget → real `boot_progress` `SandboxEvent` → real
`TimerFired{Name: TimerConnectingDeadline}`), each round re-confirming from
Postgres that the sandbox is still `Booting` and `connecting_deadline` is
still armed with a genuinely NEW `fires_at` — and asserting the test's own
cumulative elapsed wall-clock time genuinely exceeds `FirstConnectBudget`,
proving this is really "kept alive by repeated pings", not "finished
within the first window regardless". Finally drives a real heartbeat to
`Ready` and confirms the clean hand-off (`connecting_deadline` deletes
itself, `liveness_check` takes over) — the SAME transition
`TestConnectingDeadlineHandoff_ToLivenessCheck` already proves in
isolation, now shown surviving many prior pings first, not merely
reachable in a vacuum.

- `TestResilienceScenario3_SlowBoot_SurvivesRepeatedBootProgressPings_NeverFalselyKilled`
  — `test/resilience/scenario3_slow_boot_test.go`
- `TestEvaluateConnectingTimeout` —
  `internal/domain/sandbox/liveness_test.go` (the pure timeout decision
  function this scenario's own real behavior is built on, still proven in
  isolation too)
- `TestConnectingDeadlineHandoff_ToLivenessCheck` —
  `internal/app/sessionactor/timerfired_integration_test.go` (the
  connecting-deadline → liveness-check hand-off this scenario's own final
  step re-exercises against a session that has already survived several
  slow-boot rounds)

### #4 — late `execution_complete`

> `execution_complete` arrives AFTER terminalization → state reconciled,
> automation counters corrected.

**Status: covered, with an accepted, self-documented gap.** State
reconciliation itself (a late `execution_complete` arriving after the
turn/session already terminalized some other way) is real and tested.
Automation-counter correction is not — `handleSandboxEvent`'s own doc
comment (`internal/app/sessionactor/sandboxevent.go`, not the test file
below) says so directly ("automations are not a built feature anywhere
in this codebase yet ... there is no automation_runs table, no
automation domain package, nothing to correct"), and that remains
accurate: it is genuinely Phase 3+ work, not something this PR's audit
found freshly.

- `TestHandleSandboxEvent_LateExecutionComplete_RecoversSandboxTurnAndSession`
  — `internal/app/sessionactor/suspectrecovery_integration_test.go`

### #5 — two concurrent spawns

> Two concurrent spawns (double-click / retry race) → single winner by
> gen fencing; loser sandbox reaped by GC.

**Status: covered**, for both the RESUME path and the plain-SPAWN path. A
genuine two-actor race is proven for RESUME (two actors, same session,
concurrently attempting to resume the same sandbox — only one call to the
provider's `ResumeSandbox` actually happens) and, as of this same PR, an
identical genuine two-actor race for a plain SPAWN (a brand-new session
with no sandbox row at all — only one call to the provider's
`CreateSandbox` actually happens, actor B's own `EvaluateSpawnDecision`
correctly reading the row as already-`Spawning` at gen 1 and no-opping
rather than double-spawning) — plus the epoch-fencing primitive that makes
either race safe and the reconciler's own orphan-reaping half. Both
concurrency tests live together in `internal/app/sessionactor/
dispatch_integration_test.go`, not in this package: a genuine two-actor
race needs exactly the white-box helpers already built and proven there
(`newTestPoolPair`, `killAdvisoryLockHolder`, `fakeSpawnProvider`, ...),
which this package's own separate `resilience_test` package cannot import
(unexported) — duplicating that whole harness here for one more variant
of an already-covered scenario would be needless, not "genuinely
multi-package orchestration" the way #7 below actually is.

- `TestResilience_ConcurrentResumeAcrossActors_ResumeSandboxCalledAtMostOnce`
  — `internal/app/sessionactor/dispatch_integration_test.go`
- `TestResilience_ConcurrentPlainSpawnAcrossActors_CreateSandboxCalledAtMostOnce`
  — `internal/app/sessionactor/dispatch_integration_test.go` (this PR's own
  addition: the plain-SPAWN variant, mirroring the RESUME test above
  step-for-step)
- `TestExecuteSpawn_StaleEpochOnRecord_PropagatesErrStaleEpoch` —
  `internal/app/sessionactor/dispatch_integration_test.go` (the fencing
  primitive both spawn and resume rely on)
- `TestReconcileOnce_ReapsOrphansLeavesLiveRowAlone` —
  `internal/app/reconciler/reconciler_integration_test.go` (the loser's
  sandbox actually gets reaped)

### #6 — stale sandbox from an old gen reconnects

> Stale sandbox from old gen reconnects → rejected 403, logged, session
> unaffected.

**Status: covered**, across the two places a stale-gen reconnect is
actually rejected: the sandbox-side WS upgrade itself, and the
scm-credentials bearer-token endpoint a sandbox agent separately calls.

- `TestSandboxHandler_GenMismatch` —
  `internal/adapters/inbound/wshub/sandbox_test.go`
- `TestHandleSandboxEvent_FullRoundTrip` (its stale-gen sub-case) —
  `internal/app/sessionactor/sandboxevent_integration_test.go`
- `TestScmCredentials_GenMismatch_Rejected` —
  `internal/adapters/inbound/httpapi/scmcredentials_integration_test.go`

### #7 — WS drop during event stream

> WS drop during event stream → ack protocol redelivers the 6 critical
> events exactly once.

**Status: covered**, end to end, for all 6 critical types
(`execution_complete`, `error`, `snapshot_ready`, `push_complete`,
`push_error`, `sub_task_finish` — `contracts/sandbox-ws/v1/events.schema.json`'s
own description, confirmed exhaustive against `internal/sandboxagent/
wsbridge/doc.go` too). A table-driven test builds this package's own
harness with a REAL commander (`wshub.NewSandboxRegistry`) and a REAL
`wshub.NewSandboxHandler` behind an `httptest.Server`, then dials a REAL,
unmodified `internal/sandboxagent/wsbridge.Bridge` against it through a
small message-relaying proxy (`wsProxy`, this file's own addition) that
can sever the connection at an exact, deterministic point — adapting
`TestSendCritical_ResendUntilAckedThenNeverAgain`'s own scripted-fake-server
mechanism to a real backend instead of a fake one. For each of the 6
types: the critical event is sent via a real `Bridge.SendCritical` call,
the proxy forces the connection closed BEFORE any ack can ever come back,
the bridge genuinely resends the identical event after reconnecting, the
`events` table shows exactly ONE row for that `(session_id, message_id)`
pair despite the resend (the same upsert-dedupe primitive
`TestEventStore_Create_DedupesOnSessionIDAndMessageID` already proves at
the store level, now exercised through the full inbound WS pipeline), and
— for the two types with a real, confirmable idempotent side effect —
`execution_complete` completes a Processing turn exactly once and
`snapshot_ready` sets the sandbox's own `snapshot_id` exactly once
(correlated via its own `PendingSnapshotMessageID` guard). The other 4
types have no bespoke per-type DB-mutation case in `sandboxevent.go`
today, so the events-table dedupe assertion alone is this codebase's own
correct and sufficient proof of "redelivered exactly once" for those — no
fake per-type side effect was invented just to have something more to
assert.

- `TestResilienceScenario7_WSDropAckRedelivery_CriticalEventsRedeliveredExactlyOnce`
  — `test/resilience/scenario7_ack_redelivery_test.go` (table-driven over
  all 6 critical types, one subtest each)
- `TestSendCritical_ResendUntilAckedThenNeverAgain` —
  `internal/sandboxagent/wsbridge/bridge_test.go` (sender-side
  resend-until-acked against a scripted fake WS server, for one critical
  type — the mechanism this PR's own `wsProxy` adapts to a real backend)
- `TestEventStore_Create_DedupesOnSessionIDAndMessageID` —
  `internal/adapters/outbound/postgres/event_artifact_wstoken_integration_test.go`
  (the store-level dedupe primitive the scenario test above now proves
  through the full inbound WS pipeline too)

### #8 — provider API down during spawn

> Provider API down during spawn → typed transient error, retry with
> backoff, circuit breaker only on permanent.

**Status: covered, with an accepted, user-confirmed gap.** The
transient/permanent classification and the circuit breaker's own
permanent-only-increments behavior are genuinely tested against a real
fake provider. "Retry with backoff" specifically is NOT a real, distinct
mechanism anywhere in this codebase: spawn retries use a fixed
`SpawnStuckTimeout`-based force-respawn (`platform/timeouts.go`), not
exponential backoff — unlike `domain/imagebuild.EvaluateBackoff`
(`internal/domain/imagebuild/backoff.go`), which is a real backoff
mechanism, but for a completely different feature (image builds, not
spawn retries).

The user was asked and explicitly chose: document this as an accepted,
deliberate gap — no new backoff mechanism for spawn retries is being
built to satisfy this scenario's literal wording. This is a conscious
product decision, not an oversight this PR is hiding.

- `TestHandleEnsureDispatched_PermanentProviderError_IncrementsCircuitBreaker`
  — `internal/app/sessionactor/dispatch_integration_test.go`
- `TestHandleEnsureDispatched_TransientProviderError_DoesNotIncrementCircuitBreaker`
  — `internal/app/sessionactor/dispatch_integration_test.go`

### #9 — Outbox: Slack API 500s

> Outbox: Slack API 500s for 10 min → notification eventually delivered,
> no loss.

**Status: covered.** Step 35 ("outbox delivery") built both dependencies
this scenario needed (the outbox delivery worker, `internal/app/
outboxworker`, and the Slack Notifier adapter, `internal/adapters/
outbound/slackapi`) and this scenario alongside them.
`scenario9_outbox_retry_test.go` drives a REAL `outboxworker.Builder`
against a REAL `slackapi.Client`, pointed at a fake Slack-shaped
`httptest.Server` scripted to return 500 for several requests before
recovering — the "10 min" outage is compressed via short, test-scale
`platform.Timeouts` overrides (mirroring scenario #3's own identical
convention), never by weakening `domain/outbox.MaxAttempts` itself.
Asserts, via direct Postgres inspection after each tick: the row is never
dead-lettered while genuinely still within its retry budget, is
eventually delivered once the fake server recovers, and is never
delivered twice.

- `TestResilienceScenario9_Outbox_SlackAPI500sThenRecovers_EventuallyDeliveredNoLoss`
  — `test/resilience/scenario9_outbox_retry_test.go`

### #10 — concurrent @mentions on one PR

> Concurrent @mentions on one PR → exactly one review session (atomic
> claim).

**Status: deferred to a later phase** — but for a narrower reason than this
doc previously stated. Step 32 ("GitHub ingress") is no longer a blocking
dependency: it is long since merged and real (`internal/adapters/inbound/github`
is a fully-implemented package — webhook signature verification, mention
detection, and its own per-PR atomic-claim session coalescing via
`coalesce.go`'s `SessionCoalescer.CreateOrJoin` — not the stub this doc used
to quote). What is still missing is the *review session* this scenario
actually asks about: a session the domain recognizes as a code-review
session (risk-map verdict, severities, re-trigger via label/button), as
opposed to any other bot-spawned session Step 32's own generic coalescing
already produces. That domain concept, and the atomic-claim reuse built on
top of it, is **Step 45, "domain/review", and Step 46, "review sessions"**
(both Phase 5 — renumbered from the formerly-Phase-4 Steps 40/41; see
`docs/IMPLEMENTATION_PLAN.md`'s Phase 4 intro and its lines 106-109), and
both remain genuinely unbuilt: `internal/domain/review/doc.go` is still
exactly the empty stub it always was: "Package review will hold
code-review domain logic: risk-map verdicts, sentinels, and the verdict
floor — implemented in PR-40."

Nothing here is built or faked to simulate coverage; there is genuinely
nothing yet to test.

### #11 — dirty working tree at relaunch

> Dirty working tree at relaunch → stash → checkout session branch → pop;
> zero lost user edits.

**Status: covered**, and the strongest, most literal match of all 12
scenarios against its own §9.3 wording — a real dirty tree, a real
relaunch onto a different branch, a real stash → checkout → pop sequence,
asserted against real git state with zero lost edits.

- `TestResilienceScenario11_DirtyWorkingTree_RelaunchWithDifferentBranch_ZeroLostEdits`
  — `internal/sandboxagent/gitclone/sync_test.go`

### #12 — deploy rollout (rolling restart)

> Deploy rollout (rolling restart) → zero sessions marked failed.

**Status: covered — this PR's own new scenario**, the harness's first
proof-of-concept. A real session with a Ready sandbox and a turn
genuinely `Processing` (dispatched moments ago, every timer armed
comfortably in the future — none of the machinery scenario #1 relies on
is anywhere near overdue), owned by a real actor via the harness's
`Registry.GetOrSpawn`. `Registry.Shutdown()` — the REAL graceful path
`cmd/control-plane/main.go` itself uses on `SIGTERM`, not
`pg_terminate_backend`-style connection-severing the way scenario #1's
test simulates a hard pod kill — is called while the turn is still
genuinely in flight. The turn and session are read back from Postgres
afterward and confirmed NOT force-failed by the shutdown itself; a second
`Registry` (a fresh "pod" against the same database) is then confirmed
able to take ownership of the same session again, proving it is truly
resumable, not merely "not yet marked failed".

- `TestResilienceScenario12_GracefulRollingRestart_ZeroSessionsMarkedFailed`
  — `test/resilience/scenario12_rolling_restart_test.go`

## The 4 new warm-boot scenarios (Step 42, §19.2/§19.4/§19.5/§19.7)

Step 42 ("warm boot: refresh pump + hook policy") adds four new,
genuinely new scenario NUMBERS (13-16, none of the original 12 slots were
reserved for warm-boot work) to this same index — each a real,
harness-driven test proving the property, not a unit test relabeled. Two
of the four (stale-image boot, non-idempotent-setup boot) are proven
through `internal/sandboxagent/boot.RunBoot` directly rather than a file
physically inside this directory — the same precedent scenario #11 (Dirty
working tree at relaunch, above) already established: the actual
orchestration logic under test (hook policy, `workspaceMoved`, output-tail
capture, telemetry, all wired together) lives in that package, and
`cmd/sandbox-agent`'s own thin `runBootSequence` wiring around it is
already covered by that package's own pre-existing
`bootsequence_cleanbuild_integration_test.go` precedent — a second,
duplicate proof through `main.go`'s own `os.Executable()`-sensitive
subprocess machinery would add nothing.

### #13 — fetch-fail boot

> A full boot sequence with a failing fetch (Step 40's own degrade policy,
> exercised at the resilience-suite level, not just gitclone's own unit
> tests).

**Status: covered.** A REAL, full boot sequence — `runBootSequence` ->
`gitclone.SyncAll` -> `boot.RunBoot` -> hooks, exactly as `cmd/sandbox-
agent`'s own `run()` drives it — against a `repo_image` workspace baked
from a real git-http-backend test server, then pointed at a genuinely
unreachable origin (a certainly-closed local port, never a hang). Two
sub-cases, both of §19.3's own degrade policy: an invented session branch
(`repos[].branch == nil`) degrades and the whole boot still succeeds,
proceeding on stale (baked) image state; an EXPLICIT branch that is
neither local nor fetchable fails the boot fatally for its primary repo,
proving the non-negotiable "never silently fork a same-named branch at a
stale base" rule holds at the full-boot level, not just in gitclone's own
isolated unit tests (`internal/sandboxagent/gitclone/sync_test.go`'s own
`TestSyncAll_FetchFails_*` table, still covering the same rule directly
against `SyncAll` alone).

- `TestResilienceScenario_FetchFailBoot_InventedBranch_DegradesAndBootSucceeds`
  — `cmd/sandbox-agent/resilience_fetchfail_boot_integration_test.go`
- `TestResilienceScenario_FetchFailBoot_ExplicitBranchNeitherLocalNorFetchable_FatalBoot`
  — `cmd/sandbox-agent/resilience_fetchfail_boot_integration_test.go`

### #14 — stale-image boot

> A `repo_image` boot whose manifest SHA differs from the checked-out
> tree — `workspaceMoved` fires, `setup.sh` reruns non-fatally.

**Status: covered.** A real git repo (one real commit) plus a real
`boot.ImageManifest` shaped exactly like the baked `/narvi/
image-manifest.json` (§19.1), with a `built_repo_shas` entry deliberately
different from the repo's own real, current `HEAD` SHA — `workspaceMoved`
fires (`boot.ComputeWorkspaceMoved`), and a real `boot.RunBoot` call (the
same real hook-policy/output-capture/telemetry machinery `cmd/sandbox-
agent` wires in production) reruns `setup.sh`, non-fatally, confirmed by a
real marker file the rerun writes. A companion test proves the unmoved
case (manifest SHA matches exactly) stays a pure no-op — zero regression
for a session that lands on a genuinely unmoved image.

- `TestResilienceScenario_StaleImageBoot_WorkspaceMovedFiresSetupReruns`
  — `internal/sandboxagent/boot/resilience_repoimage_test.go`
- `TestResilienceScenario_StaleImageBoot_WorkspaceUnmoved_SetupSkipped`
  — `internal/sandboxagent/boot/resilience_repoimage_test.go`

### #15 — refresh-in-flight spawn

> A NEW spawn targeting a fingerprint whose `image_ref` is mid-refresh —
> confirm it still gets the OLD ready ref, never blocked, per §19.2's own
> "never degrades availability" guarantee.

**Status: covered.** A real `internal/app/imagebuild.Builder.RefreshOnce`
call is held genuinely in flight (`BuildImage` blocked, not yet returned)
against a real Postgres `image_builds` row, while a SEPARATE, brand-new
session spawn — through the real dispatch path
(`resolveAndSetImage`) — targets the identical fingerprint concurrently.
The new spawn's own `CreateSpec.Image` is confirmed to be the OLD,
still-ready `image_ref`, never the base image and never blocked waiting on
the refresh. Releasing the block and letting the refresh complete then
confirms the NEW ref is what a later spawn would see — proving the
earlier read genuinely observed an in-flight, not-yet-committed refresh.
A companion, store-level-only test in `internal/app/imagebuild` proves the
identical property in isolation, one layer down.

- `TestResilienceScenario_RefreshInFlightSpawn_StillGetsOldReadyImage`
  — `internal/app/sessionactor/refresh_inflight_integration_test.go`
- `TestRefreshOnce_OldRefStaysServableDuringRefresh` —
  `internal/app/imagebuild/builder_integration_test.go` (the identical
  property, proven at the store level in isolation)

### #16 — non-idempotent-setup boot

> A setup.sh that fails on rerun — confirm the non-fatal severity holds:
> boot still succeeds, failure is visible in the captured output tail
> from §19.5.

**Status: covered.** A real `setup.sh` that fails outright (simulating a
dependency install that is not safely re-runnable against an
already-warm workspace) is rerun under a real `workspaceMoved: true`
condition via `boot.RunBoot` — the boot still succeeds overall (§19.4's
own non-fatal-severity guarantee: a moved workspace proves nothing about
dependencies, so it can never justify failing the boot), and the
failure's own diagnostic stderr output is confirmed genuinely present in
the captured, bounded, ANSI-stripped hook-output tail (§19.5(a)) a real
`slog.Handler` observes — proving this failure mode is no longer
"undiagnosable by construction" the way it was before this Step.

- `TestResilienceScenario_NonIdempotentSetupBoot_NonFatalFailure_VisibleInOutputTail`
  — `internal/sandboxagent/boot/resilience_repoimage_test.go`

## Scenario #17 (Step 74, §27.5/§27.6/§27.8)

Step 74 ("sandbox substrate: docker, egress policy, toolchain") adds one
new scenario number (17, the next free slot after Step 42's 13-16) —
named directly by §27.8's own closing bullet: "Snapshotting a running
dockerd... is untested territory; Step 74 must add a §9.3-class scenario
for restore-with-docker before claiming it works." Proven, like scenario
#15 (refresh-in-flight spawn) before it, through
`internal/app/sessionactor`'s own real dispatch path rather than a file
physically inside this directory — the actual decision under test
(dispatch.go's `tryPlanSpawn`) lives in that package, and this scenario
needs the SAME `fakeSpawnProvider`/`newDispatchTestRegistry` harness that
package's own `dispatch_integration_test.go` already establishes.

### #17 — restore-with-docker

> A Docker-required session's sandbox needs recovery — confirm it is
> NEVER restored from a snapshot (§27.8's own genuinely unresolved
> cross-runtime snapshot-parity point), even when a stale snapshot_id
> already exists on its own row; it always takes a fresh spawn instead.

**Status: covered — with an accepted, DELIBERATE gap, not a claim of
snapshot-restore support.** This Step does not implement, and does not
claim, cross-runtime (Modal gVisor vs VM-runtime) snapshot-restore
support for Docker-enabled sandboxes at all — there is no real Modal
deployment this codebase can verify that against (every Modal wire shape
in this repo is this codebase's own invention, tested against a fake
`httptest.Server`, `internal/adapters/outbound/modal/doc.go`). Instead,
two independent, real, harness-driven guards are proven together: (1)
`sandboxevent.go`'s own `triggerSnapshotBestEffort` never even attempts
to CREATE a snapshot for a Docker-required session in the first place
(so `snapshot_id` structurally can never become non-empty for one via
this codebase's own normal flow), and (2) even in the edge case where a
sandbox row's own `snapshot_id` is non-empty anyway (planted directly in
the test, standing in for any other way a stale value could theoretically
reach that column), `dispatch.go`'s `tryPlanSpawn` downgrades what
`EvaluateSpawnDecision` would otherwise resolve as `SpawnActionRestore`
into a plain fresh `SpawnActionSpawn` — proven through a REAL
`EnsureDispatched` cycle against a real Postgres sandbox row in
`Stopped` status: `RestoreFromSnapshot` is never called; `CreateSandbox`
is, carrying `Docker: true`. A positive-control pair (Docker-false, the
identical Stopped+snapshot_id fixture) proves the downgrade is a real,
narrow discriminator — an ordinary session still restores from a real
snapshot exactly as it always has.

The `docker`/`kubectl`/§27.4 exec-credential-plugin toolchain content
itself (§27.7) is a base-image build-artifact concern, external to this
codebase's own Go test surface by the same "external, opaque-to-this-
repo build service" boundary §19.1 already draws around image builds
generally — nothing to replay here.

- `TestResilienceScenario17_RestoreWithDocker_NeverRestoresStaleSnapshot`
  — `internal/app/sessionactor/snapshot_docker_integration_test.go`
- `TestResilienceScenario17_RestoreWithDocker_DockerFalseStillRestores`
  (positive control) — `internal/app/sessionactor/snapshot_docker_integration_test.go`
- `TestTriggerSnapshotBestEffort_DockerRequiredSession_NeverSnapshots` /
  `TestTriggerSnapshotBestEffort_DockerFalseSession_StillSnapshots`
  (the companion snapshot-creation-side guard and its own positive
  control) — same file

## Scenario #20, the freeze half (Step 149, §40.2)

### #20 — the freeze flipped with candidates pending

> Session spend reaches its cap mid-run → [the cap half, Step 148's]; and
> the freeze flipped with auto-merge candidates pending → no merge, no
> auto-fix spawn, no re-review enqueue and no automation invocation occurs
> while frozen, every candidate is still a candidate after unfreeze, a
> human command still works, and no running turn is severed (§40.1,
> §40.2).

**Status: the freeze half is covered; the cap half waits on Step 148.** One
database and the components the control plane runs, driven tick by tick: a
registry whose sandboxes are real `wsbridge.Bridge` agents on the real
sandbox handler (each answers its snapshots, counts any stop, and completes
a prompt at once or when the test says), the auto-merge worker, the outbox
builder with the real sentinel auto-fix notifier, the automation engine, the
held workflow advance releaser, and the audited admin routes that freeze and
unfreeze. One code host stands in for GitHub: an armed pull request, open
until merged, and the fix branch.

- `TestResilience_Scenario20_Freeze_NothingAutomaticStarts`: an armed
  auto-merge candidate, a sentinel auto-fix delivery, a debounced
  re-review, a daily cron schedule whose one occurrence falls in the
  freeze, a workflow step's turn, and a
  person's turn processing on its sandbox are all pending when an
  administrator freezes autonomy through `POST /api/autonomy/freeze`.
  While frozen: no pull request read and no merge; the fix delivery held
  with no attempt counted, no branch and no session; the re-review's
  debounce re-armed with no turn; no cron invocation; an invocation
  recorded meanwhile, as an event's is, left unclaimed with no run; the
  workflow step's turn completes and its advance is held, which the
  decision inbox lists beside the freeze; a person's prompt on another
  session is dispatched and completes; and the processing turn is left
  processing. After `POST /api/autonomy/unfreeze`, each site runs its
  candidate once and a second tick runs nothing more: two auto-merge ticks,
  one merge; two outbox ticks, one fix branch and session on one counted
  attempt; the debounce pumped until it reviews, then due again and pumped
  twice, one review of the pushed head; the cron trigger ticked, then
  ticked again as a tick in the next minute reads it (the recorded fire
  aged by one minute), one invocation of the held occurrence; two fan-out
  ticks, one run per invocation; and two release ticks, the held advance
  released exactly once, its second step run and completed. The processing turn then completes on its own: no stop was
  ever sent to its sandbox and its gen lives. No turn failed or was
  cancelled, and each change wrote its one audit row.
  — in `scenario20_freeze_test.go`

Each site's own freeze behaviour is pinned beside it (Step 149's three
parts), and the admin action's in `internal/adapters/inbound/httpapi`'s
`autonomyfreeze_integration_test.go`.

## Scenario #22 (Step 226, §3.3)

### #22 — a prompt lost between dispatch and the sandbox

> A prompt lost between dispatch and the sandbox → the same-gen reconnect
> re-sends it once to a capable agent, which runs it once; never re-sent
> to an agent without the capability, whose turn ends at `turn_deadline`.

**Status: covered.** The control plane commits a turn `Processing`, then
writes its prompt in one frame nothing acknowledges. Here the frame is
lost with its socket after the write returned: scenario #7's `wsProxy`,
extended with `dropBackendType`/`dropClientType`, sits between a real
`wsbridge.Bridge` and the real `wshub` sandbox handler, drops the first
`prompt` it relays backend→client and severs both sides. The sandbox
reconnects on the same gen. A capable agent (a real `Bridge`, its prompt
journal open) is sent the prompt again with the same `messageId`, runs it
once, and its `execution_complete` completes the turn. When the receipt
is what is lost, the copy it is re-sent is answered `duplicate: true` and
not run again. An agent built before receipts, its frames written by hand
as `TestBootReady_PreFixAgentWire` writes them, advertises nothing, is
sent nothing more, and its turn ends at `turn_deadline` as a timeout.
The same exit with the dispatching replica killed after its commit is
scenario #1's second pair of tests, above.

- `TestResilience_Scenario22_LostPromptFrame_CapableAgent_DeliveredOnceAndCompletes`
- `TestResilience_Scenario22_LostReceiptFrame_CapableAgent_RunsOnce`
- `TestResilience_Scenario22_LostPromptFrame_PreChangeAgent_NeverResent_EndsAtTurnDeadline`
- `TestResilience_Scenario22_PromptFrameOver32KiB_CapableAgent_DeliveredOnceAndCompletes`:
  a prompt frame over the WebSocket library's default 32 KiB read limit,
  which closed the agent's connection and was lost before the agent read up
  to `platform.MaxPromptFrameBytes`, is read whole and run once. The agent's
  every `ready` states that limit (`capabilities.maxFrameBytes`), which the
  control plane holds its gen's prompts to (§3.3).
- `TestResilience_Scenario22_PromptLostOnEveryDelivery_ResendCapStopsTheLoop`:
  a prompt lost on every delivery, each loss a reconnect, is re-sent
  `PromptResendMaxPerTurn` times and then no more, and the reconnects stop.
  — all five in `scenario22_lost_prompt_test.go`

## Scenario #23 (Step 228, §3.3, §6.1)

### #23 — frames over 32 KiB on the sandbox socket

> An agent event over 32 KiB is read on the connection it came on by a
> control plane that states a larger limit, and written cut to one that
> states none, never a reconnect loop; a prompt over the bound its gen's
> agent states is refused at dispatch with both sizes named.

**Status: covered.** Before Step 228 the socket was bounded one way only.
The control plane read it at the WebSocket library's 32 KiB default, and
nothing bounded what an agent wrote: an event over 32 KiB closed the
connection, and the agent, which replays its buffer on every reconnect,
closed each one the same way, about 320 times a second, with nothing behind
the frame ever arriving. In the other direction an agent built before Step
226 read prompts at that default and lost a larger one in silence.

Each side now states the largest frame it reads, and neither writes past
what the other states. The control plane reads agent events up to
`platform.MaxEventFrameBytes` (1 MiB) and states it in the handshake's
`X-Max-Frame-Bytes` header. The agent holds every write to what its
connection states, or to 32 KiB when the header is absent. A `token`,
`tool_call` or `tool_result` over that bound is written cut, its `cut`
property set. Any other frame over it is skipped and warned about once,
and the replay goes on. Every `ready` states the agent's own read limit
(`capabilities.maxFrameBytes`), and the control plane holds each gen's
prompts to the latest `ready`: the stated limit, else 32 MiB for an agent
that advertises `promptReceipt`, else the 32 KiB default.

Every test below uses scenario #22's rig: the real `wshub` sandbox handler
and session actor on Postgres, behind scenario #7's relay. Its two knobs
stand in for both kinds of control plane:

- `forwardMaxFrameHeader` forwards the backend's header, which makes the
  relay a control plane built with it.
- `clientReadLimit` set to 32768, with the header dropped as it is by
  default, makes the relay a control plane built before it.

`sever()` forces a reconnect to whichever comes next. The agents are a
real `wsbridge.Bridge`, or one written by hand as scenario #22's are. The
`tool_result` cases use the pinned runtime's id shape, the enclosing
message's id behind that message's `step_start`, so a `tool_result` is
stored under the key its `callId` derives (Step 229). They assert what the
wire carries and the one row it leaves.

- `TestResilience_Scenario23_AgentEventOver32KiB_ReadOnOneConnection`: with
  the header forwarded, a 40 KiB text part is stored whole and a 40 KiB
  `tool_result` reaches the handler whole and is stored once, whole. The
  `execution_complete` behind them completes the turn, after one `ready`.
- `TestResilience_Scenario23_PushErrorOver32KiB_ReportedAndNextTurnCompletes`:
  a completed turn's push fails with 40 KiB of git stderr. Its `push_error`,
  capped at 4096 bytes as the agent caps it (`wsbridge.CapCriticalText`),
  is stored marked `...[truncated]` and acked, never replayed after, and
  the delivery stamp ends. The next turn's `execution_complete` behind it
  completes that turn.
- `TestResilience_Scenario23_PreChangeAgent_EventOver32KiB_Stored`: an agent
  built before the header, which bounds nothing it writes, has its 40 KiB
  text part read on the connection it came on and stored whole.
- `TestResilience_Scenario23_PreChangeControlPlane_FramesCutToFit_NoReconnectLoop`:
  with no header and a 32 KiB read, a plan-mode turn's 40 KiB text part
  arrives cut, its `cut` set. The plan reads as cut: no structured steps,
  though its plan-steps block survived the cut, and `ErrPlanCut` refuses
  Approve. A 40 KiB `tool_result` arrives cut to 32 KiB and is stored
  once, `cut` kept; the turn completes, and `ready_seq` stays at 1. A
  reconnect to a control plane that states its limit then replays both
  whole and adds no row: the turn is over, and the stored `tool_result`
  stays the cut one, first wins.
- `TestResilience_Scenario23_WholeThenCutOnRollback_FinalTextReadsWhole`:
  a part is stored whole, then replayed cut after a reconnect to a control
  plane that states nothing. The real handler adds no row for the cut,
  which yields to the whole text, and a replica built before cuts stores
  it anyway. The plan and the web timeline still read the part whole, and
  the plan is approved. The stored rows are pinned against
  `web/src/session/__tests__/fixtures/tokenWholeThenCutOnRollback.json`,
  which the timeline test reads.
- `TestResilience_Scenario23_EarlierFrameWholeThenFinalCut_CutReported`: a
  35 KiB frame is stored whole, and its replayed cut adds no row. The final
  45 KiB frame arrives cut. The plan reads that cut, never the earlier
  frame, and the approval is refused.
- `TestResilience_Scenario23_NoHeader_WritesHeldTo32KiB_PromptOver32KiBStillRead`:
  a missing header bounds what the agent writes, never what it reads. A
  40 KiB prompt is read and run once, and nothing over 32 KiB is written.
  — the seven above in `scenario23_event_frames_test.go`
- `TestResilience_Scenario23_Step226Agent_PromptOver32KiB_Delivered`: an
  agent built since Step 226 but before Step 228, whose `ready` advertises
  `promptReceipt` and states no limit, is sent a 40 KiB prompt whole on
  the connection it came on, receipts it, and completes the turn.
- `TestResilience_Scenario23_PromptOver32KiB_PreReceiptAgent_RefusedAtDispatch`:
  an agent built before Step 226, with no capabilities and the 32 KiB
  default, is written nothing; its turn fails at dispatch with a synthetic
  `execution_complete` marked `"delivered": false` and a warning naming
  both sizes, and its connection stays up.
  — both in `scenario23_prompt_frames_test.go`

## Scenario #24 (Step 204, §3.3, §21.1)

### #24 — a review turn reads the commit it recorded

> A review turn of a pull request's review session → its sandbox checks
> out the head the turn recorded before the prompt is sent, including a
> re-review in a warm sandbox whose tree the previous turn modified; a
> control plane that restarts between the checkout's send and its reply
> sends the prompt once.

**Status: covered.** The control plane is real: a registry with a real
`wshub` commander, the real sandbox handler, real Postgres, behind
scenario #7's relay. The sandbox is a real `wsbridge.Bridge` whose
`CheckoutHandler` runs the agent's own `gitclone.CheckoutPullRef` against
a base repository, `acme/widgets`, served by `git-http-backend` over TLS,
which keeps the pull request's head as `refs/pull/7/head`. The rest of the
agent stands in: a prompt records the commit and the status of the
worktree it finds, may leave edits behind as a turn does, and completes; a
snapshot is answered at once.

- `TestResilience_Scenario24_WarmReReview_EachTurnChecksOutItsOwnHead`:
  the first review turn finds S1, its recorded head, in a clean tree and
  leaves a tracked change, a staged file, a deleted file, an untracked file
  and an ignored one behind; the contributor pushes S2, and the re-review,
  on the same gen, finds S2 in a clean tree, the ignored file kept. Each
  turn records the commit it was checked out at, and each checkout's reply
  is stored before its prompt was sent.
- `TestResilience_Scenario24_ControlPlaneRestartBetweenSendAndReply_PromptSentOnce`:
  the replica that sent the checkout shuts down before the reply; the
  sandbox reconnects to another, whose actor finds the request recorded and
  the reconnect counted and asks again, while the reply in flight is
  replayed and stored under its own key. The prompt is sent once, on the
  recorded head.
  — both in `scenario24_review_checkout_test.go`

The sandbox-agent half of the exit is pinned in `cmd/sandbox-agent` and
`internal/sandboxagent/gitclone` (Step 204's first part), and the control
plane's decisions -- a moved head, a lagging ref, an old agent, a gen
not connected yet, a silent or failing sandbox -- in
`internal/app/sessionactor`'s
`reviewcheckout_integration_test.go`.

## Not a scenario: the tool events of one message (Step 229, §6.1)

The runtime adapter gives every event it derives from a part of an
assistant message that message's id, and the message's `step_start` comes
first. Stored under the wire `messageId` alone, every `tool_call`,
`tool_result` and `step_finish` of a real turn deduped onto that
`step_start`'s row, and none was stored or broadcast; the tests that should
have seen it minted a fresh id per event. Each is now stored under a key
derived from its `callId` or `stepId`, only while its turn is live and its
message's `step_start` is stored in that turn.

- `TestResilience_ToolEventsOfOneMessage_EachStoredOnce`: the real OpenCode
  adapter runs against a scripted runtime server, so the events are the ones
  its translate path emits for one message -- a `read` call and a `task`
  call that spawns a sub-task, each with its result, between a `step_start`
  and a `step_finish` -- and reach the real handler and actor through a
  real `wsbridge.Bridge`. Each is stored once under its key and broadcast
  once to a subscribed page, the `sub_task_start` names its call
  (`parentCallId`), a reconnect's replay adds no row and broadcasts none
  again, and `fetch_history` on the real client socket returns the
  message's `step_start`, `tool_call`s, `tool_result`s and `step_finish`.
  The stored rows are pinned against
  `web/src/session/__tests__/fixtures/toolEventsOfOneMessage.json`, which
  the web timeline, cost and header tests read.
  — in `tool_events_test.go`

## Summary

| # | Scenario | Status |
|---|---|---|
| 1 | Kill CP pod mid-turn | Covered (fails-with-reason, documented since Step 21; a prompt lost after the dispatch commit resumes for an agent with prompt receipts since Step 226) |
| 2 | Kill sandbox mid-turn | Covered |
| 3 | Slow boot | Covered |
| 4 | Late `execution_complete` | Covered (automation-counter correction deferred to Phase 3+) |
| 5 | Concurrent spawns | Covered (RESUME and plain-SPAWN both) |
| 6 | Stale-gen reconnect | Covered |
| 7 | WS-drop ack redelivery | Covered (all 6 critical types) |
| 8 | Provider down during spawn | Covered (backoff-retry mechanism an accepted, user-confirmed gap) |
| 9 | Outbox: Slack API 500s | Covered — this Step (35) |
| 10 | Concurrent @mentions | Deferred — needs Step 45/46 (Phase 5, domain/review + review sessions); Step 32 (Phase 3, GitHub ingress) is done and no longer blocking |
| 11 | Dirty working tree at relaunch | Covered |
| 12 | Deploy rollout (rolling restart) | Covered — this PR |
| 13 | Fetch-fail boot | Covered — Step 42 |
| 14 | Stale-image boot (`workspaceMoved` fires) | Covered — Step 42 |
| 15 | Refresh-in-flight spawn | Covered — Step 42 |
| 16 | Non-idempotent-setup boot | Covered — Step 42 |
| 17 | Restore-with-docker | Covered — Step 74 |
| 20 | Spend cap mid-run; the freeze | Freeze half covered, Step 149; cap half waits on Step 148 |
| 22 | A prompt lost between dispatch and the sandbox | Covered — Step 226 |
| 23 | Frames over 32 KiB on the sandbox socket | Covered — Step 228 |
| 24 | A review turn reads the commit it recorded | Covered — Step 204 |

Numbers 18-21 are taken by the scenarios `docs/TECHNICAL_PLAN.md` §9.3 appends for Phases 13, 15
and 18 (rotation and the interrupted turn, fresh-lineage continuity, the spend cap and freeze, the
Kubernetes provider); of those, only 20's freeze half is built (Step 149). 22 is Step 226's, 23 is
Step 228's, and 24 is Step 204's. A new scenario takes 25.
