-- Queries backing ReviewVerdictAcceptanceStore ("human acceptance of a
-- verdict the engine refuses", §21.1b) -- see migrations/
-- 000135_review_verdict_acceptances.up.sql's own doc comment for the
-- table's full design.

-- name: InsertReviewVerdictAcceptance :one
-- APPEND-ONLY create (this table's own migration doc comment: "never
-- UPDATEd for a re-accept") -- a maintainer+ accepting the SAME verdict
-- twice (a double-click, a retried request) simply inserts a second row;
-- GetActiveReviewVerdictAcceptance's own "latest non-revoked row" read is
-- what a caller consults, never a uniqueness constraint here.
INSERT INTO review_verdict_acceptances (
    repo_full_name, pr_number, verdict_id, attempt_id, head_sha,
    base_ref, base_sha, ancestor_chain, policy_version,
    reason, justification, accepted_by
) VALUES (
    $1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12
)
RETURNING *;

-- name: GetActiveReviewVerdictAcceptance :one
-- The one read the merge/decision-inbox path needs: the LATEST
-- non-revoked acceptance for one pull request, if any -- pgx.ErrNoRows
-- means no acceptance is currently active for this PR (never accepted at
-- all, or every acceptance ever granted has since been revoked).
-- Applicability against the PR's CURRENT verdict is decided by the
-- caller (internal/domain/reviewverdict.Acceptance.Applicable), never by
-- this query -- a row returned here may still be inapplicable (a new
-- attempt posted a different verdict since this row was accepted).
SELECT * FROM review_verdict_acceptances
WHERE repo_full_name = $1 AND pr_number = $2 AND revoked_at IS NULL
ORDER BY accepted_at DESC
LIMIT 1;

-- name: GetReviewVerdictAcceptance :one
-- Looked up by the revoke endpoint (acceptance id comes straight off the
-- URL path, repoFullName off the route) -- SCOPED to repoFullName,
-- mirroring GetFalsePositivePattern's own identical "id alone is not
-- enough" audit-fix precedent (reviewfalsepositivepatterns.sql): a
-- pattern/acceptance belonging to a DIFFERENT repo must never be
-- reachable through the wrong repo's own URL.
SELECT * FROM review_verdict_acceptances WHERE id = $1 AND repo_full_name = $2;

-- name: RevokeReviewVerdictAcceptance :one
-- The revoke write: a maintainer+ (the SAME role that may accept --
-- see internal/domain/authz's own ActionAcceptReviewVerdict, row 5)
-- explicitly withdraws an active acceptance. SCOPED to repo_full_name
-- (mirrors RetireFalsePositivePattern's own identical audit-fix
-- precedent) AND guarded (WHERE revoked_at IS NULL, CLAUDE.md/§11's own
-- "guarded UPDATE ... WHERE for cross-writer transitions" rule) so
-- revoking an ALREADY-revoked acceptance is a no-op that returns
-- pgx.ErrNoRows (never a second, silently overwriting revoked_at/
-- revoked_by) -- the caller (httpapi) tells "never existed in this
-- repo" apart from "exists in this repo but already revoked" via a
-- follow-up GetReviewVerdictAcceptance (also repo-scoped) read on this
-- same error path, mirroring RetireFalsePositivePattern's own identical
-- caller-side discipline.
UPDATE review_verdict_acceptances
SET revoked_at = now(), revoked_by = $2
WHERE id = $1 AND repo_full_name = $3 AND revoked_at IS NULL
RETURNING *;

-- name: ListReviewVerdictAcceptances :many
-- The audit view: EVERY acceptance for one pull request, active or
-- revoked, newest-first -- mirrors ListFalsePositivePatterns' own
-- identical audit-view shape (reviewfalsepositivepatterns.sql), bounded
-- by limit per this codebase's own "no unbounded list, ever" discipline.
SELECT * FROM review_verdict_acceptances
WHERE repo_full_name = $1 AND pr_number = $2
ORDER BY accepted_at DESC
LIMIT $3;
