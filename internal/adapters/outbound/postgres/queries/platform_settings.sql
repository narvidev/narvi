-- platform_settings (technical plan §40.2): the deployment's one row of
-- platform-wide settings, id = 1. Its one setting today is the autonomy
-- freeze. Every automatic-action site reads it through GetAutonomyFrozen,
-- per action and with no cache (internal/app/autonomy.Gate); the writes
-- below are the freeze and the unfreeze themselves.

-- name: GetAutonomyFrozen :one
-- Whether autonomy is frozen: one primary-key probe of a one-row table. A
-- missing row reads as never frozen, the column's own default. Run on the
-- pool, or inside a site's own transaction (PlatformSettingsStore.WithTx),
-- where READ COMMITTED sees every freeze committed before the statement
-- starts.
SELECT COALESCE((SELECT autonomy_frozen FROM platform_settings WHERE id = 1), false)::boolean AS autonomy_frozen;

-- name: GetAutonomyFreeze :one
-- The freeze in force, with the freezing administrator's display name
-- (NULL once that user is deleted: autonomy_frozen_by is ON DELETE SET
-- NULL). pgx.ErrNoRows when the row is missing, which reads as not frozen.
SELECT
    p.autonomy_frozen,
    p.autonomy_frozen_at,
    p.autonomy_frozen_by,
    p.autonomy_freeze_reason,
    p.updated_at,
    u.display_name AS autonomy_frozen_by_display_name
FROM platform_settings p
LEFT JOIN users u ON u.id = p.autonomy_frozen_by
WHERE p.id = 1;

-- name: FreezeAutonomy :one
-- Freezes autonomy, recording who (NULL for no user), when and why. Only
-- when autonomy is not frozen already: a second freeze matches no row
-- (pgx.ErrNoRows), and the first freeze's who, when and why are kept as
-- they are. The insert covers a missing row.
INSERT INTO platform_settings (id, autonomy_frozen, autonomy_frozen_at, autonomy_frozen_by, autonomy_freeze_reason, updated_at)
VALUES (1, true, now(), sqlc.narg('frozen_by'), sqlc.arg('reason'), now())
ON CONFLICT (id) DO UPDATE
SET autonomy_frozen = true,
    autonomy_frozen_at = now(),
    autonomy_frozen_by = EXCLUDED.autonomy_frozen_by,
    autonomy_freeze_reason = EXCLUDED.autonomy_freeze_reason,
    updated_at = now()
WHERE NOT platform_settings.autonomy_frozen
RETURNING *;

-- name: UnfreezeAutonomy :one
-- Lifts the freeze in force and returns what was lifted -- its when, who
-- and why -- for the audit row, and how long it held, in whole seconds.
-- Only when autonomy is frozen: pgx.ErrNoRows otherwise. The CTE reads the
-- freeze FOR UPDATE, so of two unfreezes at once the second waits for the
-- first, then reads it no longer frozen and matches no row.
--
-- frozen_seconds is measured on the database's clock, both ends: the
-- freeze stamped autonomy_frozen_at with now(), and this statement
-- subtracts it from its own now(), so a replica's clock never enters the
-- audit row (GetSandboxBootingElapsed's rule). GREATEST guards only a
-- database clock stepped back between the two.
WITH lifted AS (
    SELECT autonomy_frozen_at, autonomy_frozen_by, autonomy_freeze_reason
    FROM platform_settings
    WHERE id = 1 AND autonomy_frozen
    FOR UPDATE
)
UPDATE platform_settings AS p
SET autonomy_frozen = false,
    autonomy_frozen_at = NULL,
    autonomy_frozen_by = NULL,
    autonomy_freeze_reason = NULL,
    updated_at = now()
FROM lifted
WHERE p.id = 1 AND p.autonomy_frozen
RETURNING lifted.autonomy_frozen_at AS frozen_at,
          lifted.autonomy_frozen_by AS frozen_by,
          lifted.autonomy_freeze_reason AS reason,
          GREATEST(0, floor(EXTRACT(EPOCH FROM (now() - lifted.autonomy_frozen_at))))::bigint AS frozen_seconds;
