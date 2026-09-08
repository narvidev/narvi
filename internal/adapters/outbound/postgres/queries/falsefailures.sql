-- Queries backing FalseFailureStore -- see migrations/
-- 000124_false_failures.up.sql's own doc comment for the table's full
-- design (one row per detected §3.2 "watchdog kill later proven alive"
-- incident, deliberately never deduplicated beyond the call site's own
-- `inserted` gate).

-- name: InsertFalseFailure :one
-- Called from internal/app/sessionactor's recordFalseFailureIfApplicable
-- (pushpr.go), inside the SAME transaction as the rest of that
-- completeProcessingTurn call -- the durable sibling of that function's
-- own turn_false_failure_total OTel increment (opsmetrics.go), which
-- stays unchanged and is still incremented from the same call site.
INSERT INTO false_failures (session_id) VALUES ($1)
RETURNING *;

-- name: CountFalseFailuresInWindow :one
-- §12.2 item 6's own "False failures" KPI tile (target 0). A
-- straight COUNT is unbounded-safe by construction (its own result is
-- always exactly one small integer, regardless of how many rows the
-- window contains) and always well-defined -- unlike a percentile or a
-- rate, 0 here is never ambiguous between "unknown" and "measured zero":
-- an incident either happened in the window or it did not, so this tile
-- carries no separate "not yet computed" sentinel (internal/app/
-- platformanalytics's own doc comment explains why, alongside the
-- "Sessions" tile's identical reasoning).
--
-- Bounded by false_failures_detected_at_idx (migrations/
-- 000124_false_failures.up.sql).
SELECT COUNT(*)::bigint AS false_failure_count
FROM false_failures
WHERE detected_at >= $1;
