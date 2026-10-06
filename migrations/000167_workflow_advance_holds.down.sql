-- Reverses this migration's up file. Every run whose advance the autonomy
-- freeze holds is cancelled first -- the one kind of candidate a rollback
-- loses, stated rather than left stranded: without it, a binary that does
-- not know the holds would leave each such run running with no live step
-- run, every turn on its session passing through untracked and no new run
-- starting on it. Its finished attempt keeps its status and outcome; the
-- run ends cancelled, as a person's stop ends one, and no notice is sent.
-- Then the table goes. Record the held advances first:
--   SELECT workflow_run_id, step_run_id, session_id, held_at
--   FROM workflow_advance_holds ORDER BY held_at;
--
-- RUN IT WITH THE CONTROL PLANE SCALED TO ZERO, then deploy a binary
-- without this migration. Do not run it against live pods of a binary that
-- carries it, for two reasons:
--   - That binary's workflow engine writes and reads this table. After this
--     down, a tracked attempt that ends while autonomy is frozen fails its
--     hold's insert (SQLSTATE 42P01, relation does not exist), which aborts
--     the transaction ending the turn, so the turn's end is retried until
--     the older binary replaces the pod.
--   - A pod that carries this migration and restarts before the older
--     binary replaces it applies it again at boot, which locks the older
--     binary out again ("no migration found for version N", N being this
--     file's own number).
-- Guarded: the cancel runs only while the table exists, and the drop is
-- IF EXISTS, so this file also runs on a database a rollback already
-- brought back to the version before this one with the table kept
-- (`migrate force`, the up file's "Rolling back") and then forced up again.
-- Locks: the cancel takes ROW EXCLUSIVE on workflow_runs. Dropping the
-- table drops its foreign keys' triggers on workflow_runs,
-- workflow_step_runs and sessions, so the drop takes ACCESS EXCLUSIVE on
-- each -- reads included -- for the file's one implicit transaction, an
-- instant once granted, waiting behind any open transaction that touched
-- them. With the control plane scaled to zero, as above, nothing is
-- waiting.
DO $$
BEGIN
    IF to_regclass('workflow_advance_holds') IS NOT NULL THEN
        UPDATE workflow_runs
        SET status = 'cancelled', finished_at = now(), updated_at = now()
        WHERE status = 'running'
          AND id IN (SELECT workflow_run_id FROM workflow_advance_holds);
    END IF;
END
$$;
DROP TABLE IF EXISTS workflow_advance_holds;
