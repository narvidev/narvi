# Production launch checklist

Every item here is grounded in something real and checkable — a config
value, a CI check, a running process's own observable state, or a drill
someone actually performed. Fewer, real items, not a complete-looking
table (the same discipline `docs/runbooks/README.md` already states for
runbooks): an item nobody can independently verify is decoration and does
not belong here.

## 1. `NARVI_ROLLOUT_MODE` is explicitly `cohort`

**Why this is item 1.** `platform.Config.RolloutMode`
(`NARVI_ROLLOUT_MODE`) defaults to `rollout.ModeOpen` when unset
(`internal/platform/config.go`) — deliberately, so Step 76 could land as
a behavior-preserving no-op. That default is exactly wrong for
production: `open` mode admits every repository named on any incoming
session-creation request unconditionally, the moment the deployment goes
live, with none of Step 76's per-repo enrollment gate (§32) actually
engaged. `docs/TECHNICAL_PLAN.md` §32 names this Step (78) as the one
that owns verifying the flip actually happened.

**Check.** On the running production process: `NARVI_ROLLOUT_MODE=cohort`
is set in its own environment (not merely in a config template file
nobody applied). Confirm behaviorally, not just by reading the env file:
attempt a session-creation request for a repository that is deliberately
NOT enrolled and confirm the channel-appropriate refusal from §32.5 (REST:
`403` "repository not enrolled"; GitHub: silent `200`, nothing posted;
Slack/Linear: an honest in-thread/agent-session denial). A boot with an
invalid value never reaches this point at all —
`platform.InvalidRolloutModeError` fails startup outright — so "the
process is running" already proves the value is either `open` or
`cohort`; this check is what distinguishes which one.

**Also confirm**: every repository that must keep working the moment this
flips has already been enrolled (§32.6, seed-manifest-only in v1) —
arming cohort mode with zero repos enrolled refuses every single new
session platform-wide, GitHub included (§32.9's own explicit warning).
Check via `repo_settings.sessions_enabled` for each repository in the
launch cohort, or the `seed.repo_setting_upserted` audit-log row the seed
tool wrote for each.

## 2. CI is green on the exact commit being deployed

Not "CI passed at some point on `main`" — the specific commit SHA this
deployment is built from. Confirm on GitHub: the `checks` job (`go vet`,
`golangci-lint`, `narvichecks`, `go test -race ./...`, `make
contracts-check`) and the `test-integration` job (all four matrix groups)
both green for that SHA. `go test -race ./...` is where
`internal/ops`'s own `TestNoMetricDrift` and `TestNoGuideDrift` run — a
green `checks` job is simultaneously proof that every
`deploy/observability/{dashboards,alerts}` entry names a metric the code
actually emits, and that every `docs/guides/*.md` command maps to a real
route or classifier-routing value, for THIS exact commit.

## 3. The control plane boots cleanly against production config

**Check.** `GET /health` returns `200` from the deployed process, in the
target environment, with real production values for every `NARVI_*`
variable `platform.Config.Load` requires (`internal/platform/config.go`
— roughly 30 required variables: database URL, all three HMAC secrets,
GitHub/Slack/Linear app credentials and webhook secrets, the token
encryption key, the allowlist, the Modal credentials, the intent
classifier's own provider/model, ...). This single check is deliberately
NOT decomposed into one line per variable: `Load` already fails closed on
the first missing/invalid one (`errors.Join`-ing every problem found), so
a clean boot is simultaneously proof every one of those ~30 checks
passed — decomposing it further would be exactly the "an item nobody can
verify independently of the others" decoration this checklist avoids.
A clean boot also proves the embedded migrations applied successfully
(`applyMigrations` runs, advisory-locked, on every boot,
`cmd/control-plane/main.go`) — there is no separate manual migration
step to check.

**Specifically confirm `NARVI_STAGE=production`** (not `staging` left
over from a template) — `Load` accepts all three valid values equally, so
a clean boot alone does not prove this one; check it directly. Getting it
wrong weakens every auth cookie's own `Secure` attribute
(`internal/adapters/inbound/auth`'s own stage-gated cookie policy).

## 4. Each ingress surface's own webhook subscription actually reaches this deployment

A required `NARVI_*` webhook secret being set only proves the control
plane is READY to verify a real webhook — it does not prove GitHub's
App, the Slack App's Events API Request URL, or Linear's own webhook
config actually POINTS at this deployment's real
`NARVI_PUBLIC_BASE_URL`. Check each provider's own delivery log/test
button: GitHub App → recent webhook deliveries show `200`s; Slack App →
Event Subscriptions shows the URL verified (the one-time
`url_verification` handshake, `internal/adapters/inbound/slack/doc.go`);
Linear → the webhook shows recent successful deliveries. See
[docs/guides/slack.md](guides/slack.md#what-starts-a-new-session)/
[linear.md](guides/linear.md#starting-a-session)/
[github.md](guides/github.md#what-triggers-a-session-or-a-turn) for what
a real inbound event from each looks like once this is wired correctly.

## 5. The alerts in `deploy/observability/alerts/*.json` are actually evaluated somewhere

`internal/platform.SetupOTel` exports metrics and traces to **stdout
only** by default — an unset `NARVI_OTLP_ENDPOINT` is a fully supported,
byte-identical-to-before state, not a gap (Step 111, §33). Since Step
111, `cmd/control-plane/main.go` (never `cmd/sandbox-agent`, which cannot
reach one even if it wanted to — see `platform.Config.OTLPEndpoint`'s own
doc comment for why that is structural, not incidental) CAN point
`SetupOTel` at a real OTLP/HTTP collector by setting that var, in which
case both a `TracerProvider` and a `MeterProvider` export there instead of
stdout. Either way, `deploy/observability/alerts/reliability.json`'s own
schema stays deliberately backend-agnostic (`internal/ops/schema.go`'s
`PanelType`/`Alert` doc comments, precisely because no backend is
pinned) — the seven alert rules committed to this repo are correctly
derived (see [`docs/SLOS.md`](SLOS.md)) and CI-checked against real
metric names (`TestNoMetricDrift`), but **evaluating** each rule's own
`condition` and paging someone is still the operator's own collector/
alerting backend's job, never this codebase's (§33.5 keeps §1's refusal
to pin a vendor).

**Check.** Two things, not one: (1) `NARVI_OTLP_ENDPOINT` actually points
this deployment's control plane at a real, reachable collector — an unset
value here silently leaves this whole item unmet, exactly as before Step
108, just no longer for lack of a code path; (2) the alerting backend
that collector feeds lists all seven alert names from
`deploy/observability/alerts/reliability.json` (`OutboxLagHigh`,
`OutboxDeadLetterAny`, `OrphanReapRateHigh`, `BootDurationP95High`,
`SpawnLatencyP95High`, `WatchdogFalseAlarmRateHigh`,
`TurnFalseFailureAny`), each pointed at the correct routed on-call
schedule (see [`docs/ONCALL.md`](ONCALL.md)).

## 6. Uploads are either fully configured or deliberately out of launch scope

Uploads are feature-flagged entirely on `NARVI_OBJECT_STORE_ENDPOINT`
being set (`internal/platform/config.go`) — every upload route either
works end to end or refuses cleanly with nothing half-wired, but only if
this was a deliberate decision. Check: object storage is either (a)
fully configured (`NARVI_OBJECT_STORE_ENDPOINT`/`REGION`/`BUCKET` and
credentials all set, confirmed by a real mint→upload→confirm→fetch round
trip against production object storage) or (b) knowingly left unset for
this launch, with that decision recorded somewhere a later on-call
engineer investigating "uploads don't work" won't mistake for a bug.

## 7. The §32.9 rollback procedure has been drilled once against this deployment, not just read

A rollback procedure nobody has ever actually run is a plan, not a
capability. Check: someone has performed
`docs/TECHNICAL_PLAN.md` §32.9's own per-repository rollback drill
(flip one non-critical enrolled repo's `sessionsEnabled` to `false` via
the seed tool, confirm a fresh `@mention`/REST create against it is
refused per §32.5, confirm `session_rollout_refused_total` incremented,
then re-enroll it) against THIS deployment — not merely against a local
dev environment — before go-live. See [`docs/ONCALL.md`](ONCALL.md) for
the incident-time version of this same drill.

## 8. On-call coverage is real, not aspirational

Check: a named engineer is currently covering this deployment on
whatever paging rotation this organization uses, and that engineer has
actually read [`docs/ONCALL.md`](ONCALL.md) — the entry point this
checklist's own item 7 above, and every alert's own `runbook` field
(`deploy/observability/alerts/*.json`), point at.

## 9. `NARVI_SHADOW_MODE` is unset on this fleet, and no rolling change to it is planned

**Why this is here.** `platform.Config.ShadowMode` (`NARVI_SHADOW_MODE`)
is `docs/TECHNICAL_PLAN.md` §30.8's own deployment-level master switch
for platform shadow mode — read once per process at boot, forcing every
repository's egress shadow for the whole process regardless of what any
individual repository's own settings say. It is built for a
purpose-stood-up evaluation deployment that never serves real customer
traffic, not for a normal production fleet, and it is per-**process**
while a real fleet is multi-pod: changing it with a rolling restart
produces a mixed fleet in which a pod that has already picked up the new
value coexists, for the whole rollout window, with a pod still running
the old one. A shadow-to-live change made that way means a still-live pod
can really deliver an effect a shadow pod already enqueued — the exact
customer-visible leak this whole capability exists to prevent.

**Check.** On every process in this deployment's fleet: `NARVI_SHADOW_MODE`
is unset (or explicitly `false`) — confirm this on the actual running
environment, not merely a config template. If a shadow evaluation is
genuinely needed, it runs on its own, separately provisioned deployment
(§30.10's "minimal safe subset", `NARVI_SHADOW_MODE=1` plus the
credential-starvation and read-only-token work its own Steps add), never
as a value toggled on this one. A boot with an unparseable value never
reaches this point at all — `platform.InvalidShadowModeError` fails
startup outright — so, as with item 1's own `NARVI_ROLLOUT_MODE` check,
"the process is running" already rules out that failure mode; this item
is about the two values `strconv.ParseBool` DOES accept, `true` and
`false`, neither of which boot itself can distinguish from a deliberate
production choice.

## 10. The fleet's Postgres connections fit under `max_connections`

**Why this is here.** A control-plane replica opens its query pool
(`NARVI_DB_POOL_MAX_CONNS`, default 20) plus one lock connection that
holds every one of its session actors' advisory locks, whatever it hosts
(`docs/TECHNICAL_PLAN.md` §2, §5.1). No connection is held per session,
so the total is fixed by the fleet's shape, not by its load:
`replicas × (NARVI_DB_POOL_MAX_CONNS + 1)`, plus whatever else connects
to the same server (migrations run on boot through a short-lived
connection of their own, and any operator tooling). If it does not fit,
replicas fail to connect as they scale out — a failure no single replica
can see coming, since one process cannot know how many replicas there
are.

**Check.** On the production server: `SHOW max_connections;`,
`SHOW superuser_reserved_connections;` and, on Postgres 16 or later,
`SHOW reserved_connections;`. Confirm
`max_connections − superuser_reserved_connections − reserved_connections`
exceeds `replicas × (NARVI_DB_POOL_MAX_CONNS + 1)` with headroom, at the
fleet's maximum replica count (a rolling deploy briefly runs one extra
replica per surge slot). Each replica logs its own share at boot
(`narvi control-plane: postgres connection budget`, with
`replica_need`), and warns — never refuses — when even that one replica
does not fit.

To tell the lock connections apart from the pools: each reports an
`application_name` of `narvi-actor-locks-` followed by a nonce drawn
afresh on every dial, so a replica that redials shows a new name. Count
them with `application_name LIKE 'narvi-actor-locks-%'` in
`pg_stat_activity`, find the locks one holds in `pg_locks` by its pid, and
match it to its replica through that replica's
`sessionactor: lock connection established` log line, which carries the
same `application_name` and `backend_pid`.

**Also confirm** nothing between the control plane and Postgres is a
transaction-mode connection pooler: the actor locks are session-level
advisory locks, and a pooler that hands a backend to another client
between transactions would move them with it. Session-mode pooling, or
none, is required. Behind a session-mode pooler, three of its settings
matter too:

- **Its client-side TCP keepalives and user timeout** (PgBouncer:
  `tcp_keepalive`, `tcp_keepidle`, `tcp_keepintvl`, `tcp_keepcnt`, and
  `tcp_user_timeout`, in milliseconds). The lock connection asks the
  server for short keepalives on its end, and for a TCP user timeout of
  the same reap time (`ActorLockServerKeepalive*` in
  `internal/platform/timeouts.go`), but through a pooler that end faces
  the pooler: if a replica vanishes, only the pooler's own settings
  notice, and until they do the pooler keeps the backend holding that
  replica's session locks. Set the keepalives so their reap time,
  `tcp_keepidle + tcp_keepintvl × tcp_keepcnt`, lies above
  `ActorLockProbeInterval + ActorLockStatementTimeout` (11 s at the
  shipped values) and below `TimerClaimDuration` (30 s): the bounds
  `Validate` keeps the server side's own reap time within, for the same
  reasons. Below the floor, the pooler drops a cut-off replica's lock
  connection and resets its backend, releasing its locks to the other
  replicas, before that replica has found the loss and stopped its
  actors. Above the ceiling, a vanished replica's session locks outlive a
  timer's claim, so the first retry of those sessions' timers still finds
  them held. The server side's 10 s, 5 s and 3 (25 s) fit. Set
  `tcp_user_timeout` between the same bounds, for the same reasons
  (25000 fits): the keepalives only reap a connection with nothing in
  flight. Linux sends no keepalive probe while data it sent is
  unacknowledged, so a replica lost while the pooler's reply to one of its
  lock statements was in flight is otherwise dropped only when TCP's
  retransmissions give up — about 15 minutes at Linux's default
  `tcp_retries2` — its session locks held all the while. PgBouncer's
  default, 0, leaves it at that, and no other replica ends that backend:
  only the lost replica's own next lock connection would.
- **Its client idle timeout** (PgBouncer: `client_idle_timeout`). Leave it
  at 0, its default, or above the same floor: a lock connection sits idle
  between probes, and a pooler that closes an idle client resets its
  backend all the same.
- **How it passes `application_name` on.** It must set each client's own
  `application_name` on the backend it links, as PgBouncer does (the
  parameter is one it tracks). The first lock connection a replica dials
  after a loss terminates the lost connection's backend if it still runs
  that connection's session, found by the lost connection's own name;
  behind a pooler that left a reused backend under an earlier client's
  name, it could end another replica's live lock connection instead. To
  check: through the pooler, connect twice in turn with different
  `application_name` values, and confirm
  `SELECT current_setting('application_name')` returns each connection's
  own.
