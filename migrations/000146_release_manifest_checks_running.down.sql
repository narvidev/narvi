-- Reverse of 000146_release_manifest_checks_running.up.sql. The pending
-- rows those checks were claimed from are already gone, so no check runs
-- twice; only the status of a check still in flight is lost.
DROP TABLE IF EXISTS release_manifest_checks_running;
DROP INDEX IF EXISTS release_manifest_pending_session_id_idx;
