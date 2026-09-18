// Package automation is the process-wide background automation engine
// ("automations: engine", §3.5) -- a sibling of app/reconciler,
// app/imagebuild, and app/outboxworker, not folded into any of them
// (TECHNICAL_PLAN.md §1's own repo-layout convention: one package per
// major loop/subsystem under internal/app/). See internal/domain/
// automation for the pure decision functions this package's own impure
// loops are built around.
//
// Engine.Run (mirroring app/imagebuild.Builder.Run's own "fan out more
// than one independent ticker loop via a zero-value errgroup.Group" shape
// exactly) fans out THREE independent ticker loops:
//
//  1. runFanOutPump (platform.Timeouts.AutomationEnginePumpInterval),
//     calling PumpOnce -- fanout.go: claims a batch of automation_invocations
//     rows not yet fanned out (ONE transaction: SELECT ... FOR UPDATE SKIP
//     LOCKED, then a per-row UPDATE ... WHERE fanned_out_at IS NULL,
//     committed BEFORE any real session-creation work -- mirrors
//     app/imagebuild.Builder.claimBatch's own claim-batch precedent
//     exactly), then for each claimed invocation, OUTSIDE any transaction,
//     fans out one automation_runs row PER target (§3.5: "one run per
//     target, fan-out ≤10" -- internal/domain/automation.MaxFanOutTargets
//     already bounds this at invocation-creation time, CreateInvocation
//     below). Each target's own run+session pair is created together, on
//     ONE freshly opened transaction, via the SAME httpapi.CreateSessionOnTx
//     core §5.1 established and the GitHub/Slack/Linear ingress adapters already reuse three
//     times -- exactly mirroring internal/adapters/inbound/github's own
//     SessionCoalescer.CreateOrJoin, which calls CreateSessionOnTx inline on
//     its own already-open tx rather than the pool-based CreateSessionCore
//     (fanout.go's own createRunAndSession does the identical thing, for the
//     identical reason: the run row and its session must land in the SAME
//     transaction, so a crash between the two can never leave a run with no
//     session or a session with no corresponding run). One target's own
//     failure (repo validation, a Postgres error) is isolated -- logged,
//     recorded as a RunStatusFailed run, and does NOT abort the rest of that
//     invocation's own fan-out, exactly like app/imagebuild.Builder.attempt's
//     own per-row isolation.
//
//  2. runReconcilePump (platform.Timeouts.AutomationEnginePumpInterval),
//     calling ReconcileOnce -- reconcile.go: lists a bounded batch of
//     still-non-terminal automation_runs rows (a plain, unlocked SELECT --
//     see ListInFlightRuns' own generated doc comment for why no FOR UPDATE
//     SKIP LOCKED is needed here), and for each one whose own linked
//     session's turn history (automation.DeriveRunStatus) reports a NEW
//     status, applies the CAS-guarded write (PromoteToRunning or
//     Terminalize) and, for a terminalizing write this call actually won,
//     cascades to closeout.go's own maybeCloseInvocation.
//
//  3. runSweepPump (platform.Timeouts.AutomationSweepInterval), calling
//     SweepOnce -- sweep.go: §3.5's own two recovery-sweep thresholds
//     ("orphaned starting runs >5 min, running >90 min") -- a run whose own
//     started_at/running_at predates its own threshold
//     (automation.IsOrphaned, injected `now`) is terminalized via
//     RunTriggerOrphanTimeout and cascades through the SAME closeout.go path
//     a genuine turn-outcome terminalization does. Mirrors app/reconciler's
//     own periodic-sweep shape, with one deliberate simplification: unlike
//     app/reconciler.Reconciler's own in-memory, cross-tick "seen
//     unexplained since" debounce map (needed there because a live provider
//     ref's own absence from Postgres is genuinely ambiguous on first
//     sighting), a run's own started_at/running_at is already a durable
//     timestamp -- "has this run been stuck past its own threshold" is
//     answered by a single comparison against an injected `now`, with no
//     cross-tick memory required.
//
// # Cascaded through one shared closeout path
//
// closeout.go's own maybeCloseInvocation/applyFailureStrike are the ONE
// place every terminalizing write (a genuine turn outcome, via
// ReconcileOnce, OR an orphan timeout, via SweepOnce) cascades through --
// never duplicated between the two callers. See internal/domain/
// automation/doc.go's own "Closing an invocation vs. recording its
// failure-strike consequence" section for why closing an invocation
// (automation_invocations.status, guarded by its own current status) and
// recording that closure's failure-strike consequence
// (automation_invocations.failure_counted_at IS NULL, §3.5's own literal
// CAS idiom, now applied atomically together with the automations row lock
// and the strike write itself, all in ONE transaction -- applyFailureStrike
// below) are two separate steps, each its own guarded operation -- not
// collapsed into a single one spanning both.
//
// # Known, deferred: closeInvocation's own Close call is not part of that
// same transaction
//
// closeInvocation's own Close call (closeout.go) still runs as its own
// standalone, pool-auto-committed statement -- it is NOT folded into the
// SAME transaction applyFailureStrike immediately below it now shares
// between MarkFailureCounted and LockForUpdate/ApplyFailureStrike. A crash
// after Close's own status-transition commits (durably 'failed') but
// before applyFailureStrike's own transaction commits still loses that
// invocation's own strike identically: Close's own "AND status = 'pending'"
// guard blocks any retried close-out from ever re-entering once the
// invocation is already terminal, so nothing re-drives applyFailureStrike
// for it. This is a genuine, narrower residual gap left deliberately
// unaddressed by this Step (Close is called from exactly one call site
// today, so folding it into the same transaction is plausible future work,
// but doing so here would have widened this Step's own already-large
// blast radius for a window that is narrow in practice -- Close and
// applyFailureStrike run back-to-back, milliseconds apart, not indefinitely
// apart the way the claimed-but-unfanned gap below can be) -- named
// honestly here, mirroring app/imagebuild/doc.go's own established
// discipline of naming an accepted gap explicitly rather than leaving it
// silently unaddressed.
//
// # Known, deferred: a claimed-but-unfanned invocation has no recovery sweep
//
// claimBatch (fanout.go) commits fanned_out_at for a batch of invocations
// BEFORE the fan-out loop that actually creates their own runs ever runs
// (deliberately -- the claim step's own transaction must stay short-lived,
// never held open across real session-creation work). A crash between that
// commit and PumpOnce's own subsequent `for _, inv := range claimed`
// loop actually reaching a given invocation (or reaching it only
// partway, for one of its own several targets) leaves that invocation
// permanently fanned_out_at-stamped with fewer runs than total_runs
// promises -- ListDueForFanOut's own WHERE clause excludes it forever
// (fanned_out_at is no longer NULL), and no OTHER sweep in this package
// scans automation_invocations at all: the recovery sweeps §3.5 specifies
// (sweep.go's own ListOrphanedStarting/ListOrphanedRunning) are scoped to
// RUNS that already exist, never to an invocation stuck between "claimed"
// and "actually fanned out". This is a legitimate, already-accepted scope
// decision (recovery sweeps per §3.5 are specified for runs only), not an
// oversight -- mirroring app/imagebuild/doc.go's own identical precedent
// for the analogous gap on its own build-claim path ("a process crash
// between claiming a row... and recording its outcome leaves that
// fingerprint permanently stuck in 'building', never retried by this
// package's own mechanism... not built here"). A future Step could add a
// staleness sweep here too (a fanned_out_at-stamped invocation whose own
// run count never reaches total_runs past some bound gets its missing
// targets re-driven, or is force-closed failed) -- not built in this Step.
//
// # Known, residual: paused-automation TOCTOU across a claimed batch
//
// ListDueForFanOut's own "AND a.status = 'active'" join condition
// (queries/automationinvocations.sql) is commit 431e4b3's own "SECOND,
// independent layer" of defense-in-depth against fanning out a pending
// invocation whose automation has since been auto-paused -- every trigger
// evaluator this package now has (EvaluateCronTriggersOnce's own
// ListActiveCronAutomations, DispatchGitHubWebhookEvent's own
// ListActiveGitHubAutomations, DispatchLinearWebhookEvent's own
// ListActiveLinearAutomations, and the generic webhook trigger's own
// automationwebhook.NewHandler) is the FIRST layer: each already filters to
// "status = 'active'" before ever calling CreateInvocation, so a paused
// automation is never a fresh call's own target in the first place. Because
// claimBatch claims an
// entire BATCH of due invocations inside one transaction, an automation can
// still pause mid-batch: some of its own invocations, already claimed
// (fanned_out_at stamped) and already fanned out into real sessions earlier
// in that SAME PumpOnce tick, are unaffected by a pause that lands a moment
// later, in between two invocations of the SAME claimed batch. This residual
// window is consistent with, not a regression against, what 431e4b3 already
// claimed to deliver (a second layer narrowing the race, not eliminating
// every instance of it) -- fanning out a small number of runs for an
// automation that pauses moments later is an accepted outcome, not a
// correctness bug (those runs still complete/fail normally, and their own
// invocation's eventual close-out still applies the normal success/failure
// accounting). Not fixed here: skipping an already-fanned-out invocation
// is not a free early-return -- once fanned_out_at is committed it can
// never be re-listed, so closing it out consistently with an automation
// that is ALREADY paused by the time it terminalizes (e.g. not counting it
// as a failure strike against an automation no longer accepting new work)
// would need its own small design decision, not addressed by this Step.
//
// # CreateInvocation -- the one entry point every trigger evaluator shares
//
// §8.4 ("automations: triggers & extras", §8.4) owns WHAT causes an
// invocation to be created -- GitHub/Linear/webhook/cron trigger condition
// evaluation -- and every one of those four now calls this SAME entry
// point unchanged: the cron trigger pump (triggerpump.go's own
// EvaluateCronTriggersOnce), live GitHub/Linear webhook dispatch
// (githubdispatch.go's own DispatchGitHubWebhookEvent, lineardispatch.go's
// own DispatchLinearWebhookEvent), and the generic webhook trigger
// (internal/adapters/inbound/automationwebhook's own handler.go).
// invocationenqueue.go's CreateInvocation is the minimal, durable "an
// invocation now exists, fan it out" hand-off every one of them shares
// (mirrors internal/app/releasereview.Enqueue's own "one cheap INSERT, the
// real work happens later on a dedicated background loop's own schedule"
// shape). It does NOT itself decide whether an automation should fire; it
// only validates targets (automation.ValidateTargets) and durably records
// that a firing has already been decided.
package automation
