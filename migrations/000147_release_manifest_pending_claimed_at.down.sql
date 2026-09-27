-- Reverse of 000147_release_manifest_pending_claimed_at.up.sql. A claimed
-- row has already had its one attempt; the previous claim deleted rows, so
-- one left behind here would be claimed and checked a second time.
DELETE FROM release_manifest_pending WHERE claimed_at IS NOT NULL;
DROP INDEX IF EXISTS release_manifest_pending_session_id_idx;
ALTER TABLE release_manifest_pending DROP COLUMN IF EXISTS claimed_at;
