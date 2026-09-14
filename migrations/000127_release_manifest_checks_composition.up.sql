-- Step 125 ("release composition findings", §15.3/§12.2 item 9): the
-- aggregate-diff composition review pass itself was, until this Step,
-- never dispatched anywhere -- only its own trigger decision
-- (aggregate_review_triggered, migrations/000097_release_manifest_checks.
-- up.sql) was computed and persisted. This migration adds the columns the
-- composition pass's own two new writers need:
--
--   - The composition-findings-posting tool (POST /sessions/:id/
--     release-manifest/composition-findings, sandbox-bearer authenticated,
--     internal/adapters/inbound/httpapi/releasecompositionfindings.go)
--     writes composition_reviewed_at + composition_findings, once, via a
--     guarded UPDATE ("... AND composition_reviewed_at IS NULL") keyed on
--     session_id -- idempotent against a retried/duplicate tool call.
--   - The Block release / Acknowledge & ship actions (§12.2 item 9, POST
--     /api/sessions/:id/release-manifest/{block,acknowledge}) write
--     composition_decision + composition_decision_by + composition_decision_at,
--     via a guarded UPDATE keyed on the CURRENT composition_decision value
--     (internal/domain/review.TransitionCompositionDecision's own
--     transition table governs which value is legal to write).
--
-- composition_reviewed_at is the sentinel this Step's own "never render a
-- failed query or an undispatched pass as a confident zero" discipline
-- depends on: NULL means "not yet available" (never computed, or dispatch
-- failed, or the turn simply has not completed yet) -- distinct, by
-- construction, from a real, empty composition_findings array (the pass
-- ran and found nothing to report), mirroring computed_at's own identical
-- role one column over (migrations/000097's own doc comment) and §21's
-- "not yet computed" sentinel convention generally.
--
-- composition_decision defaults to 'pending' -- not NULL-able, since
-- "no decision has been made yet" is this column's own zero/starting
-- state (internal/domain/review.CompositionDecisionPending), not an
-- absence of data the way composition_reviewed_at's NULL is. A CHECK
-- constraint pins the closed three-value enum at the database layer too,
-- mirroring this table's own existing discipline of never trusting a
-- single validation layer for a value application code also enforces
-- (review.CompositionDecision's own Go-side fail-conservative enum
-- handling, compositiondecision.go).
ALTER TABLE release_manifest_checks
    ADD COLUMN composition_reviewed_at TIMESTAMPTZ,
    ADD COLUMN composition_findings JSONB NOT NULL DEFAULT '[]'::jsonb,
    ADD COLUMN composition_decision TEXT NOT NULL DEFAULT 'pending'
        CONSTRAINT release_manifest_checks_composition_decision_check
        CHECK (composition_decision IN ('pending', 'blocked', 'acknowledged')),
    ADD COLUMN composition_decision_by UUID REFERENCES users(id),
    ADD COLUMN composition_decision_at TIMESTAMPTZ;

-- Backs PostReleaseCompositionFindings/BlockReleaseComposition/
-- AcknowledgeReleaseComposition's own shared "find this session's own
-- release manifest check row" lookup (ReleaseManifestCheckStore.
-- GetBySessionID) -- session_id is not unique on this append-only table
-- (migrations/000097's own doc comment: modeled append-only for a future
-- re-run Step, though in practice at most one row exists per session
-- today), so this index backs an "ORDER BY created_at DESC LIMIT 1"
-- lookup exactly like release_manifest_checks_repo_pr_created_at_idx
-- already does for (repo_full_name, pr_number).
CREATE INDEX release_manifest_checks_session_id_created_at_idx
    ON release_manifest_checks (session_id, created_at DESC);

-- §15.3's own versioned prompt template for the aggregate-diff composition
-- review pass -- a SECOND distinct template purpose in prompt_templates
-- (migrations/000033_intent_classifier.up.sql's own "one row per distinct
-- template purpose" precedent), reusing that SAME DB-backed storage/
-- versioning mechanism for a review-shaped prompt rather than a
-- classification one, per §15.3's own explicit "same mechanism as
-- §8.3/§12.2 item 5" instruction. No "{{variable_name}}" placeholder --
-- internal/app/releasereview's own dispatchCompositionReview has no
-- per-call value of its own to substitute in here, mirroring
-- 'intent_classifier_plan_followup' own identical "no substitution needed"
-- precedent (migrations/000074_plan_followup.up.sql).
INSERT INTO prompt_templates (name, template) VALUES (
    'release_composition_review',
    'You are reviewing the composition of a release: a pull request that bundles many already-individually-reviewed pull requests (a release-branch cut, or a develop-to-main promotion). Every constituent pull request has ALREADY been reviewed and approved on its own -- line-by-line correctness is not your job here, and you must never re-litigate logic already approved per pull request.

Your job is narrower and different: do these already-individually-correct changes conflict, duplicate each other, or invalidate an assumption one of them made about the others? Two concrete failure classes to watch for: a migration-numbering collision across sequential pull requests (each diff clean on its own; the conflict is only visible in the merged tree), and an endpoint rename silently regressed by an unrelated merge (neither visible in any single pull request''s own diff).'
);
