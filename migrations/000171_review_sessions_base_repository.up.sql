-- Technical plan §21.1 and §30.4: a pull request's review session clones
-- the pull request's base repository and reads the head from the base's
-- refs/pull/<number>/head, with the base repository's own installation.
-- The GitHub ingress now writes the base repository into every review
-- session's spec, with the head branch only when that branch is in the
-- base repository (internal/adapters/inbound/github's reviewSessionRepo).
-- A session created before it did names its pull request's head
-- repository: for a pull request from a fork, the fork, with the fork's
-- branch. This moves those sessions onto the base repository.
--
-- What it moves: a session with exactly one github_pr_sessions claim, in
-- owner/name shape, whose sessions.repos is one repo whose url is
-- https://<host>/<owner>/<name>[.git] and names a repository other than
-- the claim's -- compared without regard to case or a ".git" suffix, as
-- sessionactor's pullRequestRef compares them. Its url becomes
-- https://<the same host>/<the claim's owner/name>.git and its branch
-- null: a fork's branch names nothing in the base, and the spec's readers
-- that act on a branch act on the spec's repository (the sentinel
-- auto-fix, apply-suggestion). Its name is kept, so a restored workspace
-- keeps its directory. Every other session is untouched -- a
-- same-repository one, a non-review one, a multi-repo one -- so the
-- statement is idempotent.
--
-- Only a session whose sandbox holds no live gen moves here: no sandbox
-- row, or one pending, stopped or failed. A gen keeps the spec it booted
-- with (SESSION_CONFIG is delivered at spawn and restore only), and its
-- clone's origin is the fork's, which holds no pull ref, while the
-- checkout gate reads the spec as the one the live gen booted with: moved
-- under a live gen, a session's review turns would be sent a checkout that
-- gen cannot fetch, retried, and refused at ReviewCheckoutTimeout. A
-- session skipped here moves in the transaction that spawns or restores
-- its next gen, before that gen's SESSION_CONFIG is assembled
-- (sessionactor's reviewbaserepository.go, the same expression as
-- MoveReviewSessionToBaseRepository in queries/sessions.sql); until then
-- it dispatches as before, on the gen that cloned the fork. No provider
-- resumes a sandbox today (ports.Capabilities.Resume), and a resume, which
-- delivers no spec, is never a moment the actor moves one.
--
-- # Locks
--
-- golang-migrate sends this file as one batch, one implicit transaction,
-- and under READ COMMITTED Postgres takes a snapshot per statement. Every
-- session actor's transaction locks its own session's row first
-- (GetSessionActorEpochForUpdate), a spawn's included, so a session whose
-- row this transaction holds can start no gen until it commits.
--
--   1. A temporary table, dropped at commit, holds the ids this file
--      locks.
--   2. INSERT ... SELECT ... FOR UPDATE OF s locks every candidate
--      session's row its snapshot sees, in id order, waiting for any
--      transaction holding one, and records each id it locked.
--   3. The UPDATE, on a fresh snapshot taken once all of them are held,
--      reads each locked session's sandbox as it stands and moves exactly
--      those with no live gen -- and only sessions step 2 locked.
--   4. The temporary table is dropped: it never outlives the file, and the
--      schema this file leaves (which sqlc also reads) has no trace of it.
--
-- Without step 2, the UPDATE's own snapshot could read a sandbox stopped
-- that a spawn committing while the UPDATE waited on its row had just
-- started, and move the spec under that gen. Without the locked ids
-- carried into step 3, its fresh snapshot would also see a session a
-- previous replica opened while step 2 ran -- one step 2 never locked --
-- and could move it while that session's first spawn, booting on the
-- fork's spec, committed. Such a session is left for the actor to move at
-- its next spawn or restore.
--
-- Only rows are locked: no table lock stronger than ROW EXCLUSIVE on
-- sessions and ACCESS SHARE on github_pr_sessions and sandboxes, which
-- block no read or write of them. The rows locked are the candidate
-- sessions' alone, held until the file commits; no transaction locks two
-- pull requests' review sessions' rows (a child session's insert takes
-- its parent's row FOR SHARE, and a stop walks children, neither of which
-- is a review session), so the order cannot cycle. No index is needed:
-- the claims are read through github_pr_sessions_session_id_idx and the
-- sandboxes by session_id, and the sessions scanned are read once.
--
-- # Rolling deploy
--
-- The previous binary works with the moved rows. Its SESSION_CONFIG sets
-- the pull request's ref on a spec whose url names the claim's repository,
-- so a moved session's next gen boots on the base repository at the ref,
-- and its checkout gate checks that session's review turns out; its
-- credential mint asks the base repository's installation, and its
-- apply-suggestion answers a moved session, which names no branch, with
-- the 500 it answered a branchless spec before. While it still serves
-- webhooks, its ingress writes the fork's url for a fork's new review
-- session: this release's actor moves that session when it spawns its
-- first gen, and a gen the previous binary spawns for it dispatches as
-- before -- the window a rolling deploy always leaves for the writes of
-- the binary it replaces.
--
-- # Rolling back
--
-- The down changes nothing (its own header says why). The previous binary
-- cannot boot on this version: golang-migrate finds no file for it. With
-- the control plane scaled to zero, either `migrate force` the previous
-- version or run the down (goto the previous version); either way the
-- moved sessions stay on their base repository, which the previous binary
-- reads as described above, and this file, run again, moves nothing it
-- already moved.
CREATE TEMPORARY TABLE review_sessions_base_repository_locked (id UUID PRIMARY KEY) ON COMMIT DROP;

INSERT INTO review_sessions_base_repository_locked (id)
SELECT s.id
FROM sessions s
JOIN github_pr_sessions g ON g.session_id = s.id
WHERE (SELECT count(*) FROM github_pr_sessions c WHERE c.session_id = s.id) = 1
  AND g.repo_full_name ~ '^[^/]+/[^/]+$'
  AND CASE WHEN jsonb_typeof(s.repos) = 'array' THEN jsonb_array_length(s.repos) END = 1
  AND s.repos->0->>'url' ~ '^https://[^/]+/[^/]+/[^/]+$'
  AND lower(regexp_replace(substring(s.repos->0->>'url' from '^https://[^/]+/(.*)$'), '\.git$', ''))
      <> lower(g.repo_full_name)
ORDER BY s.id
FOR UPDATE OF s;

UPDATE sessions s
SET repos = jsonb_set(
        jsonb_set(s.repos, '{0,url}',
            to_jsonb(substring(s.repos->0->>'url' from '^(https://[^/]+/)') || g.repo_full_name || '.git')),
        '{0,branch}', 'null'::jsonb)
FROM github_pr_sessions g, review_sessions_base_repository_locked locked
WHERE locked.id = s.id
  AND g.session_id = s.id
  AND (SELECT count(*) FROM github_pr_sessions c WHERE c.session_id = s.id) = 1
  AND g.repo_full_name ~ '^[^/]+/[^/]+$'
  AND CASE WHEN jsonb_typeof(s.repos) = 'array' THEN jsonb_array_length(s.repos) END = 1
  AND s.repos->0->>'url' ~ '^https://[^/]+/[^/]+/[^/]+$'
  AND lower(regexp_replace(substring(s.repos->0->>'url' from '^https://[^/]+/(.*)$'), '\.git$', ''))
      <> lower(g.repo_full_name)
  AND NOT EXISTS (
      SELECT 1 FROM sandboxes x
      WHERE x.session_id = s.id
        AND x.status NOT IN ('pending', 'stopped', 'failed'));

DROP TABLE review_sessions_base_repository_locked;
