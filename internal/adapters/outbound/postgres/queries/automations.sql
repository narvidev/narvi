-- Queries backing AutomationStore ("automations: engine", §3.5;
-- "automations: triggers & extras", §8.4), migrations/
-- 000051_automations.up.sql, migrations/
-- 000055_automations_triggers_and_extras.up.sql.

-- name: CreateAutomation :one
-- §8.4's own real caller: httpapi.CreateAutomation (POST
-- /api/automations). trigger_type/trigger_config/webhook_token_hash back
-- this Step's own condition builder; sandbox_path_scope/
-- sandbox_mock_configured/sandbox_contracts_path back §8.4's own
-- "sandboxSettings honored on automation sessions"; env_vars back
-- §8.4's own "per-automation env vars" (plain config, never a secret --
-- see internal/domain/automation/doc.go's own deferral writeup).
INSERT INTO automations (
    name, prompt, repos, created_by,
    trigger_type, trigger_config, webhook_token_hash,
    sandbox_path_scope, sandbox_mock_configured, sandbox_contracts_path,
    env_vars
)
VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11)
RETURNING *;

-- name: GetAutomation :one
SELECT * FROM automations
WHERE id = $1;

-- name: GetAutomationByWebhookTokenHash :one
-- Backs the webhook trigger's own inbound-auth lookup (internal/adapters/
-- inbound/httpapi's own automationwebhook.go): the presented bearer token
-- is hashed (platform.HashToken, the SAME convention ws_tokens.token_hash
-- already establishes) and looked up directly -- never a table scan
-- comparing a plaintext token against every row, and never comparing an
-- unhashed value against webhook_token_hash.
SELECT * FROM automations
WHERE webhook_token_hash = $1;

-- name: ListAutomations :many
-- Backs GET /api/automations (§8.4's own "creator/status
-- filters"). Both filters are OPTIONAL and independent -- sqlc.narg's own
-- "IS NULL OR" idiom means an absent (nil) filter matches every row, a
-- present one narrows -- so this single query serves "all automations",
-- "my automations" (createdBy set), "only paused" (status set), or both
-- combined, with no dynamic SQL string-building on the Go side. Unbounded
-- (no pagination), matching this codebase's own established "expected to
-- stay small" precedent for a workspace-wide list (ListMembers's own
-- identical shape).
SELECT * FROM automations
WHERE (sqlc.narg('created_by')::uuid IS NULL OR created_by = sqlc.narg('created_by'))
  AND (sqlc.narg('status')::automation_status IS NULL OR status = sqlc.narg('status'))
ORDER BY created_at DESC;

-- name: ListActiveCronAutomations :many
-- Backs the cron trigger pump's own per-tick scan (app/automation's own
-- new triggerpump.go) -- every active, cron-triggered automation, oldest
-- last_cron_fired_at first (NULLS FIRST: an automation that has never
-- fired at all is always evaluated before one that has, on a tie, though
-- in practice each row is evaluated every tick regardless of this
-- ordering -- this just keeps output deterministic for tests).
SELECT * FROM automations
WHERE trigger_type = 'cron' AND status = 'active'
ORDER BY last_cron_fired_at ASC NULLS FIRST;

-- name: ListActiveGitHubAutomations :many
-- Backs the live GitHub webhook dispatch path (§8.4, app/automation's
-- own githubdispatch.go, called inline from internal/adapters/inbound/
-- github's own handler.go) -- every active, github-triggered automation,
-- evaluated against each dispatchable webhook delivery. Mirrors
-- ListActiveCronAutomations' own shape exactly, one row over, ordered by
-- id only for deterministic test output (unlike the cron pump, there is no
-- "last fired" column this trigger type advances).
SELECT * FROM automations
WHERE trigger_type = 'github' AND status = 'active'
ORDER BY id ASC;

-- name: ListActiveLinearAutomations :many
-- The Linear twin of ListActiveGitHubAutomations immediately above --
-- backs app/automation's own lineardispatch.go, called inline from
-- internal/adapters/inbound/linear's own webhook.go.
SELECT * FROM automations
WHERE trigger_type = 'linear' AND status = 'active'
ORDER BY id ASC;

-- name: ClaimCronFire :one
-- The CAS half of the cron trigger pump's own per-automation fire guard:
-- "UPDATE ... WHERE last_cron_fired_at IS NULL OR last_cron_fired_at <
-- <this minute's own start>" -- guards against firing the SAME scheduled
-- minute twice (a slow previous tick, clock jitter, a second pod's own
-- concurrent pump), while still allowing every LATER minute's own match to
-- fire again -- unlike automation_invocations.fanned_out_at's one-way
-- NULL-to-non-NULL flip, this guard is a recurring per-minute bucket, not
-- a permanent latch. $2 is the current UTC minute's own truncated start
-- instant, used both as the new last_cron_fired_at value and as the
-- guard's own comparison point.
UPDATE automations
SET last_cron_fired_at = $2
WHERE id = $1 AND (last_cron_fired_at IS NULL OR last_cron_fired_at < $2)
RETURNING *;

-- name: UpdateAutomationLastRun :one
-- Backs §8.4's own "last_run + artifact_summary populated" -- called by
-- app/automation's own closeout.go, the SAME closeInvocation call site
-- that already resets/increments consecutive_failures, the moment an
-- invocation's own outcome is decided. last_run_status reuses
-- automation_invocation_status verbatim (never a new, parallel taxonomy).
UPDATE automations
SET last_run_at = $2,
    last_run_status = $3,
    artifact_summary = $4,
    updated_at = now()
WHERE id = $1
RETURNING *;

-- name: PauseAutomation :one
-- The manual-admin twin of ResumeAutomation below: backs internal/domain/
-- automation.TriggerAutoPause applied by a direct admin action (POST
-- /api/automations/{id}/pause) rather than the auto-pause path -- automation.
-- go's own doc comment on TriggerAutoPause already anticipates exactly
-- this reuse ("Transition supports the edge regardless, since pausing and
-- auto-pausing land on the identical Status either way"). Guarded by "AND
-- status = 'active'" so a non-active automation's own pause attempt
-- affects zero rows, mirroring ResumeAutomation's own identical "AND
-- status = 'paused'" guard.
UPDATE automations
SET status = 'paused',
    updated_at = now()
WHERE id = $1 AND status = 'active'
RETURNING *;

-- name: LockAutomationForUpdate :one
-- Row-level lock backing the failure-strike accounting step
-- (app/automation's own closeout.go): taken in the SAME transaction as
-- MarkAutomationInvocationFailureCounted (queries/automationinvocations.sql)
-- so two invocations belonging to the SAME automation that close (fail)
-- concurrently apply their own automation.EvaluateFailureStrike
-- consequence one at a time, never as a lost update racing against each
-- other's own read-then-write of consecutive_failures.
SELECT * FROM automations
WHERE id = $1
FOR UPDATE;

-- name: ApplyFailureStrike :one
-- Records automation.EvaluateFailureStrike's own verdict -- called only
-- while still holding LockAutomationForUpdate's own row lock, in the SAME
-- transaction.
UPDATE automations
SET consecutive_failures = $2,
    status = $3,
    updated_at = now()
WHERE id = $1
RETURNING *;

-- name: ResetConsecutiveFailures :execrows
-- A succeeded invocation resets its own automation's streak to zero --
-- idempotent (no CAS guard needed the way the failure path needs one,
-- §3.5's own "at-most-one failure strike" requirement does not apply to a
-- success: applying this reset twice is harmless, unlike double-counting
-- a failure). "AND consecutive_failures <> 0" is a pure no-op-avoidance
-- optimization (skip a write when there is nothing to reset), never a
-- correctness guard.
UPDATE automations
SET consecutive_failures = 0,
    updated_at = now()
WHERE id = $1 AND consecutive_failures <> 0;

-- name: RotateAutomationWebhookToken :one
-- Review fix ("webhook token has no rotation/revocation/expiry"): backs
-- POST /api/automations/{automationID}/webhook-token. Mints a FRESH
-- webhook_token_hash (the caller has already generated a new plaintext
-- token via platform.GenerateToken()/HashToken(), the SAME mint step
-- CreateAutomation itself already runs) and overwrites the old one
-- outright -- the old hash stops matching GetAutomationByWebhookTokenHash
-- the instant this commits, with no grace period. Guarded by "AND
-- trigger_type = 'webhook'" so rotating a token on a non-webhook
-- automation (which never had a webhook_token_hash to begin with) affects
-- zero rows rather than silently writing a hash column that
-- GetAutomationByWebhookTokenHash's own trigger_type check
-- (automationwebhook/handler.go) would never actually honor anyway.
UPDATE automations
SET webhook_token_hash = $2,
    updated_at = now()
WHERE id = $1 AND trigger_type = 'webhook'
RETURNING *;

-- name: RevokeAutomationWebhookToken :one
-- Review fix, same finding as RotateAutomationWebhookToken above -- backs
-- DELETE /api/automations/{automationID}/webhook-token. Clears
-- webhook_token_hash to NULL unconditionally (no trigger_type guard,
-- unlike rotate): clearing an already-NULL hash on a non-webhook
-- automation is a harmless no-op, and the caller (httpapi.
-- RevokeAutomationWebhookToken) already runs its own existence check
-- first, mirroring Pause/Resume's identical "existence check, then a
-- guarded/unconditional single-row UPDATE" shape. Once this commits, the
-- automation has NO webhook_token_hash at all -- every future call to its
-- own inbound webhook endpoint 401s (GetAutomationByWebhookTokenHash
-- simply never matches a NULL-hash row, since that lookup only ever
-- searches by a hashed VALUE, never by presence/absence) until a
-- subsequent RotateAutomationWebhookToken mints a new one.
UPDATE automations
SET webhook_token_hash = NULL,
    updated_at = now()
WHERE id = $1
RETURNING *;

-- name: ResumeAutomation :one
-- Backs automation.TriggerResume (internal/domain/automation) -- no HTTP
-- caller exists yet in this Step (§8.4/§10 own the actual "Resume"
-- button, mockups.html's own Automations view), reserved so that surface
-- needs no store-layer change to use it. Guarded by "AND status =
-- 'paused'" so a non-paused automation's own resume attempt affects zero
-- rows rather than silently no-op-writing an already-active row.
UPDATE automations
SET status = 'active',
    consecutive_failures = 0,
    updated_at = now()
WHERE id = $1 AND status = 'paused'
RETURNING *;
