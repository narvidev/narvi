-- Queries backing ReviewCheckRunStore (§21.1b's own publisher identity).
-- See migrations/000132_review_check_runs.up.sql's own doc
-- comment for the full per-PR claim design these implement together --
-- mirrors EnsureGitHubPRSessionRow/LockGitHubPRSessionForUpdate/
-- SetGitHubPRSessionID's own identical three-step atomic-claim idiom
-- (githubprsessions.sql), applied here to "which candidate emission wins
-- the row" instead of "which mention wins the session".

-- name: EnsureReviewCheckRunRow :exec
-- Idempotently ensures a (repo_full_name, pr_number) claim row exists,
-- with a deliberately EMPTY placeholder phase/head_sha on a fresh
-- insert -- the real values are always written by
-- UpdateReviewCheckRunPublished below, under LockReviewCheckRunForUpdate's
-- own lock, never by this INSERT itself (mirrors EnsureGitHubPRSessionRow's
-- own "session_id left NULL on a fresh insert" precedent: this step's
-- only job is "make sure the row is there to lock next").
INSERT INTO review_check_runs (repo_full_name, pr_number, head_sha, phase)
VALUES ($1, $2, '', '')
ON CONFLICT (repo_full_name, pr_number) DO NOTHING;

-- name: LockReviewCheckRunForUpdate :one
-- Locks the (repo_full_name, pr_number) row for the remainder of the
-- caller's own transaction -- any concurrent caller's own call to this
-- SAME query for the SAME PR blocks here until the first caller commits
-- or rolls back. The caller must have already run
-- EnsureReviewCheckRunRow (in the SAME transaction) so this row is
-- guaranteed to exist by the time this runs.
SELECT * FROM review_check_runs
WHERE repo_full_name = $1 AND pr_number = $2
FOR UPDATE;

-- name: UpdateReviewCheckRunPublished :one
-- Records a winning internal/domain/reviewcheck.Emission -- called only
-- after the caller has already evaluated reviewcheck.Supersedes against
-- the row LockReviewCheckRunForUpdate just returned, still holding that
-- SAME lock (this UPDATE never itself decides whether to apply; it is
-- the caller's own already-made decision, persisted). external_id is
-- deliberately NOT set here: this write happens BEFORE the real GitHub
-- call (reserving the identity, never spanning the network call inside
-- this transaction -- migrations/000132's own doc comment), so
-- external_id is left exactly as it was (unchanged) when the head_sha is
-- unchanged too (an in-place update, the existing external check run
-- still applies), or explicitly cleared to NULL by the caller via
-- ClearReviewCheckRunExternalID below when head_sha is changing (a new
-- commit landed -- the OLD external_id belongs to a SHA this row no
-- longer targets, and reusing it would silently update the wrong,
-- superseded check run).
UPDATE review_check_runs
SET head_sha = $3, attempt_id = $4, attempt_created_at = $5, phase = $6,
    base_ref = $7, base_sha = $8, policy_version = $9, updated_at = now()
WHERE repo_full_name = $1 AND pr_number = $2
RETURNING *;

-- name: ClearReviewCheckRunExternalID :exec
-- Companion to UpdateReviewCheckRunPublished above -- called in the SAME
-- transaction, under the SAME lock, whenever the caller (outboxworker's
-- own reviewCheckNotifier, newIdentityNeeded) has decided the existing
-- external_id must not carry forward. Finding A10: this doc used to name
-- a head_sha change as the ONLY such condition -- the caller ALSO clears
-- it when the row is already terminal-shaped (Stale/TerminalAssessed/
-- TerminalNotAssessed) and candidate belongs to a genuinely newer
-- attempt on the SAME head sha ("open a NEW check when a review
-- restarts after a terminal result... never reopen a concluded one",
-- finding A1) -- a case this doc omitted even though the caller has
-- always implemented it. Both share the same reason: the existing
-- external_id belongs to an identity this row's own next write must not
-- reuse (a different SHA in the first case; an already-concluded run in
-- the second).
UPDATE review_check_runs
SET external_id = NULL
WHERE repo_full_name = $1 AND pr_number = $2;

-- name: SetReviewCheckRunExternalID :one
-- The publisher's own POST-GitHub-call record write -- a GUARDED update
-- (CLAUDE.md/§11's own "guarded UPDATE ... WHERE for cross-writer
-- transitions" idiom), never a blind SET: applies only if this row's
-- attempt_id still matches attempt_id_guard AND external_id is still
-- NULL, i.e. nobody else has since reserved a NEWER identity on this row
-- while this caller's own GitHub call was in flight (Notifier.Deliver
-- runs with no Postgres transaction held open across it -- migrations/
-- 000132's own doc comment). A guard miss (0 rows, pgx.ErrNoRows) means
-- exactly that race happened: this caller's own GitHub check run may now
-- be an orphan nothing else in this system ever points to again (an
-- accepted, bounded residual -- mirrors githubapi.VerdictNotifier's own
-- documented "CreateReview is not itself idempotent... a known, accepted
-- limitation shared with every other Notifier in this codebase" residual)
-- -- the caller logs it and returns nil (this delivery attempt is done;
-- whichever newer candidate won the row already has, or will soon get,
-- its OWN correct external_id recorded by its own Deliver call).
UPDATE review_check_runs
SET external_id = $3, updated_at = now()
WHERE repo_full_name = $1 AND pr_number = $2 AND attempt_id IS NOT DISTINCT FROM $4 AND external_id IS NULL
RETURNING *;

-- name: GetReviewCheckRunByRepoAndPRNumber :one
-- A plain, non-locking read -- mirrors GetGitHubPRSessionByRepoAndPRNumber's
-- own identical "forward, non-locking read" precedent
-- (githubprsessions.sql). pgx.ErrNoRows means no row exists yet for this
-- PR.
--
-- Finding A10: this used to be documented as "the read a Notifier.Deliver
-- call opens with... resolves reviewcheck.Supersedes' own current
-- argument" -- describing a pre-read Deliver never actually performed
-- (Deliver's own claim sequence resolves "current" from
-- LockReviewCheckRunForUpdate, inside its transaction, never from this
-- query) -- and, before finding A3's own fix, had no production caller
-- at all. It now has one: reviewCheckNotifier.
-- guardAgainstSupersessionDuringCall (reviewcheck.go) calls this, with
-- no transaction open, AFTER a GitHub write completes, to detect whether
-- a concurrently-racing, newer Deliver call already committed a
-- different row while that write was in flight -- a genuinely reachable
-- pgx.ErrNoRows there would mean this row was deleted between this
-- notifier's own earlier claim and this later read (not a real
-- production path today, but handled rather than assumed unreachable).
SELECT * FROM review_check_runs
WHERE repo_full_name = $1 AND pr_number = $2;
