ALTER TABLE release_manifest_checks
    DROP COLUMN IF EXISTS composition_head_sha,
    DROP COLUMN IF EXISTS composition_diff_truncated;
