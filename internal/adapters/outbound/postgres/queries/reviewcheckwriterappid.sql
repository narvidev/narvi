-- Queries backing ReviewCheckRunStore's own writer-app-id persistence
-- (finding B2). See migrations/000134_review_check_writer_app_id.up.sql's
-- own doc comment for the full "why a Postgres-owned singleton, not only
-- an in-process cache" reasoning.

-- name: GetReviewCheckWriterAppID :one
-- The single row's own app_id, if this deployment has ever observed one
-- (from ANY successful CreateCheckRun, on ANY pull request, at ANY point
-- since this table started being written). pgx.ErrNoRows means "never
-- observed" -- the caller degrades exactly like observedWriterAppID's
-- own in-process 0 already does: never adopt, always create.
SELECT app_id FROM review_check_writer_app_id WHERE id = 1;

-- name: UpsertReviewCheckWriterAppID :exec
-- Records id's own newly-observed app_id -- called immediately after
-- EVERY successful CreateCheckRun response, mirroring recordWriterAppID's
-- own in-process "cache the real value GitHub itself reports" contract,
-- now durable. A concurrent writer overwriting this SAME row with the
-- SAME, or another, genuinely-observed app_id is never a conflict worth
-- guarding against (this deployment's bot credential has exactly one
-- true App id at a time; two different observed values would mean the
-- credential itself changed, not a race to resolve).
INSERT INTO review_check_writer_app_id (id, app_id, updated_at)
VALUES (1, $1, now())
ON CONFLICT (id) DO UPDATE SET app_id = EXCLUDED.app_id, updated_at = now();
