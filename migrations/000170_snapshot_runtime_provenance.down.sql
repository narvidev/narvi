-- Reverses 000170_snapshot_runtime_provenance.up.sql. With the columns goes
-- what every sandbox's snapshot recorded of the agent that minted it.
-- Nothing a binary without 000170 reads; a binary with it that runs 000170
-- again reads every snapshot as "provenance unknown" -- restored, and
-- counted as unknown -- until the sandbox's next snapshot.
--
-- RUN IT WITH THE CONTROL PLANE SCALED TO ZERO, then deploy a binary without
-- 000170. Do not run it against live pods of a binary that carries 000170,
-- for two reasons:
--   - Every query of that binary that returns a whole sandbox row names
--     these columns (sqlc writes each SELECT * and RETURNING * out as a
--     column list): creating, reading and updating sandboxes among them,
--     and its UpdateSandboxSnapshotID and ClearSandboxSnapshot write them.
--     After this down each of them fails with SQLSTATE 42703 (column does
--     not exist), so no sandbox is spawned, restored or snapshotted and no
--     turn is dispatched or completed until the older binary replaces the
--     pod.
--   - A pod that carries 000170 and restarts before the older binary
--     replaces it applies 000170 again at boot, which locks the older binary
--     out again ("no migration found for version 170").
-- Guarded: IF EXISTS, so it also runs when the columns are already gone --
-- this down ran, the recorded version was then forced back to 170, and the
-- down runs again. Each drop is a catalog change: it rewrites nothing, but
-- takes ACCESS EXCLUSIVE on sandboxes for the file's one implicit
-- transaction.
ALTER TABLE sandboxes DROP COLUMN IF EXISTS snapshot_minted_at;
ALTER TABLE sandboxes DROP COLUMN IF EXISTS snapshot_runtime_version;
ALTER TABLE sandboxes DROP COLUMN IF EXISTS snapshot_agent_protocol;
ALTER TABLE sandboxes DROP COLUMN IF EXISTS snapshot_provenance_id;
