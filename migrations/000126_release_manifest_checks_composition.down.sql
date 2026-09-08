DELETE FROM prompt_templates WHERE name = 'release_composition_review';

DROP INDEX IF EXISTS release_manifest_checks_session_id_created_at_idx;

ALTER TABLE release_manifest_checks
    DROP COLUMN IF EXISTS composition_reviewed_at,
    DROP COLUMN IF EXISTS composition_findings,
    DROP COLUMN IF EXISTS composition_decision,
    DROP COLUMN IF EXISTS composition_decision_by,
    DROP COLUMN IF EXISTS composition_decision_at;
