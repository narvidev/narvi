-- Queries backing TimerScheduler (§4.3, §2). UpsertSessionTimer uses
-- ON CONFLICT (session_id, name) DO UPDATE per the "each is armed/re-armed
-- independently" semantics (§2) — re-arming updates the existing row, never
-- inserts a duplicate.

-- name: UpsertSessionTimer :one
INSERT INTO session_timers (session_id, name, fires_at)
VALUES ($1, $2, $3)
ON CONFLICT (session_id, name) DO UPDATE
    SET fires_at = EXCLUDED.fires_at
RETURNING *;

-- name: GetSessionTimer :one
SELECT * FROM session_timers
WHERE session_id = $1 AND name = $2;

-- name: GetSessionTimerAge :one
-- A timer's created_at beside the database's now(), in one statement, so
-- the age of a kind the session actor does not know
-- (sessionactor.DecideUnknownTimer) is measured on the database's clock
-- alone: created_at is the database's now() at insert, and neither a
-- re-arm (UpsertSessionTimer) nor a claim (ClaimDueTimer) moves it.
SELECT created_at, now()::timestamptz AS db_now
FROM session_timers
WHERE session_id = $1 AND name = $2;

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
-- fires_at forward by the pump's claim duration, so a second
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
