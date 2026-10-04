-- Queries backing GitHubPRSessionStore (§8.2's "atomic claim coalescing of
-- concurrent @mentions" "GitHub ingress"). See
-- migrations/000028_github_pr_sessions.up.sql's own doc comment for the
-- full two-step claim design (ensure the row exists via ON CONFLICT, then
-- lock + branch via FOR UPDATE) these three queries implement together.

-- name: EnsureGitHubPRSessionRow :exec
-- Idempotently ensures a (repo_full_name, pr_number) claim row exists,
-- with session_id left NULL on a fresh insert -- the SAME
-- "INSERT ... ON CONFLICT" atomic-claim idiom ClaimWebhookDelivery
-- (§5.1) already establishes. DO NOTHING here since this step's only job is
-- "make sure the row is there to lock next" -- the following
-- LockGitHubPRSessionForUpdate call, in the SAME transaction, is what
-- actually determines who won.
INSERT INTO github_pr_sessions (repo_full_name, pr_number)
VALUES ($1, $2)
ON CONFLICT (repo_full_name, pr_number) DO NOTHING;

-- name: LockGitHubPRSessionForUpdate :one
-- Locks the (repo_full_name, pr_number) row for the remainder of the
-- caller's own transaction -- any concurrent caller's own call to this
-- SAME query for the SAME PR blocks here until the first caller commits
-- or rolls back, mirroring internal/adapters/inbound/httpapi/turn.go's
-- own GetActorEpochForUpdate row-lock precedent exactly. The caller must
-- have already run EnsureGitHubPRSessionRow (in the SAME transaction) so
-- this row is guaranteed to exist by the time this runs.
SELECT session_id FROM github_pr_sessions
WHERE repo_full_name = $1 AND pr_number = $2
FOR UPDATE;

-- name: SetGitHubPRSessionID :exec
-- Fills in session_id for a (repo_full_name, pr_number) claim row while
-- still holding LockGitHubPRSessionForUpdate's own row lock -- called
-- exactly once, by whichever caller observed session_id NULL under that
-- lock (the genuine first mention on this PR), immediately after it
-- creates the real session, still inside the SAME transaction as both the
-- lock and the session insert.
UPDATE github_pr_sessions
SET session_id = $3
WHERE repo_full_name = $1 AND pr_number = $2;

-- name: GetGitHubPRSessionBySessionID :one
-- The REVERSE lookup §5.1 ("outbox delivery") needs: given a
-- session_id, which (repo_full_name, pr_number) PR does it back? Backed
-- by migrations/000032_github_pr_sessions_session_id_idx.up.sql's own new
-- index (this table had no session_id index before that Step, since §8.2's
-- own ingress path only ever needed the FORWARD direction, its own
-- primary key). A pgx.ErrNoRows result means this session was never
-- created via a GitHub PR mention -- the caller skips enqueuing a GitHub
-- notification entirely rather than fabricating one.
SELECT * FROM github_pr_sessions
WHERE session_id = $1;

-- name: GetGitHubPRSessionByRepoAndPRNumber :one
-- The FORWARD, non-locking, non-claiming read this table's own primary
-- key already supports for free but which, before the decision inbox's
-- own "open review" action needed it, had no plain SELECT of its own --
-- every existing forward-direction query (EnsureGitHubPRSessionRow/
-- LockGitHubPRSessionForUpdate/SetGitHubPRSessionID above) is part of
-- §8.2's own atomic first-mention CLAIM sequence and must run inside that
-- one transaction, with LockGitHubPRSessionForUpdate's own FOR UPDATE
-- serializing every concurrent claimant for the same PR behind it. A
-- caller that only wants to READ "does a review session already exist
-- for this PR, and if so which one" -- decisioninbox.buildPROpenItem,
-- deciding whether a PR-shaped row can link to Narvi's own review readout
-- instead of GitHub -- has no claim to make and must never take that
-- lock itself (it would needlessly serialize behind, or itself block, a
-- real concurrent @mention claim for the same PR). pgx.ErrNoRows means
-- exactly what it means at GetGitHubPRSessionBySessionID above: Narvi has
-- never been mentioned on this PR, so there is no review session to
-- link to -- a common, legitimate negative, never an error.
SELECT * FROM github_pr_sessions
WHERE repo_full_name = $1 AND pr_number = $2;

-- SetGitHubPRSessionHeadSHA (and pending_head_sha, migrations/000068) is
-- REMOVED as of migrations/000072_turns_review_head_sha.up.sql (§21
-- review finding C2, CRITICAL, fixed) -- superseded by turns.
-- review_head_sha, set once at turn-creation time and read back via
-- turns.GetByDispatchedMessageID (finding F3, §21.1's amendment,
-- migrations/000131_turns_dispatched_message_id.up.sql), never a shared
-- per-(repo,PR) column any later, unrelated turn's own context-fetch
-- could overwrite. See migration 000072's own doc comment for the full
-- "why".

-- Queries backing §24's ("review: automatic re-review on new
-- commits", §24) trailing-edge debounce + per-PR budget -- see
-- migrations/000075_github_pr_sessions_retrigger.up.sql's own doc comment
-- for the three columns below, including why pending_retrigger_head_sha
-- is a deliberately DIFFERENT name from the retired pending_head_sha.

-- name: UpsertPendingRetriggerHeadSHA :one
-- The synchronize webhook handler's own direct, actor-bypassing write
-- (§24.1's 4th cost item, internal/adapters/inbound/github/
-- pullrequestsynchronize.go) -- called in the SAME transaction as
-- UpsertSessionTimer's own re-arm (session_timers.sql), never
-- independently committed. Guarded on session_id IS NOT NULL (CLAUDE.md/
-- §11's own "guarded UPDATE ... WHERE for cross-writer transitions" idiom)
-- so this is a genuine no-op -- pgx.ErrNoRows, never a fabricated row --
-- for exactly the two cases §24.1 says must be acknowledged and ignored:
-- no github_pr_sessions row at all for this PR, or a row whose session_id
-- is still NULL (nobody has ever mentioned the bot on this PR) -- "no
-- session to re-trigger", identical in kind to today's "no mention"
-- no-op for comment events. Overwrites (never appends) on every event,
-- per §24.2's own "upserted, not appended" rule.
--
-- A push re-arms an automatic re-review that gave up on a moved context
-- (technical plan §24.9, DropAutoRetrigger below): the drop it recorded is
-- cleared here, so the status stops showing it the moment the next head
-- is owed a review. The move count is not touched: it counts automatic
-- attempts that met a moved context in a row, and only one starting
-- resets it (ResetAutoRetriggerContextMoves).
UPDATE github_pr_sessions
SET pending_retrigger_head_sha = $3,
    auto_retrigger_dropped_at = NULL,
    auto_retrigger_dropped_head_sha = NULL
WHERE repo_full_name = $1 AND pr_number = $2 AND session_id IS NOT NULL
RETURNING *;

-- name: ClearPendingRetriggerHeadSHA :one
-- The review_retrigger_debounce timer's own fire handler
-- (sessionactor.handleReviewRetriggerDebounceTimer) calls this after
-- EVERY firing that reaches a decision (§24.3 steps 3 and 4 alike: heads
-- already match, a turn was just enqueued, or the budget is exhausted) --
-- guarded on pending_retrigger_head_sha STILL equalling the exact value
-- ($3) this firing just read and acted on (a compare-and-swap, CLAUDE.md/
-- §11's own guarded-UPDATE idiom): a NEW synchronize event landing
-- between this firing's own read and this clear (a genuine, expected
-- race between the webhook handler and this timer's own claimed-but-
-- still-processing window) will already have overwritten this column
-- with a NEWER head sha and re-armed the timer fresh -- this guard
-- ensures THAT newer, still-unprocessed push is never silently clobbered
-- back to NULL by a decision made against a now-stale value. pgx.ErrNoRows
-- on a guard miss is the expected, harmless outcome in that race (nothing
-- to clear -- a fresher event already owns this row). Rereview fix
-- (finding 2, correcting this comment's own earlier false claim): the
-- caller must NOT then proceed to delete "its own claimed timer row" --
-- session_timers has UNIQUE(session_id, name), so there is exactly ONE
-- review_retrigger_debounce row for this session, and on a guard miss it
-- is already the SAME row the newer synchronize event's own re-arm just
-- updated. Deleting it here would strand that newer push with no timer
-- left to ever act on it -- the caller must skip its own delete on a
-- guard miss instead, trusting the newer event's own already-committed
-- re-arm to stand in for it (the same re-arm-or-delete contract every
-- named timer already follows, timerfired.go, satisfied by the newer
-- event's re-arm rather than by this firing's own delete).
UPDATE github_pr_sessions
SET pending_retrigger_head_sha = NULL
WHERE repo_full_name = $1 AND pr_number = $2 AND pending_retrigger_head_sha = $3
RETURNING *;

-- name: IncrementGitHubPRSessionMentionCount :one
-- §12.2 item 2's own "coalesced-mention/session-reuse info" gap
-- (migrations/000122_github_pr_sessions_mention_count.up.sql's own doc
-- comment): called from coalesce.go's own REUSE branch, AFTER that
-- transaction's own early commit -- audit fix, moved off
-- LockGitHubPRSessionForUpdate's own row lock deliberately (it used to run
-- while still holding it, before that same commit): the increment must
-- only land once the REUSE attempt has actually cleared authorization AND
-- produced a real turn, both of which happen post-commit (coalesce.go's
-- own CreateOrJoin top doc comment explains why that ownership-aware
-- authz check itself cannot run any earlier), so counting had to move
-- with it or a denied/failed attempt would inflate this column with no
-- corresponding turn. Still needs no application-level CAS guard despite
-- holding no lock at call time: this is a single UPDATE statement, and
-- Postgres holds the target row's lock for that ONE statement's own
-- duration regardless of any ambient transaction -- two concurrent REUSE
-- callers for the SAME PR simply apply in whichever order Postgres
-- schedules their two UPDATE statements, never losing either one. Compare
-- IncrementAutoRetriggerCount's own DIFFERENT "only ever reached under a
-- lock/single-writer" reasoning immediately below -- that column has a
-- single writer (this PR's own session actor) and needs neither this
-- reasoning nor that one to be safe.
UPDATE github_pr_sessions
SET mention_count = mention_count + 1
WHERE repo_full_name = $1 AND pr_number = $2
RETURNING *;

-- name: IncrementAutoRetriggerCount :one
-- §24.6's own budget counter -- incremented exactly once per PR each
-- time handleReviewRetriggerDebounceTimer actually enqueues an automatic
-- re-review turn (never for a manual label/button re-trigger, which is
-- never subject to this budget at all). A plain increment, not a guarded
-- one: only this PR's own session actor ever writes this column, so
-- there is no cross-writer race here to guard against (unlike
-- pending_retrigger_head_sha, which the webhook handler ALSO writes).
UPDATE github_pr_sessions
SET auto_retrigger_count = auto_retrigger_count + 1
WHERE repo_full_name = $1 AND pr_number = $2
RETURNING *;

-- name: RequeueAutoRetrigger :one
-- Technical plan §24.9: an automatic review attempt met a moved context at
-- its dispatch and ended context_moved without running, so the automatic
-- re-review asks again, in that dispatching transaction: the pull
-- request's pending head becomes head_sha -- the head the dispatch read
-- live -- unless a push already left one pending, which is as new and
-- stays (COALESCE), and the count of such moves in a row grows by one. The
-- caller (sessionactor's endContextMovedTurn) arms the debounce if it has
-- none, and drops the request past ReviewContextMoveMaxConsecutive
-- (DropAutoRetrigger). Only the pull request's own session actor moves the
-- count; the synchronize webhook writes the pending head too, and this
-- statement's row lock orders the two. RETURNING the row, its new count
-- among it. pgx.ErrNoRows: no row for this pull request, or one with no
-- session.
UPDATE github_pr_sessions
SET pending_retrigger_head_sha = COALESCE(pending_retrigger_head_sha, sqlc.arg('head_sha')::text),
    auto_retrigger_context_moves = auto_retrigger_context_moves + 1
WHERE repo_full_name = sqlc.arg('repo_full_name') AND pr_number = sqlc.arg('pr_number') AND session_id IS NOT NULL
RETURNING *;

-- name: DropAutoRetrigger :one
-- Technical plan §24.9's bound: the automatic re-review gives up after
-- ReviewContextMoveMaxConsecutive of its attempts in a row met a moved
-- context. In the transaction that recorded the last move
-- (RequeueAutoRetrigger, whose row lock it still holds), it clears the
-- pending head it just requeued -- only while the head is still that one,
-- the guarded clear ClearPendingRetriggerHeadSHA makes -- and records when
-- it gave up and the head it gave up on, which the session's status shows
-- (GetSessionActivityFacts) until a push clears them
-- (UpsertPendingRetriggerHeadSHA). The move count stays where it is: the
-- next automatic attempt that starts resets it. pgx.ErrNoRows: the pending
-- head is no longer head_sha.
UPDATE github_pr_sessions
SET pending_retrigger_head_sha = NULL,
    auto_retrigger_dropped_at = now(),
    auto_retrigger_dropped_head_sha = sqlc.arg('head_sha')::text
WHERE repo_full_name = sqlc.arg('repo_full_name') AND pr_number = sqlc.arg('pr_number')
  AND pending_retrigger_head_sha = sqlc.arg('head_sha')::text
RETURNING *;

-- name: ResetAutoRetriggerContextMoves :execrows
-- Technical plan §24.9: one of the automatic re-review's attempts started
-- -- its context fresh, unconfirmed, or never asked since it waited behind
-- no turn -- so the count of its attempts in a row that met a moved
-- context starts again, in the dispatching transaction. Only a row whose
-- count is not already 0 is written, so an ordinary dispatch writes
-- nothing; 0 rows then.
UPDATE github_pr_sessions
SET auto_retrigger_context_moves = 0
WHERE session_id = sqlc.arg('session_id') AND auto_retrigger_context_moves <> 0;

-- name: MarkAutoRetriggerBudgetNoticeSent :one
-- §24.6's own "a one-time event, not repeated on every subsequent
-- firing" rule -- guarded on auto_retrigger_budget_notice_sent_at IS
-- NULL so a caller can tell (via pgx.ErrNoRows on a guard miss) "this PR
-- was already notified", the same guarded-UPDATE-as-claim idiom
-- RetireFalsePositivePattern already establishes
-- (reviewfalsepositivepatterns.sql) for a comparable single-writer,
-- once-only transition.
UPDATE github_pr_sessions
SET auto_retrigger_budget_notice_sent_at = now()
WHERE repo_full_name = $1 AND pr_number = $2 AND auto_retrigger_budget_notice_sent_at IS NULL
RETURNING *;

-- name: ReadRepoEntitlement :one
-- The one read of a repository's eligibility for session creation (§31.4):
-- whether this deployment knows it, and whether an administrator revoked
-- it, in one statement and so one snapshot. No caller can read the first
-- without the second.
--
-- repo_known: ANY github_pr_sessions row exists for repo_full_name, across
-- every pr_number -- the composite primary key's own leading column
-- (repo_full_name, pr_number) indexes this. This is a sound "this
-- deployment is genuinely attached to this repo" proof because the ONLY
-- writer of that table is internal/adapters/inbound/github's own
-- HMAC-verified webhook ingress (coalesce.go/handler.go) -- no httpapi REST
-- handler writes it -- and because a row only ever COMMITS with a non-NULL
-- session_id (coalesce.go's own single-transaction
-- EnsureRow+LockForUpdate+SetSessionID sequencing: a denied/failed claim
-- rolls back the whole transaction, leaving no row behind), so a bare
-- existence check needs no separate session_id IS NOT NULL filter. See
-- httpapi's own resolveKnownRepo (reposettings.go) for the full "why this
-- signal, and why repo_settings/sessions.repos are NOT sound" reasoning.
--
-- revoked: an administrator revoked the repository
-- (repo_entitlement_revocations). Keyed by the exact
-- name, like repo_known; the primary key indexes it.
SELECT
    EXISTS (SELECT 1 FROM github_pr_sessions g WHERE g.repo_full_name = sqlc.arg('repo_full_name')) AS repo_known,
    EXISTS (SELECT 1 FROM repo_entitlement_revocations r WHERE r.repo_full_name = sqlc.arg('repo_full_name')) AS revoked;

-- name: FirstRevokedRepoForSession :one
-- The first repository of a session that an administrator revoked
-- (§31.4), read again before the session's sandbox is spawned and before
-- each of its turns is dispatched (internal/app/sessionactor). A session
-- names its repositories two ways, and both are checked: the clone URLs in
-- sessions.repos, parsed by the caller into repo_full_names, and the
-- pull-request claims keyed to the session -- a review session's
-- github_pr_sessions row, and a sentinel auto-fix child's sentinel_fixes
-- row. The claims name the pull request's base repository, which the clone
-- URL of a fork pull request does not. pgx.ErrNoRows means none is revoked.
-- Indexed on every arm: repo_entitlement_revocations' primary key,
-- github_pr_sessions(session_id) (migrations/000032) and
-- sentinel_fixes(fix_child_session_id) (migrations/000047).
SELECT r.repo_full_name FROM repo_entitlement_revocations r
WHERE r.repo_full_name = ANY (sqlc.arg('repo_full_names')::text[])
   OR r.repo_full_name IN (
        SELECT g.repo_full_name FROM github_pr_sessions g WHERE g.session_id = sqlc.arg('session_id')
        UNION
        SELECT f.repo_full_name FROM sentinel_fixes f WHERE f.fix_child_session_id = sqlc.arg('session_id'))
ORDER BY r.repo_full_name
LIMIT 1;

-- handleReviewRetriggerDebounceTimer's own read of pending_retrigger_head_
-- sha/auto_retrigger_count/auto_retrigger_budget_notice_sent_at reuses the
-- EXISTING GetGitHubPRSessionBySessionID above (a.sessionID is exactly
-- what a TimerFired command carries -- there is no separate (repo,
-- pr_number) identity to look this row up by at that point) -- no new
-- query needed for it.

-- name: RecordMergeOutcome :one
-- §31.7's own G4 arming write (migrations/
-- 000118_github_pr_sessions_merge_outcome.up.sql): captures the SAME
-- `pull_request` "closed" webhook's own pull_request.merged/closed_at
-- fields verbatim -- never re-derived, never polled. Guarded on
-- session_id IS NOT NULL, mirroring UpsertPendingRetriggerHeadSHA's own
-- identical guard: a claim row with no session ever attached was never
-- actually reviewed, so it has nothing for a merge outcome to arm
-- eligibility for. pgx.ErrNoRows (unwrapped) means exactly that --
-- either no github_pr_sessions row exists for this PR at all (Narvi was
-- never mentioned on it), or one exists with session_id still NULL --
-- both acknowledged and ignored by the caller, the SAME "no session to
-- act on" outcome UpsertPendingRetriggerHeadSHA's own callers already
-- treat identically. Overwrites (never appends) on every event, the SAME
-- "last observed wins" polarity UpsertPendingRetriggerHeadSHA's own doc
-- comment states for a re-opened-then-re-closed PR (a real but rare
-- GitHub possibility this table does not attempt to model as history).
UPDATE github_pr_sessions
SET pr_merged = $3, pr_closed_at = $4
WHERE repo_full_name = $1 AND pr_number = $2 AND session_id IS NOT NULL
RETURNING *;
