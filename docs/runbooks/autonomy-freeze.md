# The autonomy freeze

An operator control, not a failure mode: no alert backs it. The freeze
(`docs/TECHNICAL_PLAN.md` §40.2) stops every action that starts without a
person asking for it right then, platform-wide, at once -- the control to
reach for during an incident instead of turning off auto-merge repository by
repository and pausing automations one by one. It never stops a person, and
it never severs a turn already running.

It is one row, `platform_settings` (id = 1): `autonomy_frozen`, and the
when, who and why of the freeze in force. Every site reads it on every
action, with no cache, so a freeze takes effect on each site's next read on
every replica, with no restart.

## What holds while frozen

| Site (`site` label) | What does not happen | Where the held action waits |
|---|---|---|
| `auto_merge` | the auto-merge worker merges nothing, reads nothing from GitHub, confirms nothing | the auto-approved verdict, listed again on the next tick |
| `sentinel_fix_merge` | an allowed sentinel-fix merge gate does not merge | the fix pull request stays open as an ordinary review item; its audit row says `"skipped": "frozen"`. The gate runs once per close event, so it is not re-run after the freeze lifts |
| `sentinel_auto_fix_spawn` | no fix branch, no fix session | its outbox row: pending, attempt not counted, due again every minute (a row born in shadow is not held: see below) |
| `description_autofix` | no rewrite of a pull request's description | its outbox row: pending, attempt not counted, due again every minute (a row born in shadow is not held: see below) |
| `auto_re_review` | no automatic review turn, no budget spent, no GitHub read | the debounce timer, re-armed every minute, with the pushed head kept as its target |
| `automation_cron` | a matched schedule is not fired or claimed | nothing: see "After the freeze lifts" |
| `automation_fan_out` | an invocation starts no run and no session; no failed run, no strike, no auto-pause | the invocation, pending and unclaimed |

An event-triggered automation (GitHub, the issue tracker, the generic
webhook) still records its invocation while frozen -- the webhook still
answers 202 -- and the invocation waits at fan-out.

**Pausing an automation defers its backlog; it does not discard it.** A
paused automation's invocations are not fanned out while it stays paused,
frozen or not, but pausing and resuming touch no invocation: when the
automation is resumed, every invocation it recorded meanwhile -- the ones
held through the freeze included -- fans out, five per tick per replica,
each starting its runs and sessions on today's code. Nothing in the product
discards them yet: keep a runaway automation paused until it can be
resumed safely. A supported way to discard an automation's held invocations
is a planned follow-up, with the freeze's admin action.

An outbox row of the two held kinds that was born in shadow (its
`suppressed_in_shadow` stamp set at enqueue, `docs/TECHNICAL_PLAN.md` §30.8)
is not held: it can only ever be recorded in the suppression ledger, so its
delivery starts nothing. It resolves into the ledger as usual while frozen,
and a person's shadow-to-live Activate, which waits for every such row to
settle, is never kept waiting by the freeze.

## What does not hold

- **A person's command**: a prompt (web, MCP, chat, the issue tracker, a
  code-host mention), a plan approval, the decision inbox's Merge click, the
  label and button re-trigger, a workflow step decision, a stop, a resume.
- **A person's owed review request**: a request that met a moved pull
  request is re-run for its requester (§24.9), frozen or not.
- **A turn already enqueued**, whoever asked for it: holding it would also
  hold every person's prompt queued behind it.
- **A turn already running.** Nothing is sent to its sandbox; it ends on its
  own, or at its deadline (§32.8).
- **Notifications about work already done**, and every other outbox kind.
- **A release's composition review**, the second half of a review a person
  asked for.
- **Infrastructure**: the reconciler, image builds, digests, sweeps, token
  refresh, sandbox rotation, idle stop.

The workflow engine's automatic advance between steps is not held yet.

## Confirm

```json narvi-metrics
{"metrics": ["autonomy_freeze_skip_total"]}
```

- `autonomy_freeze_skip_total{site, reason}` counts every action a site
  skipped. `reason` is `frozen`, or `freeze_unreadable` when the freeze
  could not be read: a read that fails is a skip, never a pass, so a
  database problem holds automatic actions rather than letting them run.
- Each pump logs one Info line per tick with how many actions it held,
  with `outcome=skipped`.
- A held outbox row's `last_error` reads
  `skipped (frozen): the autonomy freeze holds this delivery; attempt not
  counted`, and its `attempts` does not move: that is what tells it from a
  failed delivery.
- To read the freeze in force, on the control plane's database:
  `SELECT autonomy_frozen, autonomy_frozen_at, autonomy_frozen_by,
  autonomy_freeze_reason FROM platform_settings WHERE id = 1;` -- a missing
  row reads as not frozen.

## Setting and lifting it

The admin action -- a Settings card and the routes behind it, each change
audited as `autonomy.frozen` or `autonomy.unfrozen` -- is not built yet, and
nothing in the product writes the row. During an incident before then, an
operator with access to the control plane's database can set it directly.
This writes no audit row: record who froze, when and why in the incident.

```sql
UPDATE platform_settings
SET autonomy_frozen = true, autonomy_frozen_at = now(),
    autonomy_freeze_reason = '<why, 1 to 500 characters>', updated_at = now()
WHERE id = 1 AND NOT autonomy_frozen;

UPDATE platform_settings
SET autonomy_frozen = false, autonomy_frozen_at = NULL, autonomy_frozen_by = NULL,
    autonomy_freeze_reason = NULL, updated_at = now()
WHERE id = 1;
```

## The tail

An action whose last read of the freeze came before the freeze committed
finishes, the way a running turn does. Each is bounded:

- auto-merge: a merge whose read right before the merge preceded the freeze,
  one per candidate, within `GitHubMergePRTimeout`;
- the outbox: a delivery already started, within `OutboxDeliveryTimeout`;
- the automatic re-review: an insert whose read preceded the freeze, one
  turn per session, which then dispatches;
- cron: a claim whose read preceded the freeze -- one pending invocation,
  then held at fan-out;
- fan-out: an invocation whose read preceded the freeze, one per replica;
- a turn enqueued automatically before the freeze dispatches.

## After the freeze lifts

Every held candidate is still a candidate:

- auto-merge and fan-out: on the next tick, within a minute. Held
  invocations fan out in the order they were created.
- the outbox and the automatic re-review: within a minute and five seconds
  (`AutonomyFreezeRecheckInterval` plus the pump's interval). The
  re-review reviews the head pushed last.
- cron: an occurrence the freeze held fires once, on the first tick after
  the freeze lifts, if that tick comes less than ten minutes
  (`AutomationCronCatchUpWindow`) after the occurrence; otherwise it waits
  for its next occurrence. The rule behind it: each tick fires an automation
  at most once, when its schedule matches a minute after the later of its
  last fire and ten minutes before the tick, up to the tick's own minute --
  for an automation that has never fired, after the later of the minute
  before it was created and ten minutes before the tick, so its first
  occurrence is caught up like any other and nothing from before it existed
  fires. A freeze never builds up a burst.

Three existing bounds still apply. An auto-merge candidate older than
`AutoMergeCandidateLookback` (seven days) ages out of the candidate list, as
it would on any idle week, and stays mergeable by a person in the decision
inbox. The cron catch-up window above. And the sentinel-fix merge gate,
evaluated once per close event of the origin pull request.

## The outbox while frozen

A held outbox row is due again every minute and held again, for as long as
the freeze lasts, and more are enqueued while reviews keep running. So while
frozen -- or while the freeze cannot be read -- each pump tick claims the two
held kinds in a lane of their own, beside the batch of every other kind:
however many rows the freeze holds, a notification is never kept waiting
behind them, and each held row is still claimed, held and counted.

A held row's age grows with the freeze. While frozen, `outbox_lag_seconds`
leaves the held kinds out, so a long freeze does not read as a stuck outbox
(`OutboxLagHigh`, [outbox-delivery.md](outbox-delivery.md)); a delivering row
that is due is always claimed in its own lane, so the gauge still reads its
age. `outbox_due_backlog_count` counts every pending row, the held ones
included.

## Rolling back

A binary without the freeze reads no freeze. Rolling back past it lifts any
freeze in force, whatever the row says: turn off auto-merge and the other
automatic toggles per repository, and pause automations, before rolling back
during an incident. A paused automation's invocations wait, and fan out when
it is resumed (above).
