DROP INDEX IF EXISTS turns_session_id_dispatched_message_id_idx;
ALTER TABLE turns DROP COLUMN IF EXISTS dispatched_message_id;
