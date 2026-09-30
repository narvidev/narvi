// Package outboxworker is the process-wide background outbox-delivery
// loop ("outbox delivery", §5.1/§9.3 scenario 9) -- a sibling of
// app/reconciler and app/imagebuild, not folded into either
// (TECHNICAL_PLAN.md §1's own repo-layout convention: one package per
// major loop/subsystem under internal/app/).
//
// Builder.Run (mirroring app/imagebuild.Builder.Run/app/reconciler.
// Reconciler.Run's own identical shape) ticks every platform.Timeouts.
// OutboxPumpInterval, calling PumpOnce -- exported separately, exactly
// like ReconcileOnce/imagebuild's own PumpOnce, so tests can drive exactly
// one tick deterministically. PumpOnce:
//
//  1. Claims a batch of outbox rows eligible to (re)attempt delivery now
//     (status='pending', next_attempt_at elapsed) -- ONE transaction:
//     SELECT ... FOR UPDATE SKIP LOCKED, then per-row UPDATE bumping
//     next_attempt_at forward by platform.Timeouts.OutboxClaimDuration (a
//     provisional protection window -- the outbox table, unlike
//     image_builds' own 'building' status, has no third, in-flight status
//     to mark a row claimed with) and incrementing attempts, committed
//     BEFORE any real notifier call -- mirrors app/imagebuild.Builder.
//     claimBatch/app/sessionactor/timerpump.go's own claimDueTimers
//     precedent exactly.
//  2. For each claimed row, OUTSIDE any transaction, bounded by platform.
//     Timeouts.OutboxDeliveryTimeout: renews THIS row's own
//     claim-protection window via a genuine optimistic-concurrency
//     compare-and-swap (RenewOutboxClaim, guarded by both status='pending'
//     AND next_attempt_at still matching the value this row carried when
//     last claimed/renewed, from a fresh time.Now() taken right before the
//     real call, WITHOUT incrementing attempts again -- audit fix H6, see
//     attempt()'s own doc comment for the batch-level claim-lease race
//     this closes, and why status alone is not enough to close it), then
//     routes to whichever
//     of the three ports.Notifier implementations (internal/adapters/
//     outbound/{slackapi,linearapi via linearNotifier,githubapi}) owns the
//     row's own kind, via a caller-supplied kind->Notifier map (constructed
//     once in cmd/control-plane/main.go). On success: MarkOutboxEntryDelivered.
//     On failure: domain/outbox.EvaluateBackoff (fed the row's own
//     post-claim attempts count) decides RecordOutboxEntryFailure
//     (schedule next_attempt_at) vs MarkOutboxEntryDeadLetter (attempts
//     have exhausted domain/outbox.MaxAttempts).
//
// One row's delivery failure (or any error recording its outcome) is
// logged and does NOT abort the rest of the batch -- exactly like
// app/imagebuild.Builder.attempt's own per-row isolation.
//
// A delivery this process's own shutdown cuts short is not a failed
// attempt (§5.1). The Builder reads the process's shutdown state
// (platform.ShutdownState, which the control plane sets as its drain
// begins, before the worker's context ends) -- never a cancellation error
// -- and domain/outbox.EvaluateFailure decides: the claim's attempt is given
// back (the deferred class, DeferOutboxEntry) and the row is due again at
// once, unless the kind is one a second delivery would add to
// (repeatability.go), the delivery outlived its own OutboxDeliveryTimeout,
// or the row has been interrupted more times in a row than
// OutboxMaxConsecutiveInterruptions allows (outbox.consecutive_interruptions,
// reset when an attempt completes). The rest of the batch, never started,
// is handed back with its attempts, and every outcome recorded after the
// shutdown began is written on one context per tick that outlives it,
// bounded by OutboxShutdownRecordTimeout. None of it is logged at error
// level: each is a warning naming the shutdown and the rule applied.
//
// Three OTel instruments are constructed once, at NewBuilder time
// (mirroring app/imagebuild's own image_build_failure_streak precedent):
// the outbox_lag gauge (§5.3's own "outbox lag" observability item,
// an explicit requirement there) observes, once per
// tick, the age (in seconds) of the oldest still-due pending row at the
// START of that tick's own claim step -- zero when nothing is overdue;
// the outbox_due_backlog_count gauge (audit fix M15/M17) observes, once
// per tick, a genuine COUNT(*) of every 'pending' row regardless of
// whether it is due yet -- independent of outbox_lag_seconds, which reads
// zero whenever every currently-pending row happens to be mid-backoff, a
// real blind spot during a sustained outage; and the outbox_dead_letter
// counter increments once per row this Builder dead-letters.
//
// attempt() also logs correlation_id (Batch 11 audit-fix scope) alongside
// session_id -- the enqueueing request/webhook's own platform.
// CorrelationIDFromContext value, threaded through from app/sessionactor's
// own enqueueOutboxNotification (outboxenqueue.go) at enqueue time via the
// outbox table's own correlation_id column, null when the enqueuing
// context carried none.
//
// linearnotifier.go's own linearNotifier is this package's own small
// Linear-specific ports.Notifier wrapper (holding a *linearapi.Client, a
// *postgres.LinearInstallationStore, and platform.Config.
// TokenEncryptionKey): it looks up the target workspace's real Linear API
// credential FRESH, by organization_id, at delivery time (decrypt via
// platform.DecryptToken), and never inside internal/adapters/outbound/
// linearapi itself -- keeping that package free of a postgres-store
// dependency it would otherwise be the only outbound adapter to carry (a
// hard security requirement: a decrypted token must never sit in the
// outbox payload at rest, and this package -- not linearapi -- is where
// the postgres.LinearInstallationStore dependency this lookup needs
// already naturally lives, alongside every other store this Builder's own
// wiring already threads through). An audit-fix batch (finding M16,
// "completeness", internal/adapters/outbound/linearapi/doc.go's own
// "future Step" note) extends linearNotifier to ALSO handle
// ports.NotificationKindLinearProgress rows -- a mid-turn "thought"
// AgentActivity, enqueued by app/sessionactor (progressnotify.go) the
// first time a Linear-origin session's turn processes a tool_call wire
// event -- routed to the exact SAME linearNotifier instance already
// registered for ports.NotificationKindLinear, which now dispatches on
// notification.Kind internally (mirroring planSlackNotifier's own
// established precedent for one wrapper type handling more than one kind).
package outboxworker
