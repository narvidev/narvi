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
-- and Postgres takes a snapshot per statement. The first statement locks
-- every candidate session's row, in id order (FOR UPDATE OF s): it waits
-- for any transaction holding one -- every session actor's transaction
-- locks its own session's row first (GetSessionActorEpochForUpdate), a
-- spawn's included -- and from then on no actor of those sessions can
-- write, so the second statement, on a fresh snapshot, reads each
-- sandbox's status as it stands and moves exactly the sessions with no
-- live gen. Without the first, the UPDATE's snapshot could read a sandbox
-- stopped that a spawn committing while the UPDATE waited on its row had
-- just started, and move the spec under that gen.
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
FROM github_pr_sessions g
WHERE g.session_id = s.id
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
