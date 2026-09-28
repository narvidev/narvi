-- Reverse of 000148_sandbox_booting_since.up.sql.
ALTER TABLE sandboxes
    DROP COLUMN IF EXISTS booting_since_gen,
    DROP COLUMN IF EXISTS booting_since;
