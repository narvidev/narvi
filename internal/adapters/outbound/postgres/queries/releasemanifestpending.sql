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
-- up to $1 rows, oldest-first, by DELETING them in the SAME statement a
-- SELECT ... FOR UPDATE SKIP LOCKED subquery identifies -- a single round
-- trip, no separate explicit transaction required (a plain DELETE is
-- already atomic). Claiming a row this way IS this table's one and only
-- "attempt" -- there is no revisit, no retry, no backoff (see the
-- table's own doc comment for why releasereview.Run itself has no
-- failure signal to record one against). FOR UPDATE SKIP LOCKED still
-- matters even though this is a DELETE, not an in-place claim UPDATE: it
-- is what lets two concurrent pods' own Worker.PumpOnce calls each claim
-- a DISJOINT batch instead of blocking on (or double-claiming) the same
-- row -- mirrors ListDuePendingOutboxEntries' own identical multi-pod
-- reasoning (queries/outbox.sql).
DELETE FROM release_manifest_pending
WHERE id IN (
    SELECT id FROM release_manifest_pending
    ORDER BY created_at
    LIMIT $1
    FOR UPDATE SKIP LOCKED
)
RETURNING *;

-- name: StartReleaseManifestCheck :exec
-- Technical plan §43.20 (migrations/000146_release_manifest_checks_running
-- .up.sql): records that the check claimed from pending row pending_id is
-- now running on session_id, stamped with the database's now(). Called by
-- ReleaseManifestPendingStore.ClaimDue in the SAME transaction as
-- ClaimDueReleaseManifestPending's delete, so every snapshot sees a
-- claimed check either waiting or running until it finishes.
INSERT INTO release_manifest_checks_running (pending_id, session_id)
VALUES ($1, $2);

-- name: FinishReleaseManifestCheck :exec
-- The worker's own last step for one claimed check: it has returned, and
-- any composition review turn it inserted has already committed, so the
-- running row -- which the session's status reads as work still able to
-- create a turn -- goes now. A session whose status no longer counts the
-- check therefore already counts that turn.
DELETE FROM release_manifest_checks_running WHERE pending_id = $1;

-- name: PurgeStaleReleaseManifestChecks :execrows
-- Deletes running rows claimed longer ago than max_age: their one attempt
-- is over -- ReleaseManifestCheckTimeout bounds it -- but their worker died
-- before FinishReleaseManifestCheck ran. The session's status has already
-- stopped counting such a row (the same bound, measured on the same
-- database clock); this keeps them from piling up. Never touches a
-- release_manifest_pending row: those are only ever claimed.
DELETE FROM release_manifest_checks_running
WHERE claimed_at < now() - sqlc.arg('max_age')::interval;
