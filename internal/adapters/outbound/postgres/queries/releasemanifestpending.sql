-- Queries backing internal/adapters/outbound/postgres.
-- ReleaseManifestPendingStore (blocking-finding fix #1, "release PR
-- review", §15.2) -- see migrations/000050_release_manifest_pending.up.sql's
-- own doc comment for the table's full design and the "why" behind this
-- fix.

-- name: CreateReleaseManifestPending :one
-- Written by internal/adapters/inbound/github's own webhook handler,
-- inline, immediately before its own fast ack -- deliberately as cheap as
-- a single INSERT (see the table's own doc comment for why the ACTUAL
-- check work never runs on this same request path).
INSERT INTO release_manifest_pending (session_id, owner, repo, pr_number, base_ref, head_ref, correlation_id)
VALUES ($1, $2, $3, $4, $5, $6, $7)
RETURNING *;

-- name: ClaimDueReleaseManifestPending :many
-- internal/app/releasereview.Worker's own poll query: atomically claims
-- up to $1 not-yet-claimed rows, oldest-first, by stamping claimed_at with
-- the database's now() in the SAME statement a SELECT ... FOR UPDATE SKIP
-- LOCKED subquery identifies -- a single round trip, no separate explicit
-- transaction required. Claiming a row this way IS this table's one and
-- only "attempt" -- a claimed row is never selected again, so there is no
-- revisit, no retry, no backoff (see the table's own doc comment for why
-- releasereview.Run itself has no failure signal to record one against).
-- The row stays, claimed, while its check runs -- the session's status
-- reads it as work that can still create a turn (technical plan §43.20,
-- migrations/000147) -- and the worker deletes it once the check returns
-- (FinishReleaseManifestPending). FOR UPDATE SKIP LOCKED is what lets two
-- concurrent pods' own Worker.PumpOnce calls each claim a DISJOINT row
-- instead of blocking on (or double-claiming) the same one -- mirrors
-- ListDuePendingOutboxEntries' own identical multi-pod reasoning
-- (queries/outbox.sql).
UPDATE release_manifest_pending
SET claimed_at = now()
WHERE id IN (
    SELECT id FROM release_manifest_pending
    WHERE claimed_at IS NULL
    ORDER BY created_at
    LIMIT $1
    FOR UPDATE SKIP LOCKED
)
RETURNING *;

-- name: FinishReleaseManifestPending :exec
-- The worker's own last step for one claimed row: its check has returned,
-- and any composition review turn it inserted has already committed, so
-- the row -- which the session's status reads as work still able to
-- create a turn -- goes now. A session whose status no longer counts this
-- row therefore already counts that turn.
DELETE FROM release_manifest_pending WHERE id = $1;

-- name: PurgeStaleClaimedReleaseManifestPending :execrows
-- Deletes rows claimed longer ago than max_age: their one attempt is over
-- -- ReleaseManifestCheckTimeout bounds it -- but their worker died before
-- FinishReleaseManifestPending ran. The session's status has already
-- stopped counting such a row (the same bound, measured on the same
-- database clock); this keeps them from piling up. Never touches an
-- unclaimed row.
DELETE FROM release_manifest_pending
WHERE claimed_at < now() - sqlc.arg('max_age')::interval;
