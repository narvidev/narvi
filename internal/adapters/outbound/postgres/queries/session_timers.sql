-- Queries backing TimerScheduler (§4.3, §2). UpsertSessionTimer uses
-- ON CONFLICT (session_id, name) DO UPDATE per the "each is armed/re-armed
-- independently" semantics (§2) — re-arming updates the existing row, never
-- inserts a duplicate.

-- name: UpsertSessionTimer :one
-- Every arm and re-arm stamps armed_at with now() (migration 000153): the
-- instant a binary that knows the kind last maintained the row, from which
-- a kind the session actor does not know is aged (GetSessionTimerAge). The
-- insert takes it from the column's default.
INSERT INTO session_timers (session_id, name, fires_at)
VALUES ($1, $2, $3)
ON CONFLICT (session_id, name) DO UPDATE
    SET fires_at = EXCLUDED.fires_at, armed_at = now()
RETURNING *;

-- name: ArmSessionDispatchTimer :exec
-- The session's dispatch timer (technical plan §2, §3.3), armed due at once
-- on the database's clock, in the transaction that creates a turn: the only
-- caller is TurnStore.CreateAndArmDispatch, which inserts the turn in the
-- same transaction. 'dispatch' is sessionactor.TimerDispatch; the kind is
-- named here rather than passed in, so no caller can arm another kind this
-- way. A re-arm moves fires_at back to now even while the pump holds the
-- row claimed, and stamps armed_at like every arm (UpsertSessionTimer).
INSERT INTO session_timers (session_id, name, fires_at)
VALUES ($1, 'dispatch', now())
ON CONFLICT (session_id, name) DO UPDATE
    SET fires_at = now(), armed_at = now();

-- name: WakeReviewRetriggerDebounce :execrows
-- Technical plan §24.9: the wake-up a turn's end owes the re-review
-- debounce, run by the session actor in the same transaction as the write
-- that left a turn completed, failed or cancelled (sessionactor's
-- turnWriter and transact). While a turn of the review session is open the
-- debounce holds, re-arming itself at ReviewRetriggerHoldBackstop; this
-- moves it to now, so the review the hold kept back runs, for the head
-- pushed last, as soon as the session is free. 'review_retrigger_debounce'
-- is sessionactor.TimerReviewRetriggerDebounce, named here like the kind
-- ArmSessionDispatchTimer names, so no caller can wake another kind.
--
-- An UPDATE, never an insert: a debounce row exists while a push still
-- waits for its review (the hold re-arms it), and one that is gone was
-- removed on purpose -- an enqueue that took its head, an opt-in switched
-- off or a revocation, or a person's stop -- which an insert would undo.
-- A debounce armed at or before the session's standing stop request is
-- left alone (sessionactor's disarmWorkCreatingTimers deletes it when the
-- stop timer runs, by the same rule), so a turn the stop's dispatch gate
-- cancels never wakes the work the stop asked to drop. armed_at is stamped
-- like every arm (UpsertSessionTimer); created_at, the instant a stop is
-- compared with, is kept. A session with no debounce updates nothing: one
-- probe of the (session_id, name) unique index.
UPDATE session_timers AS st
SET fires_at = now(), armed_at = now()
FROM sessions AS s
WHERE st.session_id = sqlc.arg('session_id')
  AND st.name = 'review_retrigger_debounce'
  AND s.id = st.session_id
  AND (s.stop_requested_at IS NULL OR st.created_at > s.stop_requested_at);

-- name: BackOffSessionDispatchTimer :execrows
-- The session actor's backoff of its dispatch timer (sessionactor's
-- backOffDispatchTimer and failDispatchedTurn, technical plan §2): after a
-- dispatch evaluation that failed, or one that sent a prompt the sandbox
-- never received. fires_at moves to now plus the row's age since the first
-- arm of its chain of failures, held between the two bounds, so each
-- failure doubles the delay until the bound. That first arm is created_at,
-- which a re-arm never moves, or since when it is earlier: a failed
-- delivery ends its turn, and a workflow step it re-queues arms a new row
-- whose created_at would restart the chain, so since carries the first arm
-- of the row the evaluation deleted, and created_at takes it. armed_at is
-- not moved: a backoff is not an arm. A session with no dispatch timer is
-- left without one (zero rows).
--
-- It holds only while the row still carries armed_at: after a failed
-- evaluation, the armed_at that evaluation read when it deleted the timer
-- (DeleteSessionDispatchTimer) -- a turn created since re-armed it due at
-- once and moved armed_at, and that re-arm wins (zero rows).
UPDATE session_timers
SET created_at = LEAST(created_at, COALESCE(sqlc.narg('since')::timestamptz, created_at)),
    fires_at = now() + LEAST(
        GREATEST(now() - LEAST(created_at, COALESCE(sqlc.narg('since')::timestamptz, created_at)),
                 make_interval(secs => sqlc.arg('base_seconds')::float8)),
        make_interval(secs => sqlc.arg('max_seconds')::float8))
WHERE session_id = sqlc.arg('session_id') AND name = 'dispatch'
  AND armed_at = sqlc.arg('armed_at');

-- name: DeleteSessionDispatchTimer :one
-- A dispatch evaluation's first write (sessionactor's planDispatch,
-- technical plan §2): the session's dispatch timer is deleted, and its
-- armed_at and created_at returned, so an evaluation that fails -- its
-- transaction rolls the delete back -- backs off only the row it read, and
-- one whose prompt is never delivered carries the row's first arm on to
-- the next (BackOffSessionDispatchTimer). pgx.ErrNoRows when the session
-- has none.
DELETE FROM session_timers
WHERE session_id = $1 AND name = 'dispatch'
RETURNING armed_at, created_at;

-- name: GetSessionTimer :one
SELECT * FROM session_timers
WHERE session_id = $1 AND name = $2;

-- name: GetSessionTimerAge :one
-- A timer's armed_at beside the database's now(), in one statement, so the
-- age of a kind the session actor does not know
-- (sessionactor.DecideUnknownTimer) is the time since its last arm,
-- measured on the database's clock alone: every arm and re-arm
-- (UpsertSessionTimer) sets armed_at to the database's now(), and neither
-- the claim (ClaimDueTimer) nor the backoff (PostponeSessionTimerIfArmedAt)
-- moves it.
SELECT armed_at, now()::timestamptz AS db_now
FROM session_timers
WHERE session_id = $1 AND name = $2;

-- name: PostponeSessionTimerIfArmedAt :execrows
-- The session actor's backoff of a timer kind it does not know: moves
-- fires_at, never armed_at, and only while the row still carries the
-- armed_at the decision read (GetSessionTimerAge). A row re-armed since --
-- by a newer replica that knows the kind -- is left alone: zero rows.
UPDATE session_timers
SET fires_at = $1
WHERE session_id = $2 AND name = $3 AND armed_at = $4;

-- name: DeleteSessionTimerIfArmedAt :execrows
-- The session actor's deletion of a timer kind it does not know, only
-- while the row still carries the armed_at the decision read: a row
-- re-armed since is left alone (zero rows).
DELETE FROM session_timers
WHERE session_id = $1 AND name = $2 AND armed_at = $3;

-- name: ListDueTimers :many
-- The timer pump's poll query (§2, explicit): FOR UPDATE SKIP LOCKED so
-- multiple concurrent pump ticks (this pod or another) never select the
-- same due row.
SELECT * FROM session_timers
WHERE fires_at <= now()
ORDER BY fires_at
LIMIT $1
FOR UPDATE SKIP LOCKED;

-- name: ClaimDueTimer :one
-- Pushes an already-locked (via ListDueTimers, same transaction) timer's
-- fires_at forward by the pump's claim duration -- never armed_at: a claim
-- is not an arm, and a kind no binary maintains must keep ageing through
-- every claim of it (GetSessionTimerAge) -- so a second
-- concurrent/later pump tick won't re-select the same row as due again
-- until the claim window elapses -- the redelivery-safety mechanism (§2):
-- claiming before delivering means a crash after claiming but before the
-- actor finishes handling it self-heals once the claim window passes,
-- with no permanent loss.
UPDATE session_timers
SET fires_at = $1
WHERE session_id = $2 AND name = $3
RETURNING *;

-- name: DeleteSessionTimer :exec
DELETE FROM session_timers
WHERE session_id = $1 AND name = $2;

-- name: ListSessionTimers :many
-- Every timer armed on one session: the stop timer's handler deletes the
-- ones whose firing creates a turn (sessionactor.ClassifyTimer).
SELECT * FROM session_timers
WHERE session_id = $1
ORDER BY name;
