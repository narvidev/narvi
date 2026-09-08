-- Queries backing AutomationRunStore ("automations: engine",
-- §3.5), migrations/000053_automation_runs.up.sql.

-- name: CreateAutomationRun :one
-- sessionID is sqlc.narg (nullable): set when this run's own
-- CreateSessionOnTx call (app/automation's own fanout.go) succeeded for
-- its target, NULL when it failed before any session ever existed (the
-- run is still created either way, RunStatusStarting or RunStatusFailed
-- respectively -- every target names exactly one row, regardless of
-- outcome, so total_runs on the parent invocation always matches the real
-- row count).
INSERT INTO automation_runs (invocation_id, automation_id, target, session_id, status)
VALUES ($1, $2, $3, sqlc.narg('session_id'), $4)
RETURNING *;

-- name: CreateAutomationRunIfAbsent :one
-- The IDEMPOTENT counterpart to CreateAutomationRun immediately above --
-- app/automation's own createFailedRun (fanout.go) uses this one
-- exclusively, never the plain INSERT, because createFailedRun is also
-- the recovery path for createRunAndSession's own AMBIGUOUS tx.Commit
-- failure (Postgres may have committed server-side despite the client
-- observing an error, so a run row for this exact target may already
-- exist). "ON CONFLICT (invocation_id, (target::text)) DO NOTHING",
-- backed by automation_runs_invocation_target_uniq (migrations/000054),
-- makes retrying safe: RETURNING then yields zero rows -- surfaced to the
-- caller as pgx.ErrNoRows, the SAME "lost the race, harmless no-op" idiom
-- every other CAS-guarded write in this package already treats
-- identically (this file's own ListInFlightRuns/TerminalizeAutomationRun
-- doc comments).
INSERT INTO automation_runs (invocation_id, automation_id, target, session_id, status)
VALUES ($1, $2, $3, sqlc.narg('session_id'), $4)
ON CONFLICT (invocation_id, (target::text)) DO NOTHING
RETURNING *;

-- name: GetAutomationRun :one
SELECT * FROM automation_runs
WHERE id = $1;

-- name: ListInFlightRuns :many
-- Every run still starting/running, oldest first, bounded by limit --
-- app/automation's own reconcile pump (ReconcileOnce). A plain SELECT, not
-- FOR UPDATE SKIP LOCKED: the actual state-changing writes below
-- (TerminalizeAutomationRun/PromoteAutomationRunToRunning) are each their
-- own CAS-guarded UPDATE, so two concurrent readers observing the SAME
-- row here is harmless -- at most one of them ever wins the later write
-- (mirrors app/reconciler.ReconcileOnce's own identical "the read is
-- plain, only the write is race-guarded" reasoning for provider.List/
-- ListLiveProviderIDs).
SELECT * FROM automation_runs
WHERE status IN ('starting', 'running')
ORDER BY created_at
LIMIT $1;

-- name: PromoteAutomationRunToRunning :one
-- automation.RunTriggerProcessing: Starting -> Running. Guarded by "AND
-- status = 'starting'" -- a losing/duplicate promotion attempt (this run
-- already promoted by a concurrent reconcile tick) affects zero rows,
-- observed via pgx.ErrNoRows, never a partial/duplicate write.
UPDATE automation_runs
SET status = 'running',
    running_at = now()
WHERE id = $1 AND status = 'starting'
RETURNING *;

-- name: TerminalizeAutomationRun :one
-- Applies internal/domain/automation.RunTransition's own verdict (to is
-- always RunStatusSucceeded/RunStatusFailed) -- guarded by "AND status IN
-- ('starting', 'running')" (a non-terminal-only guard, not an exact
-- single-status match: see this file's own top doc comment on
-- ListInFlightRuns for why a status change between this run's own SELECT
-- and this UPDATE is harmless here -- the terminal status itself is
-- always freshly re-derived from the linked session's CURRENT turn
-- history, never a stale in-memory value). Used by BOTH the reconcile
-- pump (a genuine turn outcome) and the recovery sweep (an orphan
-- timeout) -- the SAME guard correctly excludes an already-terminal run
-- from either caller.
UPDATE automation_runs
SET status = $2,
    completed_at = now()
WHERE id = $1 AND status IN ('starting', 'running')
RETURNING *;

-- name: CountTerminalRunsForInvocation :one
-- Feeds automation.EvaluateInvocationOutcome (app/automation's own
-- closeout.go): how many of this invocation's own runs are terminal, and
-- how many of those specifically failed. total_runs itself is already
-- persisted on automation_invocations (no need to recompute it here).
SELECT
    count(*) FILTER (WHERE status IN ('succeeded', 'failed')) AS terminal_runs,
    count(*) FILTER (WHERE status = 'failed') AS failed_runs
FROM automation_runs
WHERE invocation_id = $1;

-- name: ListOrphanedStartingRuns :many
-- §3.5's own "orphaned starting runs >5 min" sweep -- started_at older
-- than cutoff (platform.Timeouts.AutomationRunStartingOrphanThreshold
-- subtracted from "now" by the caller, mirroring app/imagebuild.
-- RefreshOnce's own staleClaimCutoff precedent: one cutoff instant computed
-- ONCE per tick, not a fresh now() per row).
SELECT * FROM automation_runs
WHERE status = 'starting' AND started_at < $1
ORDER BY started_at
LIMIT $2;

-- name: ListRunsForInvocation :many
-- Backs §8.4's own "artifact_summary populated" -- app/automation's own
-- closeout.go reads every run of a just-closed invocation (small, ≤
-- automation.MaxFanOutTargets rows) to name which specific targets failed
-- (automation.BuildArtifactSummary), reusing app/automation's own
-- unmarshalTargets (target.go) against each row's own target JSONB rather
-- than a second, independent JSON decode.
SELECT * FROM automation_runs
WHERE invocation_id = $1
ORDER BY created_at;

-- name: ListOrphanedRunningRuns :many
-- §3.5's own "running >90 min" sweep -- same shape as
-- ListOrphanedStartingRuns immediately above, against running_at/its own
-- distinct cutoff.
SELECT * FROM automation_runs
WHERE status = 'running' AND running_at < $1
ORDER BY running_at
LIMIT $2;

-- name: ListRunHealthForAutomations :many
-- Backs §12.2 item 4's own health-column ratio ("12/12 ok", "47/48 ok")
-- and §8.4's own named gap ("Automation carries no aggregate; an honest
-- ratio is a COUNT over automation_runs") -- computed PER REQUEST from
-- this table directly, never persisted onto automations as a running
-- counter: automation_runs.automation_id was denormalized from
-- automation_invocations.automation_id back in migrations/
-- 000053_automation_runs.up.sql specifically "so a per-automation health
-- rollup... never has to join through automation_invocations at all" --
-- this query is that rollup, and Postgres remains the single source of
-- truth (§5.1) for a fact a plain aggregate query already answers
-- correctly and cheaply (automation_runs_health_idx below), rather than
-- a second, independently-written counter that could drift from the rows
-- it summarizes.
--
-- ALL-TIME per automation (every terminal run ever, not a trailing
-- window) -- the plan's own gap description names no window, and this is
-- the same "cumulative, not windowed" shape CountTerminalRunsForInvocation
-- above already uses one level down (per invocation instead of per
-- automation). One row per automation_id that has AT LEAST ONE terminal
-- run; an automation with zero terminal runs simply does not appear in
-- the result (the caller's own job to render that as "no runs yet" rather
-- than a fabricated 0/0).
SELECT
    automation_id,
    count(*) FILTER (WHERE status = 'succeeded') AS succeeded_runs,
    count(*) FILTER (WHERE status IN ('succeeded', 'failed')) AS terminal_runs
FROM automation_runs
WHERE automation_id = ANY(sqlc.arg('automation_ids')::uuid[])
  AND status IN ('succeeded', 'failed')
GROUP BY automation_id;
