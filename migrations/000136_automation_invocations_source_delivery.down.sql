DROP INDEX IF EXISTS automation_invocations_source_delivery_uniq;
ALTER TABLE automation_invocations
    DROP COLUMN IF EXISTS source_provider,
    DROP COLUMN IF EXISTS source_delivery_id;
