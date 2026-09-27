-- Reverse of 000146_turns_status_changed_at.up.sql.
ALTER TABLE turns DROP COLUMN IF EXISTS status_changed_at;
