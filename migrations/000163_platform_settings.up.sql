-- platform_settings: the deployment's platform-wide settings, one row
-- (technical plan §40.2). Its one setting today is the autonomy freeze:
-- while autonomy_frozen is true, no automatic action starts -- no
-- auto-merge, no sentinel-fix merge, no sentinel auto-fix spawn, no
-- description rewrite, no automatic re-review, no cron fire and no
-- automation fan-out. A person's own commands are never read against it.
--
-- A row, never an environment variable: a freeze is one write that every
-- replica sees on its next read, with no restart. Every site reads it per
-- action, with no cache (§5.1: "no cache with authority" -- a cached
-- "not frozen" would let a merge through after the freeze committed), as
-- one primary-key probe of this one-row table:
--   SELECT COALESCE((SELECT autonomy_frozen FROM platform_settings
--                    WHERE id = 1), false);
-- so a missing row reads as never frozen. A read that fails is a skip,
-- never a pass (internal/app/autonomy.Gate).
--
-- The freeze in force carries its who, when and why: all three are set
-- while autonomy_frozen is true and all are NULL while it is false
-- (platform_settings_freeze_shape). autonomy_frozen_by becomes NULL if
-- the administrator's user row is deleted; the reason stays. The history
-- of freezes lives in audit_log (autonomy.frozen, autonomy.unfrozen), not
-- here. Nothing in this release writes the row: every site reads it, so
-- the action that freezes finds every replica already honouring it.
--
-- ONE row, ALWAYS id = 1, seeded here, as review_check_writer_app_id's
-- singleton is shaped (migrations/000134).
--
-- To read the freeze in force:
--   SELECT autonomy_frozen, autonomy_frozen_at, autonomy_frozen_by,
--          autonomy_freeze_reason FROM platform_settings WHERE id = 1;
--
-- IF NOT EXISTS and ON CONFLICT DO NOTHING: see "Rolling back" -- a
-- rollback that keeps the table leaves it in place, with its row, when
-- this file runs again.
--
-- # Locks
--
-- The create rewrites nothing, but its foreign key (autonomy_frozen_by
-- REFERENCES users) adds referential triggers to users, so the statement
-- takes SHARE ROW EXCLUSIVE on users for the file's one implicit
-- transaction -- an instant once granted. It conflicts with any row write:
-- the migration waits behind an open transaction that has written to users
-- (a sign-in creating a user, a role change), and while it waits, new
-- writes to users queue behind it. Reads of users are not blocked.
-- controlplane/migrate.go sets no lock_timeout, so a long transaction on
-- users holds the boot's migration until it ends. The seed insert writes
-- one row to the new table.
--
-- # Rolling deploy
--
-- The previous binary never names this table, and nothing in this release
-- writes it: every replica of either binary behaves as before, unfrozen.
--
-- # Rolling back
--
-- Every control-plane boot runs the embedded migrations up
-- (controlplane/migrate.go), and golang-migrate refuses a database whose
-- version it has no file for. So once this migration is applied, the
-- previous binary cannot boot ("no migration found for version N", N being
-- this file's own number). A rollback therefore takes one of two steps
-- first, with the control plane scaled to zero, V being the version before
-- this file's:
--   - Keep the row: with the golang-migrate CLI, `migrate force V`. The
--     previous binary then boots and never reads it; when this release is
--     deployed again, this file runs again and leaves the table and its
--     row as they are.
--   - Drop it: run this migration's down (`migrate goto V`) with this
--     release's migrations.
-- Either way, A FREEZE IN FORCE IS LIFTED under the previous binary: it
-- reads no freeze, so every automatic action starts again, whatever the
-- row says. Turn off auto-merge and the other automatic toggles per
-- repository, and pause automations, before rolling back during an
-- incident.
CREATE TABLE IF NOT EXISTS platform_settings (
    id                     SMALLINT PRIMARY KEY DEFAULT 1
        CONSTRAINT platform_settings_singleton CHECK (id = 1),
    autonomy_frozen        BOOLEAN NOT NULL DEFAULT false,
    autonomy_frozen_at     TIMESTAMPTZ,
    autonomy_frozen_by     UUID REFERENCES users(id) ON DELETE SET NULL,
    autonomy_freeze_reason TEXT,
    updated_at             TIMESTAMPTZ NOT NULL DEFAULT now(),
    CONSTRAINT platform_settings_freeze_shape CHECK (
        (autonomy_frozen
            AND autonomy_frozen_at IS NOT NULL
            AND autonomy_freeze_reason IS NOT NULL
            AND char_length(btrim(autonomy_freeze_reason)) BETWEEN 1 AND 500)
        OR (NOT autonomy_frozen
            AND autonomy_frozen_at IS NULL
            AND autonomy_frozen_by IS NULL
            AND autonomy_freeze_reason IS NULL))
);

INSERT INTO platform_settings (id) VALUES (1) ON CONFLICT (id) DO NOTHING;
