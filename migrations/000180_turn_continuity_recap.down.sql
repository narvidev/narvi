-- Reverses the turn_continuity_recap up. With the columns goes every turn's
-- record of the recap its prompt carried, and of the gen it was built for.
-- A binary without them does not read them; a binary with them that runs
-- the up again reads every turn as having carried none, so a receipt
-- re-send of a turn dispatched with a recap before the down omits it.
--
-- RUN IT WITH THE CONTROL PLANE SCALED TO ZERO, then deploy a binary
-- without it. Do not run it against live pods of a binary that carries it:
-- every query of that binary that returns a whole turn row names the
-- columns (sqlc writes each SELECT * and RETURNING * out as a column list),
-- so each fails with SQLSTATE 42703 and no turn is created, dispatched or
-- completed until the older binary replaces the pod; and a pod that
-- restarts first applies the up again at boot, which locks the older
-- binary out again.
-- Guarded: IF EXISTS, so it also runs when the columns are already gone.
-- The drops are catalog changes: they rewrite nothing, but take ACCESS
-- EXCLUSIVE on turns for the file's one implicit transaction, and touch no
-- other table.
ALTER TABLE turns DROP COLUMN IF EXISTS continuity_recap_gen;
ALTER TABLE turns DROP COLUMN IF EXISTS continuity_recap;
