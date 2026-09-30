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
