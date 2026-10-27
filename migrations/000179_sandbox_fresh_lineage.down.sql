-- Reverses the sandbox_fresh_lineage up. With the columns go every
-- sandbox's record of a fresh lineage whose recap has not yet reached it,
-- and every snapshot's record of a recap its gen still owed. A binary
-- without them does not read them; a binary with them that runs the up
-- again reads every gen as unmarked and every snapshot as owing nothing,
-- so a gen marked before the down gets no recap.
--
-- RUN IT WITH THE CONTROL PLANE SCALED TO ZERO, then deploy a binary
-- without it. Do not run it against live pods of a binary that carries it:
-- every query of that binary that returns a whole sandbox row names the
-- columns (sqlc writes each SELECT * and RETURNING * out as a column list),
-- so each fails with SQLSTATE 42703 and no sandbox is spawned or read until
-- the older binary replaces the pod; and a pod that restarts first applies
-- the up again at boot, which locks the older binary out again.
-- Guarded: IF EXISTS, so it also runs when the columns are already gone.
-- The drops are catalog changes: they rewrite nothing, but take ACCESS
-- EXCLUSIVE on sandboxes for the file's one implicit transaction, and touch
-- no other table.
ALTER TABLE sandboxes DROP COLUMN IF EXISTS snapshot_recap_owed_id;
ALTER TABLE sandboxes DROP COLUMN IF EXISTS fresh_lineage_gen;
