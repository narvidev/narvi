-- Queries backing HandoffSentinelStore ("handoff-readiness
-- sentinel", §14.4) -- see migrations/000049_handoff_sentinel_runs.up.sql's
-- own doc comment for the full table design and its single-step claim
-- idiom.

-- name: ClaimHandoffSentinelRun :one
-- Atomic first-writer-wins claim on (repo_full_name, pr_number): ON
-- CONFLICT DO NOTHING means a caller that loses the race gets ZERO rows
-- back (a :one query with no matching row surfaces pgx.ErrNoRows,
-- unwrapped, to the caller -- HandoffSentinelStore.Claim's own doc
-- comment) -- simpler than sentinel_fixes'/github_pr_sessions' own
-- two-step InsertIfAbsent+GetForUpdate idiom because this caller never
-- needs the ALREADY-CLAIMED row's own data back, only a yes/no answer
-- (§17.1's own claim needs the existing row's fix_child_session_id;
-- this one does not have an equivalent follow-on read).
--
-- contract_drift_flagged/todo_count (migrations/
-- 000123_handoff_sentinel_runs_summary.up.sql, §12.2 item 2's own
-- "handoff-readiness display" gap) are the SAME contractDrifted/
-- len(todos) values the caller (runHandoffSentinelBestEffort) already
-- computed to decide whether to claim at all -- persisted here, in this
-- SAME insert, rather than a second write.
INSERT INTO handoff_sentinel_runs (repo_full_name, pr_number, session_id, contract_drift_flagged, todo_count)
VALUES ($1, $2, $3, $4, $5)
ON CONFLICT (repo_full_name, pr_number) DO NOTHING
RETURNING id;

-- name: GetHandoffSentinelRun :one
-- §12.2 item 2's own "handoff-readiness display" gap: the code-review
-- readout's own read of this PR's handoff-sentinel claim, if any.
-- pgx.ErrNoRows (unwrapped) means the handoff-readiness sentinel never
-- flagged anything for this PR -- including every PR that was never a
-- scoped-session prototype in the first place.
SELECT * FROM handoff_sentinel_runs
WHERE repo_full_name = $1 AND pr_number = $2;
