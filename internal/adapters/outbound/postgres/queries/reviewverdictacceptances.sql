-- Queries backing ReviewVerdictAcceptanceStore ("human acceptance of a
-- verdict the engine refuses", §21.1b) -- see migrations/
-- 000135_review_verdict_acceptances.up.sql's own doc comment for the
-- table's full design.

-- name: SupersedeActiveReviewVerdictAcceptances :many
-- Revokes every currently-active row for (repo_full_name, pr_number),
-- revoked_by the SAME accepting user (this is an automatic supersession
-- by a fresh accept, not a maintainer's own explicit revoke click, so
-- there is no separate revoker to name -- the accepting user is the only
-- actor this statement genuinely knows about) and revocation_reason =
-- 'superseded' (finding F4, adversarial review -- distinct from
-- RevokeReviewVerdictAcceptance's own 'explicit', below: this column is
-- what lets a reader tell "this row was auto-superseded by a fresh
-- accept" apart from "a maintainer explicitly clicked revoke", which
-- revoked_at/revoked_by alone cannot). Called by
-- ReviewVerdictAcceptanceStore.Insert (postgres package) IMMEDIATELY
-- before InsertReviewVerdictAcceptance below, as two separate statements
-- -- NEVER as one WITH-clause statement (finding F2's own first attempt,
-- adversarial review: `WITH superseded AS (UPDATE ...) INSERT ...` reads
-- as atomic and is the obvious idiom, but PostgreSQL's WITH sub-statements
-- and the primary statement all execute against the SAME snapshot taken
-- at the START of the query -- verified against real Postgres in a
-- scratch database: the primary INSERT's own unique-constraint check does
-- NOT see the CTE's UPDATE having cleared the conflicting index entry,
-- and the whole statement fails with a spurious duplicate-key error on
-- the routine, common case this exists to allow -- a plain re-accept of
-- the SAME verdict). Two ordinary sequential statements (this one, then
-- the INSERT) do not share this hazard -- PROVIDED they run inside the
-- SAME transaction (finding F2, adversarial review, corrected: this used
-- to be true only in the CTE sense, never in the commit sense -- see
-- ReviewVerdictAcceptanceStore.Insert's own doc comment, postgres
-- package, for the fix: the caller now supplies a WithTx-scoped store).
-- review_verdict_acceptances_one_active_idx is the actual invariant
-- enforcer either way: a concurrent accept racing between this statement
-- and the INSERT below (from a DIFFERENT transaction) can still make the
-- INSERT fail on that constraint (a genuine, rare double-accept race) --
-- an ordinary, retryable 500, never a silent violation of "at most one
-- active row". RETURNING * (finding F4: this used to be :exec, silently
-- discarding which row, if any, was superseded) is what lets the caller
-- record a DISTINCT, properly-attributed audit fact for the supersession
-- itself, naming the superseded row's own id -- see
-- httpapi.AcceptReviewVerdict's own review_verdict.accept_supersedes_prior
-- audit action.
UPDATE review_verdict_acceptances
SET revoked_at = now(), revoked_by = $3, revocation_reason = 'superseded'
WHERE repo_full_name = $1 AND pr_number = $2 AND revoked_at IS NULL
RETURNING *;

-- name: InsertReviewVerdictAcceptance :one
-- APPEND-ONLY create (this table's own migration doc comment: "never
-- UPDATEd for a re-accept") -- but AT MOST ONE row may be ACTIVE
-- (non-revoked) for a given (repo_full_name, pr_number) at a time
-- (review_verdict_acceptances_one_active_idx, finding F2, adversarial
-- review: two live acceptances let a revocation of "the" active one
-- silently re-activate the other). ReviewVerdictAcceptanceStore.Insert
-- always calls SupersedeActiveReviewVerdictAcceptances above FIRST, as a
-- separate statement (see that query's own doc comment for why never a
-- single combined WITH-clause statement), so an ordinary re-accept never
-- collides with this INSERT's own unique-index check.
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
-- Looked up by the revoke endpoint's own failed-revoke diagnosis path
-- (httpapi.RevokeReviewVerdictAcceptance) -- id and repoFullName both
-- arrive in that endpoint's JSON request body (POST
-- /api/decision-inbox/revoke-verdict-acceptance, restdtos.
-- RevokeReviewVerdictAcceptanceRequest), never off a URL path segment or
-- route (finding F10, adversarial review: this comment previously
-- described a route surface -- "acceptance id comes straight off the URL
-- path" -- this endpoint does not have; it is a flat POST with no path
-- parameters at all). SCOPED to repoFullName regardless, mirroring
-- GetFalsePositivePattern's own identical "id alone is not enough"
-- audit-fix precedent (reviewfalsepositivepatterns.sql): a
-- pattern/acceptance belonging to a DIFFERENT repo must never be
-- reachable through the wrong repo's own request.
SELECT * FROM review_verdict_acceptances WHERE id = $1 AND repo_full_name = $2;

-- name: RevokeReviewVerdictAcceptance :one
-- The revoke write: a maintainer+ (the SAME role that may accept --
-- see internal/domain/authz's own ActionAcceptReviewVerdict, row 5)
-- explicitly withdraws an active acceptance -- revocation_reason =
-- 'explicit' (finding F4, adversarial review), distinct from
-- SupersedeActiveReviewVerdictAcceptances' own 'superseded' above: THIS
-- is the one write that is a genuine, deliberate human revocation click,
-- never an automatic side effect of a fresh accept. SCOPED to
-- repo_full_name (mirrors RetireFalsePositivePattern's own identical
-- audit-fix precedent) AND guarded (WHERE revoked_at IS NULL, CLAUDE.md/
-- §11's own "guarded UPDATE ... WHERE for cross-writer transitions" rule)
-- so revoking an ALREADY-revoked acceptance is a no-op that returns
-- pgx.ErrNoRows (never a second, silently overwriting revoked_at/
-- revoked_by) -- the caller (httpapi) tells "never existed in this
-- repo" apart from "exists in this repo but already revoked" via a
-- follow-up GetReviewVerdictAcceptance (also repo-scoped) read on this
-- same error path, mirroring RetireFalsePositivePattern's own identical
-- caller-side discipline.
UPDATE review_verdict_acceptances
SET revoked_at = now(), revoked_by = $2, revocation_reason = 'explicit'
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
