// Package automation holds the automation → invocation → run(s) domain
// model ("automations: engine", §3.5): "automation → invocation →
// run(s) (one run per target, fan-out ≤10). At-most-one failure strike per
// invocation via CAS (UPDATE ... WHERE failure_counted_at IS NULL).
// Auto-pause after 3 consecutive failed invocations. Recovery sweeps:
// orphaned starting runs >5 min, running >90 min."
//
// No I/O, no time.Now(), no randomness (§11) -- every input a decision
// function here needs (counts, states, "now") is supplied by the caller
// (internal/app/automation). This mirrors internal/domain/imagebuild's own
// split exactly (EvaluateBackoff/ImageBuildStreakThreshold there; the
// analogous EvaluateFailureStrike/AutoPauseThreshold here) -- in fact
// domain/imagebuild.ImageBuildStreakThreshold's own doc comment already
// names this package's own "3" explicitly, as the same established
// constant this codebase already uses twice (sandbox.
// CircuitBreakerThreshold, ImageBuildStreakThreshold) before this package
// existed for real.
//
// # Three machines, three transition tables
//
// Automation (automation.go): Active ⇄ Paused -- the auto-pause/resume
// lifecycle a maintainer/admin eventually toggles (mockups.html's own
// Automations view: "auto-paused chip + Resume").
//
// Invocation (invocation.go): Pending -> Succeeded | Failed -- closed
// exactly once, when every one of its fanned-out runs has reached a
// terminal state. Mirrors internal/domain/plan.Status's own shape (one
// non-terminal state, terminal states with no outgoing edge) more closely
// than turn's six-state machine, since an invocation has no interesting
// internal progress of its own -- only "still waiting on its runs" vs.
// "decided".
//
// Run (run.go): Starting -> Running -> Succeeded | Failed -- named to
// match §3.5's own sweep vocabulary exactly ("orphaned starting runs...
// running..."). DeriveRunStatus derives a run's own status from its linked
// session's turn history, the SAME "derive an aggregate status from a
// child summary slice" shape internal/domain/session.DeriveStatus already
// establishes for a session's own turns -- Starting corresponds to no
// turn having reached Processing yet (matches a run whose sandbox may
// still be cold-starting -- exactly the condition the "starting >5 min"
// sweep threshold exists to catch), Running to a turn now Processing.
//
// # Closing an invocation vs. recording its failure-strike consequence
//
// Closing an invocation (Pending -> Succeeded/Failed, via Transition,
// app/automation's own closeInvocation) and recording a FAILED invocation's
// failure-strike consequence against its own automation's consecutive-
// failure streak (applyFailureStrike) are two separate steps, each its own
// guarded operation: mirrors internal/app/sessionactor/progressnotify.go's
// own documented "two, independent, both reused rather than invented"
// double-guard precedent. The invocation's own status transition is guarded
// by its own current status (the Transition table's usual precondition,
// exactly like turn/plan/sandbox); the strike accounting is SEPARATELY
// guarded by automation_invocations.failure_counted_at IS NULL (§3.5's own
// literal CAS idiom, the same UPDATE ... WHERE <nullable-timestamp> IS NULL
// shape already established by TurnStore.MarkProgressNotified/
// ApprovePlanIfAwaitingApproval) -- so a crash between closeInvocation
// committing and a retried close attempt can never double-count the SAME
// invocation's failure against the streak twice, even though the
// invocation's own status is by then already terminal and would otherwise
// short-circuit a naive single-guard re-check.
//
// That failure_counted_at guard now runs INSIDE the SAME transaction as
// AutomationStore's own LockForUpdate/ApplyFailureStrike (app/automation's
// own closeout.go, applyFailureStrike) -- matching the "MUST run inside the
// same transaction" comments this package's own store/query layer has
// always carried (AutomationInvocationStore.WithTx, AutomationStore.WithTx,
// queries/automationinvocations.sql, queries/automations.sql). An earlier
// revision of this package's own writeup described the guard as committing
// STANDALONE, ahead of that transaction, as though that were a third,
// independently-committing step rather than a bug -- it was a bug: any
// failure between that standalone commit and the (separate) strike
// transaction's own commit (LockForUpdate, ApplyFailureStrike, or the
// commit itself) permanently set the one-way CAS guard with NO strike ever
// recorded, and nothing anywhere reads failure_counted_at to retry or
// reconcile it -- that invocation's own failure would silently and
// permanently never count toward auto-pause. Fusing the guard and its
// consequence into one atomic transaction closes that gap: either both the
// guard flip and the strike land together, or neither does, leaving a
// subsequent retry of applyFailureStrike free to win the still-unset guard
// and apply the strike for real.
//
// A crash between closeInvocation's OWN status-transition commit and this
// now-atomic strike transaction's own commit remains a SEPARATE, still-open
// residual gap -- closeInvocation's own Close call is not itself part of
// the same transaction as the strike accounting below it (see
// internal/app/automation/doc.go's own "closeInvocation's own Close call is
// not part of that same transaction" section, mirroring app/imagebuild/
// doc.go's own analogous claim-crash-gap precedent for why this is left as
// an accepted, documented gap rather than folded in here).
// This residual window is narrower than the one just closed above (Close
// and applyFailureStrike run back-to-back, milliseconds apart, versus the
// old standalone-guard gap, which could persist indefinitely with no retry
// path at all), but it is not zero.
//
// # EvaluateFailureStrike computes, the CAS-guarded store records
//
// EvaluateFailureStrike (strike.go) is a pure decision, mirroring
// domain/imagebuild.EvaluateBackoff/domain/sandbox.EvaluateCircuitBreaker's
// own shape exactly: given the CURRENT consecutive-failure count and
// whether the invocation that just closed failed, it returns the new count
// and whether that crosses AutoPauseThreshold (3). It does not itself
// count anything, or decide whether THIS particular failure has already
// been counted -- that is the CAS's job, entirely in app/automation's own
// impure layer, per this package's own "no I/O" boundary (§11).
//
// # §8.4 ("automations: triggers & extras", §8.4): what this package adds
//
// Five new files, each a small, independent, pure addition -- none of them
// touch the three transition tables above, since none of this Step's own
// scope is a NEW state or a NEW edge in any of the three machines already
// documented above:
//
//   - trigger.go: TriggerType (manual/cron/github/linear/webhook) plus one
//     validated Config shape per type. A single trigger_type/trigger_config
//     column pair on automations (migrations/
//     000055_automations_triggers_and_extras.up.sql), not an
//     automation_triggers side table -- mockups.html's own Automations view
//     shows exactly one trigger per automation row, confirming this shape
//     rather than guessing it. GitHub/Linear's own condition (event/action/
//     label/name/conclusion or event/action/team filter) is fully modeled,
//     validated, AND (a later closing pass) LIVE-DISPATCHED: an inbound
//     GitHub/Linear webhook now reaches MatchesGitHubTrigger/
//     MatchesLinearTrigger for real, through internal/app/automation's own
//     githubdispatch.go/lineardispatch.go, called inline from the existing,
//     already-merged GitHub/Linear webhook ingress handlers
//     (internal/adapters/inbound/github/handler.go's own
//     dispatchAutomationsBestEffort call site, internal/adapters/inbound/
//     linear/webhook.go's own identical one).
//
//     The architectural fork this package's own earlier writeup left open --
//     WHICH event categories a generic automation trigger is even
//     subscribed to, given GitHub's handler is entirely tuned to @mention
//     detection plus one PR-closed merge-gate special case, and Linear's
//     ignores every category but AgentSessionEvent -- is resolved here, in
//     dispatch.go, as an explicit, closed, typed register rather than
//     "whatever arrives": GitHubDispatchAllowlist (pull_request, issues,
//     issue_comment, push, check_run, status) and LinearDispatchAllowlist
//     (Issue, Comment -- AgentSessionEvent deliberately excluded, since that
//     category already belongs to Linear's own existing agent-session
//     pipeline). ClassifyGitHubDispatch/ClassifyLinearDispatch return a
//     named GitHubDispatchSkipReason/LinearDispatchSkipReason for anything
//     outside the register, logged at the call site rather than silently
//     dropped -- expanding either allowlist is a deliberate, reviewed source
//     edit, never an implicit consequence of a new webhook category this
//     deployment happens to start receiving. Automation dispatch is an
//     ADDITIONAL, independent consumer of the same already-deduplicated
//     delivery the @mention/AgentSessionEvent pipelines already process --
//     never a replacement for either, and a panic or error inside dispatch
//     is recovered and logged rather than allowed to suppress them (see
//     each adapter's own dispatchAutomationsBestEffort doc comment).
//
//     dispatch.go ALSO answers the second question live dispatch raises the
//     moment a trigger's own filter matches: WHICH of an automation's own
//     configured target repos does a given event concern?
//     TargetMatchesGitHubEvent scopes by repo (RepoFullNameFromCloneURL,
//     comparing an automation's own Target.URL against the event's
//     repository.full_name) and, when a target's own Branch is set, by
//     branch -- and this is where the branch-CONTAINS-vs-branch-TIP trap is
//     closed: a GitHub `status` event's own branches[] field lists every
//     branch that CONTAINS the commit, never the branch whose CURRENT TIP
//     it is, so membership is decided by TipBranchNames (branches[i].
//     HeadSHA == the event's own SHA) and NEVER by scanning branches[] for a
//     matching NAME alone -- a branch scoped target whose own branch merely
//     contains an ancestor commit must not fire for it. Deduplication
//     reuses the ALREADY-ESTABLISHED webhookDeliveryStore.Claim/Release
//     mechanism the @mention/AgentSessionEvent pipelines already dedupe
//     redeliveries with (§5.1) -- no second, independently-invented
//     dedup mechanism exists for automation dispatch.
//
//   - cron.go: a small, honest, fully-tested 5-field cron matcher
//     (CronMatches/ValidateCronExpr) -- standard vixie-cron field
//     vocabulary (minute hour day-of-month month day-of-week; *, N, A-B,
//     */N, A-B/N, comma lists), deliberately not a byte-for-byte
//     reimplementation of every dialect quirk (no seconds field, no
//     "@daily" macros, no day-of-month/day-of-week OR-instead-of-AND
//     special case). Pure: the caller (app/automation's own new trigger
//     pump) supplies `now`, exactly like IsOrphaned already does for the
//     recovery sweep.
//
//   - sandboxsettings.go: SandboxSettings, the exact same path_scope/
//     mock_configured/contracts_path attributes environment.Environment
//     already carries for an ordinary session, namespaced onto an
//     automation directly (no standalone, reusable Environment-by-id
//     entity exists anywhere in this codebase yet to reference instead).
//     ValidateSandboxSettings reuses environment.ValidatePathScope/
//     ValidateContractsPath directly, never a second, independently-
//     maintained copy of either check.
//
//   - envvar.go: EnvVar, §8.4's own "per-automation env vars" -- PLAIN,
//     non-secret configuration only (see this section's own trailing
//     paragraph below for why per-automation SECRETS are a different,
//     deliberately unbuilt thing).
//
//   - summary.go: BuildArtifactSummary, a deterministic, MECHANICALLY
//     generated one-line sentence over already-persisted, typed
//     invocation-outcome data (target names, succeeded/failed counts) --
//     §8.4's own "artifact_summary populated". Deliberately NOT a
//     model-authored narrative posted through a new agent-facing tool:
//     internal/domain/reviewpost.Summary is this codebase's own
//     established precedent for THAT shape (a required free-text field
//     on a structured verdict-posting tool call review sessions already
//     run at the end of every session) -- but an arbitrary automation's
//     own turn has no equivalent tool call today, and building one
//     (a new OpenCode-facing tool plus a server-side posting endpoint,
//     mirroring §8.2's own review-verdict machinery) would be a
//     materially larger, separate feature. Reusing already-persisted
//     counts/names honestly closes mockups.html's own named gap ("the
//     column exists in the UI but the backend never fills it") without
//     inventing that larger mechanism here.
//
// # Per-automation secrets: deferred to §25.1, deliberately not built here
//
// §8.4 also lists "per-automation secrets" -- NOT implemented here,
// by direct, explicit decision, and named here rather than silently
// dropped (mirroring this codebase's own established discipline of
// documenting an accepted gap explicitly, e.g. app/imagebuild/doc.go's own
// claimed-but-unbuilt sweep, and this package's sibling app/automation/
// doc.go's own "Known, deferred" sections immediately above). Grepped
// directly, twice, before writing this: no `CREATE TABLE ... secret`
// anywhere under migrations/, and no `SecretStore` type anywhere under
// internal/ -- generic secret storage does not exist ANYWHERE in this
// codebase yet. §25.1/§25.3 ("provider credential injection") is explicit
// that this is the first work that actually builds secret storage, scoped
// initially to
// provider API keys mapped provider->env-var name. Building an ad-hoc,
// one-off secrets mechanism here -- before that generic design lands --
// would either conflict with it or be thrown away once it does. EnvVar
// (envvar.go, immediately above) is the deliberate, honest contrast: PLAIN,
// non-confidential per-automation configuration, which carries no such
// risk and is fully implemented here. Once §25.1 lands generic
// secret storage, a small, focused follow-up is expected to extend it to
// automations, alongside the repo/environment/global scopes mockups.html's
// own Settings view already shows resolving in that order ("automation →
// environment → repo → global, this value wins").
package automation
