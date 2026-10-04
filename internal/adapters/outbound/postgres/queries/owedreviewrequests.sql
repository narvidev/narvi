-- Queries backing OwedReviewRequestStore: technical plan §24.9's owed
-- human review requests (migrations/000160). A person's review attempt
-- (the label, the web button, a mention) that waited behind another turn
-- and found its pull request moved past the context it recorded ends
-- context_moved, and its request becomes a row here, owed to its
-- requester; the session actor's owed_review_request timer consumes the
-- rows oldest first. Every statement is scoped to one session and served
-- by owed_review_requests_session_id_idx (session_id, created_at), or by
-- the primary key.

-- name: InsertOwedReviewRequest :one
-- The dispatching transaction that ends a person's review attempt
-- context_moved owes its request (sessionactor's oweReviewRequest), in the
-- same transaction as that end and the arm of the owed_review_request
-- timer. created_at is the database's now: the instant a person's stop
-- compares with.
INSERT INTO owed_review_requests (session_id, requested_by, trigger, request_text, is_review_attempt, context_moves, moved_turn_id)
VALUES ($1, $2, $3, $4, $5, $6, $7)
RETURNING *;

-- name: GetOldestOwedReviewRequest :one
-- The session's oldest owed request, the one the owed_review_request
-- timer's consumer serves next. id breaks a created_at tie, so the pick is
-- reproducible. pgx.ErrNoRows: nothing is owed.
SELECT * FROM owed_review_requests
WHERE session_id = sqlc.arg('session_id')
ORDER BY created_at, id
LIMIT 1;

-- name: DeleteOwedReviewRequest :one
-- The consumer's delete of the request it serves, in the transaction that
-- inserts its re-run turn or drops it. pgx.ErrNoRows when the row is
-- already gone -- a person's stop dropped it while the consumer read the
-- pull request -- and then nothing is re-run.
DELETE FROM owed_review_requests
WHERE id = sqlc.arg('id')
RETURNING *;

-- name: ExistsOwedReviewRequest :one
-- Whether the session is still owed a request: the consumer re-arms its
-- timer due at once when it is, and deletes it when it is not.
SELECT EXISTS (
    SELECT 1 FROM owed_review_requests o
    WHERE o.session_id = sqlc.arg('session_id')
) AS owed;

-- name: DeleteOwedReviewRequestsForStop :many
-- A person's stop drops every request owed when it was made: the rows
-- created at or before the session's standing stop request, the rule the
-- stop timer deletes the session's work-creating timers by
-- (sessionactor's disarmWorkCreatingTimers), compared with the same
-- database clock. A request owed after the stop was made by a turn created
-- after it, which the stop did not ask to drop.
DELETE FROM owed_review_requests
WHERE session_id = sqlc.arg('session_id')
  AND created_at <= sqlc.arg('stop_requested_at')
RETURNING *;
