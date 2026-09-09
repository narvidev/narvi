-- Queries backing ReleaseManifestCheckStore (§12.2 item 9, §15.2/§15.3) -- see
-- migrations/000097_release_manifest_checks.up.sql's own doc comment for
-- the table's full design.

-- name: InsertReleaseManifestCheck :one
-- The one write internal/app/releasereview.Run makes once it has computed
-- this release PR's own manifest findings + aggregate-review trigger
-- decision -- alongside, never instead of, its existing outbox-delivered
-- comment (RenderManifestComment). Best-effort: a failure here is logged
-- and Run simply continues to its own existing outbox enqueue, mirroring
-- that function's own established "every internal failure is logged and
-- this function simply returns" posture.
INSERT INTO release_manifest_checks (
    session_id, repo_full_name, pr_number, base_ref, head_ref,
    constituent_pr_count, coverage_partial,
    aggregate_review_triggered, aggregate_review_trigger_reasons,
    findings, merged_prs
)
VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11)
RETURNING *;

-- name: GetLatestReleaseManifestCheck :one
-- (repo_full_name, pr_number)'s own most-recently-computed check --
-- mirrors GetLatestReviewVerdict's own identical "one indexed lookup,
-- ORDER BY created_at DESC LIMIT 1" shape. pgx.ErrNoRows means no
-- manifest check has ever been persisted for this PR (a row predating
-- or a check whose own Run() insert failed and was only ever delivered as
-- a comment).
SELECT * FROM release_manifest_checks
WHERE repo_full_name = $1 AND pr_number = $2
ORDER BY created_at DESC
LIMIT 1;

-- name: GetLatestReleaseManifestCheckBySessionID :one
-- The session-scoped lookup: the composition-findings-posting
-- tool and the Block/Acknowledge actions all resolve their target row
-- from a sessionID alone (a sandbox-bearer-authenticated tool call, or an
-- authenticated browser request against /api/sessions/:id/release-manifest/*),
-- never from (repo_full_name, pr_number) -- mirrors GetLatestReleaseManifestCheck's
-- own identical "ORDER BY created_at DESC LIMIT 1" shape, backed by
-- release_manifest_checks_session_id_created_at_idx (migrations/
-- 000127_release_manifest_checks_composition.up.sql). pgx.ErrNoRows means
-- this session has no release manifest check on record at all.
SELECT * FROM release_manifest_checks
WHERE session_id = $1
ORDER BY created_at DESC
LIMIT 1;

-- name: UpdateReleaseManifestCompositionFindings :one
-- The composition-findings-posting tool write
-- (PostReleaseCompositionFindings, httpapi/releasecompositionfindings.go):
-- a guarded UPDATE ("AND composition_reviewed_at IS NULL") so this can
-- only ever succeed ONCE per row -- a retried/duplicate tool call for the
-- same session is a no-op here (pgx.ErrNoRows), never a silent
-- overwrite of an already-posted result. Scoped by id (the caller already
-- resolved the target row via GetLatestReleaseManifestCheckBySessionID,
-- and passes its own id back here) rather than session_id directly, so a
-- future Step that DOES re-run this check on a later push (this table's
-- own append-only design, migrations/000097's doc comment) can never have
-- this guarded write silently target the WRONG one of several rows
-- sharing a session_id.
UPDATE release_manifest_checks
SET composition_reviewed_at = now(),
    composition_findings = $2
WHERE id = $1 AND composition_reviewed_at IS NULL
RETURNING *;

-- name: UpdateReleaseManifestCompositionDecision :one
-- The Block release / Acknowledge & ship action write
-- (BlockReleaseComposition/AcknowledgeReleaseComposition, httpapi/
-- releasecompositiondecision.go): a guarded UPDATE comparing against
-- expectedCurrentDecision (the SAME current value the caller already
-- passed through internal/domain/review.TransitionCompositionDecision to
-- validate) -- pgx.ErrNoRows means a concurrent decision already won the
-- race between that validation and this write, reported back to the
-- caller as 409, never a silent overwrite of someone else's decision.
UPDATE release_manifest_checks
SET composition_decision = $2,
    composition_decision_by = $3,
    composition_decision_at = now()
WHERE id = $1 AND composition_decision = sqlc.arg(expected_decision)
RETURNING *;
