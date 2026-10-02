-- repo_entitlement_revocations (technical plan §31.4):
-- an administrator's revocation of a repository's eligibility for new
-- sessions. The writes below back the three admin-only routes under
-- /api/repos/{owner}/{repo}/entitlement (httpapi/repoentitlement.go), each
-- run in the transaction that records its audit_log row. Eligibility itself
-- is read only through ReadRepoEntitlement and FirstRevokedRepoForSession
-- (githubprsessions.sql), never from here.

-- name: RevokeRepoEntitlement :one
-- Revokes repo_full_name. ON CONFLICT DO NOTHING: a repository already
-- revoked keeps its first revocation's who, when and why, and the caller
-- sees pgx.ErrNoRows (409 "already revoked").
INSERT INTO repo_entitlement_revocations (repo_full_name, revoked_by, reason)
VALUES (sqlc.arg('repo_full_name'), sqlc.narg('revoked_by'), sqlc.arg('reason'))
ON CONFLICT (repo_full_name) DO NOTHING
RETURNING *;

-- name: RestoreRepoEntitlement :one
-- Lifts repo_full_name's revocation and returns what was lifted, for the
-- audit row. pgx.ErrNoRows when the repository is not revoked (409 "not
-- revoked"). Restoring re-opens only what github_pr_sessions already
-- admits: it never grants eligibility.
DELETE FROM repo_entitlement_revocations
WHERE repo_full_name = sqlc.arg('repo_full_name')
RETURNING *;

-- name: GetRepoEntitlementRevocation :one
-- repo_full_name's revocation, with the revoking administrator's display
-- name (NULL once that user is deleted: revoked_by is ON DELETE SET NULL).
-- pgx.ErrNoRows when the repository is not revoked.
SELECT
    r.repo_full_name,
    r.revoked_at,
    r.revoked_by,
    r.reason,
    u.display_name AS revoked_by_display_name
FROM repo_entitlement_revocations r
LEFT JOIN users u ON u.id = r.revoked_by
WHERE r.repo_full_name = sqlc.arg('repo_full_name');
