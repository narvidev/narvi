-- §15.3/§12.2 item 9's own composition-review anchor fields: a confirmed-
-- major fix on top of migrations/000127_release_manifest_checks_
-- composition.up.sql's own composition_reviewed_at/composition_findings/
-- composition_decision columns.
--
-- Before this migration, a posted composition finding carried no record
-- of WHICH commit (head sha) or how complete a diff it was actually
-- reviewed against -- a verdict attributed to a diff nobody could
-- identify afterward is not auditable. composition_head_sha/
-- composition_diff_truncated are populated once, at DISPATCH time
-- (internal/app/releasereview.dispatchCompositionReview, the SAME moment
-- the composition review turn's own turns.review_head_sha is set), never
-- at findings-post time -- the reviewing agent's own composition-findings-
-- posting tool call carries no head-sha of its own to report, and the
-- head sha this check's own composition turn was actually dispatched
-- against is already a known, durable fact the instant that turn is
-- created; recording it then, on the SAME release_manifest_checks row the
-- findings will later land on, is strictly more honest than inferring it
-- later from the turn row (a caller would have to know to go looking for
-- a turns.review_head_sha at all).
--
-- Nullable: a row whose composition pass was never dispatched at all (no
-- trigger fired, or the dispatch-time deps were not configured, or the
-- dispatch declined for want of a live head sha/diff -- compositiondispatch.
-- go's own fail-closed branches) carries neither column, mirroring
-- composition_reviewed_at's own identical "not yet available" NULL
-- convention one column over.
ALTER TABLE release_manifest_checks
    ADD COLUMN composition_head_sha TEXT,
    ADD COLUMN composition_diff_truncated BOOLEAN;
