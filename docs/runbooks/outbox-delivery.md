# Outbox delivery stalled or dead-lettering

Backs alerts: `OutboxLagHigh`, `OutboxDeadLetterAny`
(`deploy/observability/alerts/reliability.json`). Dashboard:
[turns-and-delivery.json](../../deploy/observability/dashboards/turns-and-delivery.json).

## Symptom

Users stop seeing outbound notifications for events that should produce
one: a Slack/Linear thread reply, a GitHub check/comment update, a
plan-approval-request message, a turn-completion notice. The web UI (which
reads turn/session state directly, not via the outbox) still looks
correct — this is specifically an *outbound-channel* symptom.

## Confirm

```json narvi-metrics
{"metrics": ["outbox_lag_seconds", "outbox_due_backlog_count", "outbox_dead_letter_total"]}
```

- `outbox_lag_seconds` (gauge) rising and staying above its usual
  near-zero baseline. Zero only means nothing was *due and claimed* this
  tick — during a sustained outage every pending row can be mid-backoff at
  once, so also check:
- `outbox_due_backlog_count` (gauge) — total pending rows including
  mid-backoff ones. This is the one that stays honest during a backoff
  storm.
- While autonomy is frozen ([autonomy-freeze.md](autonomy-freeze.md)),
  the `sentinel_auto_fix` and `github_description_autofix` rows are held,
  not stuck: pending, `last_error` starting `skipped (frozen)`, `attempts`
  not moving, due again every minute. `outbox_lag_seconds` leaves them out
  while frozen; `outbox_due_backlog_count` still counts them.
- `outbox_dead_letter_total` (counter) incrementing at all — each
  increment is one entry that exhausted `domain/outbox.MaxAttempts` (10)
  delivery attempts (`internal/domain/outbox/backoff.go`) and will never
  be retried automatically again.
- Logs: `"outboxworker: tick failed"` (a whole pump tick errored — check
  the `error` field; a single bad tick doesn't kill the loop, but a
  *repeated* one means something structural, e.g. the Postgres claim query
  itself failing) and, per dead-lettered entry, the log line at
  `internal/app/outboxworker/builder.go`'s own dead-letter branch (carries
  `max_attempts` and a `last_error` attribute — the SAME underlying
  delivery error persisted to the `outbox.last_error` column, but passed
  through `redactURLCredentials` first, `internal/app/outboxworker/
  redact.go`, in case a future notifier's own delivery target ever embeds
  a credential in a URL; the DB column itself is left unredacted, for the
  rare case an operator genuinely needs the raw value).
- A deploy or restart is not an outage. A delivery the pod's own shutdown
  cut short logs a *warning*, never an error, naming the shutdown and a
  `rule`: `shutdown_interrupted` (handed back, its attempt not spent, due
  again after `OutboxInterruptedSettleDelay`, 45 s, so a request the
  shutdown cut after the remote end accepted it lands before the repeat),
  `shutdown_before_start` (claimed but never started, handed back and due
  at once), or one of the three that count the attempt all the same and
  wait the later of their backoff and that settle delay —
  `shutdown_interrupted_not_repeatable` (a kind a second
  delivery would add to: a Slack post, a Linear activity, a GitHub comment
  or review, an RWX dispatch), `shutdown_interrupted_outlived_delivery_timeout`,
  or `shutdown_interrupted_past_bound` (interrupted more than
  `OutboxMaxConsecutiveInterruptions` times in a row, 3; the row's
  `outbox.consecutive_interruptions` holds the run). Many
  `shutdown_interrupted_past_bound` lines for one row mean its delivery is
  what keeps meeting a shutdown — investigate the pod restarts, not the
  notifier.

## Remediation

1. **Identify which notifier is failing.** `outboxworker.Builder` routes
   by `ports.NotificationKind` to one of the Slack/Linear/GitHub notifier
   adapters (`internal/adapters/outbound/{slackapi,linearapi,githubapi}`).
   The dead-letter log's own `last_error` attribute (or a direct read of
   `ListDeadLetter`, which returns the same string unredacted) names the
   underlying HTTP failure — a 401/403 usually means a revoked/expired bot
   token or webhook credential; a sustained 5xx means the third-party API
   itself is degraded (exactly §9.3 scenario #9's own scripted case,
   below); a 4xx on every attempt for one channel and not others narrows
   it to that channel's own credential or payload. **A `last_error` of
   `no notifier registered for kind "..."` is not a delivery failure at
   all** but a kind this deployment does not deliver: for a `github_*`,
   `sentinel_auto_fix`, `handoff_sentinel` or `release_manifest` row it
   means GitHub outbound is off (`NARVI_OUTBOUND_ENABLED` without
   `github` -- the boot log's `narvi control-plane: GitHub axes` line says
   `outbound=false`; `docs/TECHNICAL_PLAN.md` §12.5), so nothing will
   ever post it. Either the deployment is meant to post as the bot --
   declare `NARVI_OUTBOUND_ENABLED=github` with a real
   `NARVI_GITHUB_BOT_TOKEN` and restart -- or the rows are expected
   leftovers of a surface it deliberately does not call.
2. **A transient third-party outage self-heals.** `domain/outbox.
   EvaluateBackoff`'s own schedule keeps retrying (`OutboxBackoffBase` 30s
   up to `OutboxBackoffMax` 5m, `internal/platform/timeouts.go`) until
   either delivery succeeds or `MaxAttempts` (10) is exhausted — no
   operator action needed while `outbox_lag_seconds`/
   `outbox_due_backlog_count` are elevated but `outbox_dead_letter_total`
   is flat. `outbox_lag_seconds` measures a row's *cumulative* age since
   creation, not the delay of any single retry — a healthy row that has
   retried several times legitimately accumulates far more than one
   `OutboxBackoffMax` window's worth of lag. `OutboxLagHigh`'s own 2400s
   threshold is instead derived from the FULL 9-retry schedule's own
   cumulative sum (30s+60s+120s+240s+300s×5 = 1950s) plus worst-case
   per-cycle overhead, with headroom (`deploy/observability/alerts/
   reliability.json`'s own `thresholdDerivation` has the full arithmetic)
   — so it does NOT fire on ordinary backoff, however many retries deep;
   if it's firing, a row has plausibly exhausted its entire legitimate
   retry budget without ever un-sticking (or being dead-lettered).
3. **A credential problem needs a human fix.** Rotate/reissue the failing
   channel's own bot token or webhook secret (outside this codebase — the
   relevant provider's own admin console), then confirm new attempts on
   already-pending rows succeed. **The real bound is up to one full
   `OutboxBackoffMax` (5 minutes), not a few `OutboxPumpInterval` ticks**:
   each already-pending row carries a `next_attempt_at` stamped by its OWN
   last failed attempt (30s after the 1st failure, up to a full
   `OutboxBackoffMax` once a row has failed enough times to be on the
   5-minute plateau — `domain/outbox.EvaluateBackoff`'s own schedule,
   above), and `ListDuePendingOutboxEntries` will not claim a row before
   that timestamp elapses regardless of how often the pump ticks in the
   meantime. There is no operator action in this codebase that forces an
   earlier retry on an already-scheduled row — `OutboxStore` exposes no
   requeue/reset-`next_attempt_at` method (`internal/adapters/outbound/
   postgres/queries/outbox.sql`) — so the honest answer, after a
   credential fix, is: wait up to 5 minutes per row (most will retry
   sooner, depending on how many times each has already failed), watching
   `outbox_lag_seconds`/`outbox_due_backlog_count` fall back toward
   baseline. If nothing has drained after a full `OutboxBackoffMax` window,
   the credential fix did not work — re-check step 1, don't assume it just
   needs longer.
4. **Already dead-lettered rows are not automatically replayed.** This is
   a genuine, currently-unbuilt gap, not a hidden button: `OutboxStore`
   (`internal/adapters/outbound/postgres/outbox_store.go`) exposes
   `ListDeadLetter` to inspect them, but no requeue/replay method exists
   yet. Recovering a specific dead-lettered notification today means
   re-triggering whatever original action produced it (e.g. re-posting a
   review verdict, re-running the automation) through the product surface
   that creates that notification in the first place — not resurrecting
   the dead-lettered outbox row itself.

## Resilience scenario

§9.3 scenario #9 ("Outbox: Slack API 500s") proves the backoff-then-
recover half of this runbook end to end against a real
`outboxworker.Builder` and a fake-Slack `httptest.Server` scripted to
return 500 before recovering:
`TestResilienceScenario9_Outbox_SlackAPI500sThenRecovers_EventuallyDeliveredNoLoss`
— `test/resilience/scenario9_outbox_retry_test.go`.
