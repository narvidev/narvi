-- Queries backing Outbox (§4.3, §5.1). CreateOutboxEntry/GetOutboxEntry
-- prove the pipeline end to end (§5.1); the next five back §5.1's
-- ("outbox delivery") own claim/attempt/record delivery-worker loop
-- (internal/app/outboxworker), mirroring internal/adapters/outbound/
-- postgres/queries/image_builds.sql's own ListDue/Claim/RecordSuccess/
-- RecordFailure shape closely -- see that file's own doc comments for the
-- general "claim inside one transaction, attempt outside any transaction"
-- discipline this mirrors. RenewOutboxClaim/CountPendingOutboxEntries at
-- the bottom are a later audit fix (H6/M15-M17) -- see each query's own
-- doc comment below.
--
-- Unlike image_builds, the outbox table has no third, in-flight status
-- distinct from pending/delivered/dead_letter (migrations/000010_outbox.
-- up.sql's own outbox_status enum is exactly pending/delivered/
-- dead_letter) -- so ClaimOutboxEntry below cannot flip status the way
-- ClaimImageBuild flips to 'building'. Instead it bumps next_attempt_at
-- forward by the caller's own claim-protection window (platform.Timeouts.
-- OutboxClaimDuration), mirroring app/sessionactor/timerpump.go's own
-- ClaimDueTimer precedent exactly -- see that query's own doc comment for
-- why a provisional forward-bump at claim time is the correct mechanism
-- here.

-- name: CreateOutboxEntry :one
-- correlation_id (migrations/000037_outbox_correlation_id.up.sql) is
-- nullable, mirroring audit_log.correlation_id's own identical
-- convention: every caller (internal/app/sessionactor/outboxenqueue.go's
-- own enqueueOutboxNotification; internal/adapters/inbound/httpapi/
-- decideplan.go's own enqueuePlanDecisionNotifications) passes
-- platform.CorrelationIDFromContext(ctx)'s value when the enclosing
-- request/webhook context carried one, else NULL -- no id is ever invented
-- at enqueue time for a row created outside such a context.
--
-- suppressed_in_shadow (migrations/000103_outbox_shadow_epoch.up.sql,
-- §30.8) is ALWAYS computed by postgres.OutboxStore.Create itself, never
-- left to a caller -- this is the single choke point every enqueue site
-- shares (there is exactly one INSERT INTO outbox in this codebase), so
-- the stamp cannot be forgotten one call site at a time. See that
-- method's own doc comment for the resolution formula.
INSERT INTO outbox (session_id, kind, payload, correlation_id, suppressed_in_shadow)
VALUES ($1, $2, $3, $4, $5)
RETURNING *;

-- name: GetOutboxEntry :one
SELECT * FROM outbox
WHERE id = $1;

-- name: ListDuePendingOutboxEntries :many
-- outboxworker.Builder's own poll query: every 'pending' row whose own
-- next_attempt_at has elapsed, oldest-due first. FOR UPDATE SKIP LOCKED
-- mirrors ListDueImageBuilds/ListDueTimers' own identical precedent --
-- multiple control-plane pods may run this same background loop
-- independently; SKIP LOCKED lets two concurrent pods each claim a
-- DISJOINT batch instead of blocking on each other or double-claiming the
-- same row.
SELECT * FROM outbox
WHERE status = 'pending' AND next_attempt_at <= now()
ORDER BY next_attempt_at
LIMIT $1
FOR UPDATE SKIP LOCKED;

-- name: ListDuePendingOutboxEntriesOfKinds :many
-- ListDuePendingOutboxEntries restricted to the rows of kinds: one lane of
-- a pump tick while the autonomy freeze holds some kinds (technical plan
-- §40.2, outboxworker's claimBatch), which runs it once for the kinds the
-- freeze holds and once for every other kind it knows. A row the freeze
-- holds is due again every recheck interval; in the one oldest-due-first
-- batch, enough of them would take every slot and keep the notifications
-- behind them waiting. Each row of a held kind is still claimed in its own
-- lane and its hold decided and recorded at the delivery's call site
-- (Builder.attempt), never skipped by this query.
--
-- The kinds are an equality list, never an exclusion, so the lane seeks
-- outbox_pending_kind_due_idx (migrations/000164) on (kind, next_attempt_at)
-- and reads the due pending rows of its own kinds and no others, whatever
-- the table holds -- delivered rows are never deleted. Same order, lock
-- and SKIP LOCKED as its sibling.
SELECT * FROM outbox
WHERE status = 'pending' AND kind = ANY(sqlc.arg('kinds')::text[]) AND next_attempt_at <= now()
ORDER BY next_attempt_at
LIMIT sqlc.arg('max_rows')
FOR UPDATE SKIP LOCKED;

-- name: ClaimOutboxEntry :one
-- The claim half of the pump's own two-step (claim-then-attempt-outside-
-- any-transaction) shape: bumps next_attempt_at forward by the caller's
-- own OutboxClaimDuration protection window and increments attempts,
-- committed BEFORE the real notifier call is ever attempted -- mirrors
-- ClaimImageBuild's own "attempt_count counts ATTEMPTS made, not merely
-- failures" convention exactly, so domain/outbox.EvaluateBackoff is later
-- asked to schedule (or dead-letter) THIS attempt using the post-increment
-- count. Guarded by "AND status = 'pending'" so a stale/already-superseded
-- row (should be impossible given ListDuePendingOutboxEntries' own WHERE
-- clause, but defensive, mirroring RecordImageBuildSuccess/Failure's own
-- identical guard) is a harmless no-op.
UPDATE outbox
SET attempts = attempts + 1, next_attempt_at = $2
WHERE id = $1 AND status = 'pending'
RETURNING *;

-- name: MarkOutboxEntryDelivered :one
-- Records a successful delivery: status='delivered', delivered_at=now().
-- Guarded by "AND status = 'pending'", mirroring RecordImageBuildSuccess's
-- own identical guard against a stale/already-superseded row.
-- consecutive_interruptions goes back to zero: this attempt completed
-- (§5.1, migrations/000152).
UPDATE outbox
SET status = 'delivered', delivered_at = now(), consecutive_interruptions = 0
WHERE id = $1 AND status = 'pending'
RETURNING *;

-- name: MarkOutboxEntryDeliveredToLedger :one
-- §30.6/§30.8's own terminal mark: records that this row's own effective
-- egress mode was shadow at the moment outboxworker.Builder.attempt
-- would otherwise have called notifier.Deliver, so it delivered the row
-- into the suppression ledger instead of the world -- status='delivered'
-- (the SAME terminal status a genuine delivery reaches; §30.6 is explicit
-- this is "a flag column, deliberately not a new status enum"), plus
-- delivered_to_ledger=true so a later reader (§30.6's own "UNION over
-- marked outbox rows + shadow_scm_writes" read model) can tell the two
-- apart. Same "AND status = 'pending'" guard as
-- MarkOutboxEntryDelivered, for the identical reason -- and the same
-- pgx.ErrNoRows-means-superseded handling: a caller must not proceed to
-- treat this row as terminal if some other builder already raced ahead
-- of it.
--
-- suppressed_in_shadow is set here TOO, and that is not redundant with
-- the enqueue stamp. A row born LIVE and suppressed at delivery -- the
-- demotion half of §30.8's suppress-wins rule -- kept a false stamp,
-- while the operator ledger's own read is keyed on exactly that column.
-- So a suppression that genuinely happened was invisible on the one
-- surface built to show it. The column records the row's EFFECTIVE mode,
-- and by the time this statement runs that mode is shadow whichever half
-- of the rule decided it.
--
-- consecutive_interruptions goes back to zero, as in
-- MarkOutboxEntryDelivered: this attempt completed.
UPDATE outbox
SET status = 'delivered', delivered_at = now(), delivered_to_ledger = true, suppressed_in_shadow = true, consecutive_interruptions = 0
WHERE id = $1 AND status = 'pending'
RETURNING *;

-- name: RecordOutboxEntryFailure :one
-- Records a failed delivery attempt that is still eligible for another
-- retry: next_attempt_at is the caller's own domain/outbox.EvaluateBackoff-
-- computed value (overwriting ClaimOutboxEntry's own provisional bump
-- above with the real decision), last_error captures the notifier's own
-- error for observability. attempts is NOT incremented again here --
-- ClaimOutboxEntry already counted this attempt. Same "AND status =
-- 'pending'" guard as MarkOutboxEntryDelivered, for the identical reason.
--
-- consecutive_interruptions is the value domain/outbox.EvaluateFailure
-- decided (§5.1, migrations/000152): zero for an attempt that completed
-- and failed on its own merits, one more for a delivery this process's
-- shutdown cut short that counts all the same -- past
-- platform.Timeouts.OutboxMaxConsecutiveInterruptions, past its own
-- delivery timeout, or of a kind not safe to repeat.
UPDATE outbox
SET next_attempt_at = $2, last_error = $3, consecutive_interruptions = sqlc.arg('consecutive_interruptions')
WHERE id = $1 AND status = 'pending'
RETURNING *;

-- name: MarkOutboxEntryDeadLetter :one
-- Records a failed delivery attempt that has exhausted domain/outbox.
-- MaxAttempts: status='dead_letter', last_error captures the notifier's
-- own final error. Same "AND status = 'pending'" guard as
-- MarkOutboxEntryDelivered/RecordOutboxEntryFailure above.
-- consecutive_interruptions is the decided value, as in
-- RecordOutboxEntryFailure.
UPDATE outbox
SET status = 'dead_letter', last_error = $2, consecutive_interruptions = sqlc.arg('consecutive_interruptions')
WHERE id = $1 AND status = 'pending'
RETURNING *;

-- name: DeferOutboxEntry :one
-- Records a failure of the class that does not consume an attempt
-- (domain/outbox.ClassDeferred, §5.1 and §44.2): gives back the attempt
-- ClaimOutboxEntry counted and makes the row due again at the caller's
-- own next_attempt_at, without moving it toward dead-letter. Its causes
-- today: this process's own shutdown cutting a delivery short, or reaching
-- a claimed row before its delivery started; and the autonomy freeze
-- (§40.2) holding a delivery that is itself an automatic action
-- (outboxworker's holdFrozen). A rate limit whose deadline GitHub stated
-- (§44.2) is the next.
--
-- A genuine compare-and-swap on next_attempt_at, like RenewOutboxClaim's,
-- not only a status guard: this statement takes an attempt BACK, so it must
-- not land on a row another builder has re-claimed since this caller last
-- observed it -- that builder's own claim counted ITS attempt, and taking
-- it back would let the row retry past MaxAttempts. Once another builder
-- has moved next_attempt_at on, this matches no row (pgx.ErrNoRows), and
-- the attempt stays counted. GREATEST keeps attempts from going below zero
-- whatever a caller passes.
--
-- last_error is left as it was when the caller passes NULL (a row whose
-- delivery never started has nothing to say), and
-- consecutive_interruptions is the decided value: one more for an
-- interrupted delivery, unchanged for a row never started.
UPDATE outbox
SET attempts = GREATEST(attempts - 1, 0),
    next_attempt_at = $2,
    last_error = COALESCE(sqlc.narg('last_error'), last_error),
    consecutive_interruptions = sqlc.arg('consecutive_interruptions')
WHERE id = $1 AND status = 'pending' AND next_attempt_at = sqlc.arg('expected_next_attempt_at')
RETURNING *;

-- name: RenewOutboxClaim :one
-- Audit fix (H6, correctness -- internal/app/outboxworker/builder.go): the
-- per-row re-claim/heartbeat outboxworker.Builder's own attempt() calls
-- immediately before the real notifier.Deliver call, closing the batch-
-- level claim-lease race ClaimOutboxEntry above cannot close on its own.
-- claimBatch computes ONE shared time.Now() and calls ClaimOutboxEntry for
-- EVERY row in a batch (up to pumpBatchSize) before any of them are
-- actually delivered; PumpOnce then attempts each claimed row
-- SEQUENTIALLY, so a row late in the batch could still be waiting for its
-- own attempt() call to even start after its shared claim-expiry
-- timestamp has already elapsed -- letting a concurrent tick (this pod's
-- next one, or another pod's own Builder) re-claim this same row.
--
-- A first version of this fix guarded only "AND status = 'pending'",
-- exactly like every other guarded outbox UPDATE in this file -- but
-- status alone CANNOT distinguish "no one else has touched this row since
-- I last observed it" from "a DIFFERENT builder already re-claimed (or
-- renewed) this row and is now mid-delivery on it": both leave status at
-- 'pending' (the outbox table has no third, in-flight status). That gap
-- is real and empirically reproducible: builder A's batch-level claim on
-- a row can lapse before A's own sequential attempt() loop reaches it;
-- builder B then legitimately re-claims and starts delivering the SAME
-- row; if B's own delivery is still in flight when A finally reaches its
-- turn, a status-only renewal would ALSO succeed for A (status is still
-- 'pending' -- B has not called MarkDelivered yet), so A would ALSO call
-- notifier.Deliver on the same row concurrently with B.
--
-- This query is therefore a genuine optimistic-concurrency compare-and-
-- swap, not just a status check: sqlc.arg('expected_next_attempt_at') is
-- the next_attempt_at value the CALLER last observed for this row (from
-- its own prior ClaimOutboxEntry or RenewOutboxClaim return), and the
-- WHERE clause requires the row's CURRENT next_attempt_at to still match
-- it. If a different builder won the race in between, THAT builder's own
-- claim/renewal already changed next_attempt_at away from the value this
-- caller last observed, so this CAS correctly matches zero rows here
-- (pgx.ErrNoRows) instead of renewing a lease someone else already holds
-- -- attempt() already treats that error as "stop, do not deliver". This
-- is what gives the renewal real single-writer teeth: at most one
-- builder's renewal for a given prior next_attempt_at value can ever
-- succeed, so at most one builder proceeds to notifier.Deliver for this
-- row at a time.
--
-- Bumps next_attempt_at forward by the caller's own OutboxClaimDuration
-- from a FRESH time.Now() taken at the moment of the real attempt -- not
-- the batch's shared claim-time now. Deliberately does NOT touch attempts:
-- ClaimOutboxEntry already counted this attempt at batch-claim time, and
-- this is purely a claim-protection renewal, not a new delivery attempt.
UPDATE outbox
SET next_attempt_at = $2
WHERE id = $1 AND status = 'pending' AND next_attempt_at = sqlc.arg('expected_next_attempt_at')
RETURNING *;

-- name: CountPendingOutboxEntries :one
-- Audit fix (M15/M17, the lag-metric blind spot): outboxworker.Builder's
-- own outbox_lag_seconds gauge is computed ONLY from rows the CURRENT
-- tick's own claimBatch actually claimed (ListDuePendingOutboxEntries
-- above, which only returns rows whose own next_attempt_at <= now()) --
-- during a sustained notifier outage, every failed delivery schedules a
-- real domain/outbox.EvaluateBackoff-computed retry, so at any given tick
-- it is entirely possible every currently-pending row is mid-backoff (its
-- own next_attempt_at in the future, not due yet) even though there is a
-- large, genuinely stuck backlog -- in that moment outbox_lag_seconds
-- silently reads zero. This query backs a SECOND, independent gauge
-- (outbox_due_backlog_count) that counts EVERY 'pending' row, deliberately
-- NOT restricted to next_attempt_at <= now() like ListDuePendingOutboxEntries
-- -- the whole point is to see rows currently cooling down in backoff too,
-- which the due-only lag metric structurally cannot see. Deliberately
-- outside claimBatch's own transaction: a cheap, standalone read, no FOR
-- UPDATE, no lock contention with the claim step.
SELECT COUNT(*) FROM outbox
WHERE status = 'pending';

-- name: ListDeadLetterOutboxEntries :many
-- §16 ("decision inbox: read model + API", §16.1)'s own
-- needs_attention row source: every outbox row that exhausted retries
-- (MarkOutboxEntryDeadLetter above), bounded by $1 (§21.1's own "bounded
-- from day one" discipline). Ordered by created_at, NOT a dedicated
-- "became dead-lettered at" timestamp -- this table has none (verified:
-- migrations/000010_outbox.up.sql's own columns are id/session_id/kind/
-- payload/status/attempts/next_attempt_at/delivered_at/last_error/
-- created_at/correlation_id; no dead_lettered_at) -- adding one now would
-- be a new WRITE this Step's own read-model scope deliberately excludes
-- ("derive it, do not add a writer"). created_at therefore honestly
-- UNDER-estimates true time-in-dead-letter-state (a row dead-letters some
-- time strictly after its own creation, never before), which is the safe
-- direction for a STALENESS flag (over-flagging a row as old enough to
-- need attention is the harmless failure mode here) -- but is NOT precise
-- enough to feed the decision-latency metric's own median, so
-- internal/app/decisioninbox deliberately excludes dead-lettered outbox
-- rows from that specific computation (scoped instead to PR merges and
-- plan decisions, where a real actioned-at instant exists) while still
-- surfacing them in the queue itself.
--
-- ADMIN-ONLY at the RBAC/httpapi layer (§16.1's own parenthetical),
-- mirroring ListFailedSessions' own identical "no per-user filter, an ops
-- view is system-wide" reasoning -- an outbox row has no single owning end
-- user in the first place (CountPendingOutboxEntries' own doc comment
-- above: outbox rows aren't user-scoped).
SELECT * FROM outbox
WHERE status = 'dead_letter'
ORDER BY created_at DESC
LIMIT $1;

-- name: GetLatestOutboxEntryByKindPrefix :one
-- §12.5's own ("integrations read model & routes" amendment) GET
-- /api/integrations: "what did Narvi last try to POST to this surface" --
-- the most-recently-CREATED (never delivered_at, which is NULL for a
-- still-pending or dead-lettered row) outbox row whose own kind begins
-- with sqlc.arg('kind_prefix') -- httpapi/integrations.go passes each of
-- internal/domain/integrations.Providers' own three literal names
-- ("slack"/"linear"/"github") here, one call per provider, NEVER a
-- caller-supplied value (this read model has no user input at all). The
-- '%' wildcard is appended HERE, in SQL, rather than by the Go caller
-- concatenating it onto kind_prefix itself, so the "prefix means LIKE
-- X%" mechanism lives in exactly one place -- see
-- internal/domain/integrations.ProviderForOutboxKind's own doc comment
-- for the SAME naming-convention fragility this LIKE match shares
-- (a kind that does not literally begin with its own provider's name,
-- e.g. "sentinel_auto_fix"/"handoff_sentinel"/"release_manifest" as of
-- this query's own introduction, silently never matches ANY provider's
-- prefix here either -- the identical, deliberately-not-hidden gap).
-- pgx.ErrNoRows (unwrapped) means this provider has never had an outbox
-- row at all -- "no outbound attempt on record", never a store error.
SELECT * FROM outbox
WHERE kind LIKE sqlc.arg('kind_prefix')::text || '%'
ORDER BY created_at DESC
LIMIT 1;

-- name: ListShadowSuppressedOutboxWithSessionRepos :many
-- The shadow-operator surface's own UNION read model (§30.6: "the read is a UNION over
-- marked outbox rows + shadow_scm_writes"). Every row this deployment
-- ever stamped suppressed_in_shadow=true at enqueue (migrations/
-- 000103_outbox_shadow_epoch.up.sql), joined with its owning session's
-- own raw repos JSONB column -- mirrors ListLiveSandboxesWithSessionRepos'
-- own identical "join sessions for the JSONB, resolve owner/repo in Go"
-- shape (sandboxes.sql), the repo-demotion sweep's established
-- convention for this exact problem: there is no reliable SQL-side way
-- to compare a session's own repo clone URL against a bare "owner/repo"
-- string (reposource.ParseOwnerRepo's own host-agnostic path parsing has
-- no SQL equivalent here).
--
-- Both an in-flight row (status='pending' -- §30.8's own "unhandled
-- shadow-era row" internal/app/shadowoperator's own Activate refuses
-- promotion on) and a ledger-terminal one (status='delivered',
-- delivered_to_ledger=true -- the actual ledger entry) are returned
-- undifferentiated; the caller buckets by (status, delivered_to_ledger)
-- rather than this query encoding two separate reads over what is, at
-- the row level, one repo-scoped concern. A repo-less row (session_id
-- NULL -- a digest, a release manifest) is excluded by the INNER JOIN
-- itself: a suppressed effect with no identifiable repo cannot be shown
-- on, or gate, a repo-scoped surface.
--
-- Newest first, LIMIT $1 -- like CountSuppressedRepos' own doc comment
-- above, a floor for a deployment large enough to ever reach it, which a
-- dedicated shadow-evaluation deployment (§30.8) is not expected to be.
SELECT o.id AS id,
       o.session_id AS session_id,
       o.kind AS kind,
       o.status AS status,
       o.delivered_to_ledger AS delivered_to_ledger,
       o.created_at AS created_at,
       s.repos AS repos
FROM outbox o
JOIN sessions s ON s.id = o.session_id
WHERE o.suppressed_in_shadow = true
  -- A row that is DELIVERED but was never diverted to the ledger is a
  -- PASS-THROUGH kind that really executed. It carries the shadow stamp
  -- because every row does, regardless of kind -- but showing it as a
  -- suppressed effect tells the operator the opposite of what happened,
  -- and for sentinel_auto_fix it also double-counts, since the suppressed
  -- lane writes its own shadow_scm_writes row that this read already
  -- unions in.
  AND NOT (o.status = 'delivered' AND NOT o.delivered_to_ledger)
ORDER BY o.created_at DESC
LIMIT $1;

-- name: ListShadowSuppressedOutboxUnsettled :many
-- Every shadow-stamped outbox row that has NOT settled -- the set §30.8's
-- promotion quarantine actually asks about, and deliberately WITHOUT a
-- limit.
--
-- Its sibling above carries one for display, and that limit is applied
-- across the whole deployment BEFORE the caller filters to a repository
-- in Go (repo matching needs each session's repos JSONB parsed into
-- owner/repo, which SQL here cannot do). For a display list that only
-- truncates what is shown. For the Activate gate it would fail OPEN: a
-- repository's own unsettled row, older than the newest N rows anywhere
-- in the deployment, becomes invisible and promotion proceeds as if
-- nothing were outstanding.
--
-- Unbounded is safe here precisely because of what this selects. A
-- settled row -- delivered, whatever it was delivered to, or dead-lettered
-- -- is excluded, so this returns only work still genuinely in flight
-- plus anything that failed permanently. On a healthy deployment that is
-- a handful of rows; on an unhealthy one, the count is the thing the
-- operator needs to see in full.
SELECT o.id AS id,
       o.session_id AS session_id,
       o.kind AS kind,
       o.status AS status,
       o.delivered_to_ledger AS delivered_to_ledger,
       o.created_at AS created_at,
       s.repos AS repos
FROM outbox o
JOIN sessions s ON s.id = o.session_id
WHERE o.suppressed_in_shadow = true
  AND o.status <> 'delivered'
ORDER BY o.created_at DESC;
