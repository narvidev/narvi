ALTER TABLE review_verdicts
    DROP COLUMN IF EXISTS attempt_id,
    DROP COLUMN IF EXISTS policy_version,
    DROP COLUMN IF EXISTS ancestor_chain,
    DROP COLUMN IF EXISTS base_sha,
    DROP COLUMN IF EXISTS base_ref;
