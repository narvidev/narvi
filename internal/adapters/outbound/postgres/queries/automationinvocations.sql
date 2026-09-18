-- Queries backing AutomationInvocationStore ("automations:
-- engine", §3.5), migrations/000052_automation_invocations.up.sql.

-- name: CreateAutomationInvocation :one
-- Fast, cheap, durable hand-off (mirrors internal/app/releasereview.
-- Enqueue's own identical "one INSERT, the real work happens later on a
-- background loop's own schedule" shape) -- the caller (this Step: tests
-- only; §8.4: a real trigger-condition evaluation) has already run
-- automation.ValidateTargets against targets before calling this.
INSERT INTO automation_invocations (automation_id, targets, total_runs)
VALUES ($1, $2, $3)
RETURNING *;

-- name: GetAutomationInvocation :one
SELECT * FROM automation_invocations
WHERE id = $1;

-- name: CreateAutomationInvocationForDelivery :one
-- D1 audit fix's own idempotent-on-delivery variant of
-- CreateAutomationInvocation above -- used ONLY by live GitHub/Linear
-- webhook dispatch (app/automation's own githubdispatch.go/
-- lineardispatch.go), which alone has a genuine (provider, delivery_id)
-- identity to key on. The SAME "(xmax = 0) AS inserted" idiom
-- ClaimWebhookDelivery already establishes (queries/webhookdeliveries.sql)
-- -- a deliberate, self-referential no-op update (automation_id is set
-- back to its own current value) on conflict against
-- automation_invocations_source_delivery_uniq (migrations/
-- 000135_automation_invocations_source_delivery.up.sql), so RETURNING
-- always yields exactly one row whether this call just inserted a fresh
-- invocation or found an already-created one from an earlier delivery of
-- the SAME (automation_id, provider, delivery_id) -- a real webhook
-- redelivery. Callers branch on Inserted: true means "a new invocation
-- now exists, proceed exactly like CreateAutomationInvocation always has";
-- false means "already dispatched for this exact delivery -- skip,
-- never double-fire this automation for a resend".
INSERT INTO automation_invocations (automation_id, targets, total_runs, source_provider, source_delivery_id)
VALUES ($1, $2, $3, $4, $5)
ON CONFLICT (automation_id, source_provider, source_delivery_id) WHERE source_provider IS NOT NULL AND source_delivery_id IS NOT NULL
DO UPDATE SET automation_id = automation_invocations.automation_id
RETURNING *, (xmax = 0) AS inserted;

-- name: CountRecentAutomationInvocations :one
-- Backs D8's own per-automation dispatch throttle
-- (domainautomation.EvaluateDispatchThrottle) -- every invocation this
-- automation has created (any source, any outcome) since $2, regardless
-- of whether it has fanned out or closed yet.
SELECT count(*) FROM automation_invocations
WHERE automation_id = $1 AND created_at >= $2;

-- name: ListDueForFanOut :many
-- Every invocation not yet claimed for fan-out, oldest first, locked FOR
-- UPDATE SKIP LOCKED (of automation_invocations only -- "FOR UPDATE OF ai"
-- -- the join below reads automations.status but never needs to lock that
-- row) -- callers MUST run this inside the same transaction that
-- subsequently calls ClaimAutomationInvocationForFanOut on each returned
-- row, mirroring ListDueImageBuilds/ListDuePendingOutbox's own identical
-- claim-batch precedent exactly.
--
-- "AND a.status = 'active'" is this Step's own defense-in-depth against a
-- pending invocation surviving its own automation's auto-pause (§3.5): an
-- invocation already created (e.g. moments before a THIRD consecutive
-- failure elsewhere auto-pauses this same automation) is simply left
-- un-fanned-out -- still visible to this same query on a LATER tick, once
-- either the automation is resumed (§8.4/§10's own future surface) or
-- never, if it stays paused -- rather than dispatching real sessions for
-- an automation the engine itself just decided to stop trusting. §8.4's
-- own future trigger evaluator is expected to check this same status
-- before ever calling CreateInvocation in the first place; this is the
-- SECOND, independent layer, for an invocation already queued before that
-- decision landed.
SELECT ai.* FROM automation_invocations ai
JOIN automations a ON a.id = ai.automation_id
WHERE ai.fanned_out_at IS NULL AND a.status = 'active'
ORDER BY ai.created_at
LIMIT $1
FOR UPDATE OF ai SKIP LOCKED;

-- name: ClaimAutomationInvocationForFanOut :one
-- The CAS half of the claim-batch pair immediately above -- "UPDATE ...
-- WHERE fanned_out_at IS NULL" guards against a crash-and-retry
-- double-fanning-out the SAME invocation (see this table's own migration
-- doc comment).
UPDATE automation_invocations
SET fanned_out_at = now()
WHERE id = $1 AND fanned_out_at IS NULL
RETURNING *;

-- name: CloseAutomationInvocation :one
-- Applies internal/domain/automation.InvocationTransition's own verdict --
-- guarded by "AND status = 'pending'" so this invocation's own outcome is
-- decided at most once, no matter how many concurrent callers (this
-- Step's reconcile pump, the recovery sweep, or -- in principle -- both,
-- racing on the SAME invocation's last remaining run) observe "every run
-- is now terminal" at roughly the same time; exactly one of them ever
-- wins this UPDATE (returns pgx.ErrNoRows to every loser, app/automation's
-- own closeout.go).
UPDATE automation_invocations
SET status = $2,
    closed_at = now()
WHERE id = $1 AND status = 'pending'
RETURNING *;

-- name: ListInvocationsForAutomation :many
-- Backs GET /api/automations/{automationID}/invocations (the automations UI,
-- mockups.html's own "expandable invocation -> runs rows"), sourced from
-- the automation_invocations_automation_id_idx index this table's own
-- migration comment already named as backing "the future ... read model
-- the mockups' own '12/12 ok' / 'n/3 strikes' health column will need".
-- Newest first, bounded by $2 -- mirrors ListPlansForSession's own "a
-- session's own plan history is expected to stay small" precedent one
-- level up: this is an automation's own MOST RECENT invocation history for
-- the UI's own expandable table, never an unbounded full archive.
SELECT * FROM automation_invocations
WHERE automation_id = $1
ORDER BY created_at DESC
LIMIT $2;

-- name: MarkAutomationInvocationFailureCounted :one
-- §3.5's own literal CAS idiom: "UPDATE ... WHERE failure_counted_at IS
-- NULL" -- called only for an invocation CloseAutomationInvocation just
-- decided is Failed, in the SAME transaction as
-- LockAutomationForUpdate/ApplyFailureStrike (queries/automations.sql),
-- so the guard and its consequence commit or roll back together
-- atomically -- guarding the failure-strike CONSEQUENCE against being
-- applied twice even if this invocation's own close-out is somehow
-- re-attempted after a crash between CloseAutomationInvocation committing
-- and this transaction committing (internal/domain/automation/doc.go's own
-- "Closing an invocation vs. recording its failure-strike consequence"
-- section).
UPDATE automation_invocations
SET failure_counted_at = now()
WHERE id = $1 AND failure_counted_at IS NULL
RETURNING *;
