-- repo_entitlement_revocations: an administrator's revocation of a
-- repository's eligibility for new sessions (technical plan §31.4,
-- "Un-entitlement"). Eligibility itself is unchanged: a repository is
-- eligible when a github_pr_sessions row names it, and only the verified
-- GitHub webhook ingress writes those rows. A row here closes that
-- repository again, whatever github_pr_sessions holds, until an
-- administrator restores it (POST /api/repos/{owner}/{repo}/entitlement/
-- restore, which deletes the row). Revoke and restore are one admin-only
-- action, manage_repo_entitlement; each writes its audit_log row
-- (repo_entitlement.revoked, repo_entitlement.restored) in the same
-- transaction as the row change, and that is where the history lives.
--
-- A row being present is the whole fact. Every read of eligibility reads it
-- in the same statement (ReadRepoEntitlement), and a session's spawn and
-- each turn's dispatch read it again (FirstRevokedRepoForSession), so a
-- revocation also stops the pending turns of sessions created before it.
--
-- Keyed by the exact repo_full_name github_pr_sessions holds: matching is
-- exact, like the eligibility read. Revoking requires the repository to be
-- known to the deployment, so a stored name is always one a webhook payload
-- named. The primary key is the only index either read needs.
--
-- To list every revoked repository (there is no endpoint for it):
--   SELECT repo_full_name, revoked_at, revoked_by, reason
--   FROM repo_entitlement_revocations ORDER BY revoked_at;
--
-- IF NOT EXISTS: see "Rolling back" -- a rollback that keeps the table
-- leaves it in place, with its rows, when this file runs again.
--
-- # Locks
--
-- A catalog-only create of an empty table: no existing table is locked or
-- rewritten.
--
-- # Rolling deploy
--
-- The previous binary never names this table. During a rolling deploy its
-- pods keep admitting new sessions and dispatching turns on a repository
-- an administrator revokes, and the revoke route exists only on pods of
-- this release: a revocation is fully enforced once every replica runs it.
--
-- # Rolling back
--
-- Every control-plane boot runs the embedded migrations up
-- (controlplane/migrate.go), and golang-migrate refuses a database whose
-- version it has no file for. So once this migration is applied, the
-- previous binary cannot boot ("no migration found for version 156"). A
-- rollback therefore takes one of two steps first, with the control plane
-- scaled to zero:
--   - Keep the rows: with the golang-migrate CLI, `migrate force 155`. The
--     previous binary then boots and never reads them, so they are not
--     enforced while it runs; when this release is deployed again, this
--     file runs again, leaves the table and its rows as they are, and they
--     are enforced again.
--   - Drop them: run this migration's down (`migrate goto 155`) with this
--     release's migrations. Every revocation goes with the table, and each
--     repository has to be revoked again after this release is redeployed.
-- Either way, EVERY REVOKED REPOSITORY IS ELIGIBLE FOR NEW SESSIONS AGAIN
-- under the previous binary. Record the list above before rolling back.
CREATE TABLE IF NOT EXISTS repo_entitlement_revocations (
    repo_full_name TEXT PRIMARY KEY,
    revoked_at     TIMESTAMPTZ NOT NULL DEFAULT now(),
    revoked_by     UUID REFERENCES users(id) ON DELETE SET NULL,
    reason         TEXT NOT NULL
        CONSTRAINT repo_entitlement_revocations_reason_check
        CHECK (char_length(btrim(reason)) BETWEEN 1 AND 500)
);
