-- Queries backing SentinelFixStore ("sentinels + suggestions",
-- §17) -- see migrations/000047_sentinel_fixes.up.sql's own doc comment
-- for the full table design and its two-step claim idiom.

-- name: InsertSentinelFixIfAbsent :execrows
-- Step ONE of the atomic claim (mirrors github_pr_sessions' own identical
-- two-step sequencing, migrations/000028's own doc comment): ON CONFLICT
-- DO NOTHING ensures a row exists for (repo_full_name, origin_pr_number)
-- -- callers check RowsAffected (1 = this call created the row and is
-- therefore the genuine first claimant; 0 = a row already existed, from
-- an earlier qualifying finding on the SAME PR) purely for logging/metrics,
-- never as the actual claim decision -- GetSentinelFixForUpdate below,
-- run in the SAME transaction immediately after, is what the caller
-- actually branches on (whether fix_child_session_id is already set).
INSERT INTO sentinel_fixes (repo_full_name, origin_pr_number, origin_review_session_id, origin_head_branch)
VALUES ($1, $2, $3, $4)
ON CONFLICT (repo_full_name, origin_pr_number) DO NOTHING;

-- name: GetSentinelFixForUpdate :one
-- Step TWO of the atomic claim: locks and reads the row Step one just
-- ensured exists -- serializes any concurrent claimant for the SAME PR
-- behind it, mirroring github_pr_sessions' own identical "SELECT ... FOR
-- UPDATE" locking precedent.
SELECT * FROM sentinel_fixes
WHERE repo_full_name = $1 AND origin_pr_number = $2
FOR UPDATE;

-- name: GetSentinelFixByID :one
-- The outbox worker's own idempotency check (internal/app/outboxworker's
-- sentinelAutoFixNotifier): the outbox payload carries this row's own id
-- (captured at claim time, reviewverdict.go), so a redelivered/retried
-- outbox entry can cheaply check "has a child session already been
-- spawned for this claim" without a second (repo, pr_number) round trip.
SELECT * FROM sentinel_fixes WHERE id = $1;

-- name: GetSentinelFix :one
-- A plain, unlocked read -- the merge-gating webhook's own lookup
-- (§17.4), which does its own locking via GetSentinelFixForUpdate only
-- once it already knows a row exists and is about to mutate it.
SELECT * FROM sentinel_fixes
WHERE repo_full_name = $1 AND origin_pr_number = $2;

-- name: GetSentinelFixByFixSession :one
-- The reverse lookup pushpr.go's own createSentinelFixPRBestEffort needs,
-- once the FIX session's own push_complete event arrives (that code path
-- only ever has its OWN session id in hand, never the origin PR's number
-- directly).
SELECT * FROM sentinel_fixes
WHERE fix_child_session_id = $1;

-- name: UpdateSentinelFixChildSession :one
-- Set once the outbox worker has spawned the child session (§17.2) --
-- status moves from 'pending' to 'spawned'.
UPDATE sentinel_fixes
SET fix_child_session_id = $2, status = 'spawned', updated_at = now()
WHERE id = $1
RETURNING *;

-- name: UpdateSentinelFixOpened :one
-- Set once pushpr.go's own createSentinelFixPRBestEffort has actually
-- opened the fix PR -- status moves from 'spawned' to 'fix_open'.
UPDATE sentinel_fixes
SET fix_pr_number = $2, status = 'fix_open', updated_at = now()
WHERE id = $1
RETURNING *;

-- name: UpdateSentinelFixStackRegistered :one
-- Set (best-effort, §17.2/§17.6: "logged and otherwise ignored" on any
-- failure) after the POST /repos/{owner}/{repo}/stacks call -- an
-- observability-only column, never the authority on whether registration
-- actually stuck (that authority is always a fresh GetPullRequest.Stack
-- field, per §17.6 -- see this table's own migration doc comment).
UPDATE sentinel_fixes
SET stack_registered = $2, updated_at = now()
WHERE id = $1
RETURNING *;

-- name: MarkSentinelFixMerged :one
-- Merge-gating's own terminal write (§17.4) -- status moves to
-- 'fix_merged' once all four checks passed and the fix PR actually
-- merged.
UPDATE sentinel_fixes
SET status = 'fix_merged', updated_at = now()
WHERE id = $1
RETURNING *;

-- name: MarkSentinelFixAbandoned :one
-- Set when the origin PR itself is closed WITHOUT merging (§17.5: "the
-- fix PR is simply left open as an ordinary review item -- never silently
-- discarded") -- this row's own terminal-but-not-merged state, purely for
-- observability; the fix PR itself is never touched by this transition.
UPDATE sentinel_fixes
SET status = 'abandoned', updated_at = now()
WHERE id = $1
RETURNING *;

-- name: ReleaseUnspawnedSentinelFix :execrows
-- The sentinel auto-fix delivery's own release of a claim it skips
-- because the repository turned the auto-fix off (§17.2): deletes the row
-- only while it is still 'pending' with no child session recorded, so a
-- claim a concurrent delivery spawned under is never released -- that
-- delivery holds GetSentinelFixForUpdate's lock until it commits, and
-- this DELETE re-checks the row once the lock is released. Releasing the
-- claim, rather than leaving it 'pending' with nothing coming, keeps the
-- review readout from showing a fix in flight that never will be; the
-- next qualifying verdict claims afresh (InsertSentinelFixIfAbsent).
DELETE FROM sentinel_fixes
WHERE id = $1 AND status = 'pending' AND fix_child_session_id IS NULL;

-- name: ExistsSentinelFixByFixPRNumber :one
-- §16 ("decision inbox: read model + API")'s own §17 structural
-- exclusion: "sentinel auto-fix follow-up PRs must never appear as inbox
-- rows... Make this a structural exclusion, not a filter someone can
-- forget." A PR is a sentinel-auto-fix follow-up iff it appears as SOME
-- row's own fix_pr_number for this repo -- deliberately never inferred
-- from sessions.parent_session_id/spawn_depth alone (migrations/
-- 000045_sessions_child_sessions.up.sql's own doc comment: those two
-- columns are generic child-session markers shared by ANY future child-
-- session mechanism -- handoff v2, workflow HITL -- not sentinel-fix-
-- specific; over-matching on them would over-exclude an unrelated future
-- child-session's own PR). fix_pr_number is nullable (NULL until the fix
-- session's own PR actually opens, §17.2) so this naturally reports false
-- for a claim row still 'pending'/'spawned'.
SELECT EXISTS (
    SELECT 1 FROM sentinel_fixes
    WHERE repo_full_name = sqlc.arg('repo_full_name') AND fix_pr_number = sqlc.arg('fix_pr_number')
) AS exists;
