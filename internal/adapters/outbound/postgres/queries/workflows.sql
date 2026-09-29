-- Queries backing WorkflowStore ("workflow execution engine",
-- §25.6/§25.7/§25.8), the first real reader/writer of §25.4's own dark
-- schema (migrations/000057_workflows.up.sql). internal/app/workflowengine
-- is this file's only caller: it resolves which (lane, repo) binding and
-- WorkflowDefinition govern a session's current turn, tracks the run/
-- step-run ledger these queries persist, and consults
-- internal/domain/workflow.NextStep on every posted/derived step outcome
-- -- never reimplementing that decision here.
--
-- Binding resolution (§25.4): a repo-specific row (GetWorkflowBindingForRepo)
-- always wins when one exists; GetGlobalWorkflowBinding is the guaranteed
-- fallback (seeded by migration 000057 for every lane, so this SELECT is
-- never truly expected to return zero rows in a correctly-migrated
-- database -- see workflowengine's own fail-open handling for the
-- defensive case where it somehow does).
--
-- Definition assembly (GetWorkflowDefinition + ListWorkflowStepDefinitions +
-- ListWorkflowEdgesForDefinition): three queries, not one join, mirroring
-- domain/workflow.Definition's own shape (a definition row plus its
-- ordered steps, each carrying its own outgoing edges) -- workflowengine
-- groups edges by from_step_id in Go when assembling the domain value, the
-- same "read plain rows, assemble the domain type in the app layer" split
-- every other store in this package already follows (e.g. PlanStore never
-- constructs an internal/domain/plan value itself).
--
-- Run/step-run lifecycle (§25.6, migration 000057's own header comment):
-- GetRunningWorkflowRunForSession/GetLiveWorkflowStepRunForRun back the
-- "is there already a live run/step for this session" check
-- ResolveStepForNewTurn makes before ever starting a new run --
-- workflow_runs_one_running_per_session/workflow_step_runs_one_live_per_run
-- (migration 000057) make violating either a live constraint violation,
-- not just an application bug; this store's own CreateWorkflowRun/
-- CreateWorkflowStepRun are therefore only ever called from the branch that
-- already confirmed no live one exists. AttachTurnToWorkflowStepRun
-- backfills turn_id once the actual turn row exists (it cannot exist before
-- the step-run row is created, since ResolveStepForNewTurn must decide the
-- step BEFORE the turn insert it feeds prompt/model into) --
-- GetLiveWorkflowStepRunByTurnID is completion.go's own reverse lookup, at
-- turn-completion time.
--
-- MarkWorkflowStepRunAwaitingDecision/FinishWorkflowStepRun/
-- CompleteWorkflowRun/EscalateWorkflowRun are deliberately four narrow,
-- single-purpose statements rather than one CASE-parameterized UPDATE each
-- -- mirroring plans.sql's own ApprovePlanIfAwaitingApproval/
-- RejectPlanIfAwaitingApproval precedent (two verdicts, two statements) --
-- so finished_at's own "null while awaiting_decision" invariant
-- (migration 000057's own doc comment) is enforced by which statement runs,
-- never by a conditional expression a future edit could silently get wrong.
-- outcome_status uses COALESCE in both step-run UPDATEs so an outcome
-- ALREADY posted via the step-outcome tool (SetWorkflowStepRunOutcome
-- below) is never clobbered by the engine's own implicit-from-trigger
-- derivation at turn-completion time -- the posted value always wins.
--
-- SetWorkflowStepRunOutcome is the generic step-outcome-posting tool's own
-- write (§25.6's typed {status, summary, structuredPayload} handoff,
-- structurally mirroring internal/domain/reviewpost's existing
-- verdict-posting shape): guarded to "AND status = 'running'" so a caller
-- can only ever post onto the attempt currently actually in flight, never
-- an already-finished or awaiting-decision one -- :execrows lets the
-- handler distinguish "posted" from "no live attempt to post onto" without
-- a separate existence check.

-- name: GetWorkflowBindingForRepo :one
SELECT * FROM workflow_bindings WHERE lane = $1 AND repo_full_name = $2;

-- name: GetGlobalWorkflowBinding :one
SELECT * FROM workflow_bindings WHERE lane = $1 AND repo_full_name IS NULL;

-- name: LockWorkflowDefinitionForUpdate :one
-- Row-level lock making §25.11's own "a bound definition is never edited"
-- refusal actually hold, rather than hold only because callers happen not
-- to interleave. Taken in the SAME transaction as the bound/run-history
-- EXISTS checks and the step rewrite that follows them, and taken again by
-- the binding upsert (§25.10's PUT /api/workflow-bindings) before it
-- creates a binding -- so a definition cannot acquire a binding in the
-- window between a PUT's refusal check and its own COMMIT. Without it the
-- refusal is a read-then-write: the EXISTS sees no binding, an admin
-- activates the definition, and the edit lands on a now-bound definition,
-- which is exactly the past-the-admin-gate dispatch change the refusal
-- exists to prevent. Mirrors LockAutomationForUpdate (queries/
-- automations.sql), taken for the identical read-check-then-write shape.
--
-- Serialising the two writers also fixes a second, quieter race for free:
-- two concurrent PUTs on the same definition each deleted only the steps
-- visible in their own snapshot, so the "complete desired state" replace
-- could merge two step sets instead of replacing one.
SELECT * FROM workflow_definitions WHERE id = $1 FOR UPDATE;

-- name: GetWorkflowDefinition :one
SELECT * FROM workflow_definitions WHERE id = $1;

-- name: ListWorkflowStepDefinitions :many
SELECT * FROM workflow_step_definitions WHERE workflow_definition_id = $1 ORDER BY step_order;

-- name: ListWorkflowEdgesForDefinition :many
SELECT * FROM workflow_edges WHERE workflow_definition_id = $1;

-- name: GetRunningWorkflowRunForSession :one
SELECT * FROM workflow_runs WHERE session_id = $1 AND status = 'running';

-- name: GetWorkflowRun :one
SELECT * FROM workflow_runs WHERE id = $1;

-- name: CreateWorkflowRun :one
INSERT INTO workflow_runs (session_id, lane, workflow_definition_id, definition_version)
VALUES ($1, $2, $3, $4)
RETURNING *;

-- name: GetLiveWorkflowStepRunForRun :one
SELECT * FROM workflow_step_runs
WHERE workflow_run_id = $1 AND status IN ('running', 'awaiting_decision');

-- name: GetLiveWorkflowStepRunByTurnID :one
SELECT * FROM workflow_step_runs WHERE turn_id = $1 AND status = 'running';

-- name: CreateWorkflowStepRun :one
INSERT INTO workflow_step_runs (workflow_run_id, step_definition_id)
VALUES ($1, $2)
RETURNING *;

-- name: AttachTurnToWorkflowStepRun :exec
UPDATE workflow_step_runs SET turn_id = $2, updated_at = now() WHERE id = $1;

-- name: MarkWorkflowStepRunAwaitingDecision :one
UPDATE workflow_step_runs
SET status = 'awaiting_decision', outcome_status = COALESCE(outcome_status, $2), updated_at = now()
WHERE id = $1
RETURNING *;

-- name: FinishWorkflowStepRun :one
-- status is the caller-computed terminal workflow_step_run_status
-- (completed/failed/cancelled -- never awaiting_decision, which
-- MarkWorkflowStepRunAwaitingDecision above owns exclusively).
UPDATE workflow_step_runs
SET status = $2, outcome_status = COALESCE(outcome_status, $3), finished_at = now(), updated_at = now()
WHERE id = $1
RETURNING *;

-- name: CompleteWorkflowRun :one
UPDATE workflow_runs SET status = 'completed', finished_at = now(), updated_at = now() WHERE id = $1 RETURNING *;

-- name: FailWorkflowRun :one
-- §25.9's own addition (§25.9): a winning HITL reject verdict ends the
-- run -- mirrors CompleteWorkflowRun's own shape exactly, landing on
-- 'failed' instead of 'completed' (WorkflowStepDecideResponse.runStatus's
-- own documented "'failed' after a winning reject ends the run" example).
-- Terminal (finished_at set), unlike EscalateWorkflowRun's own
-- 'needs_review' (non-terminal, waiting on a human) -- a reject is itself
-- the human's own final word, nothing further to wait on.
UPDATE workflow_runs SET status = 'failed', finished_at = now(), updated_at = now() WHERE id = $1 RETURNING *;

-- name: EscalateWorkflowRun :one
UPDATE workflow_runs SET status = 'needs_review', updated_at = now() WHERE id = $1 RETURNING *;

-- name: CancelWorkflowRun :one
-- Technical plan §3.3's stop: an attempt whose turn a person's stop
-- cancelled ends its run here, terminal like FailWorkflowRun, and
-- workflow.NextStep is never consulted -- a custom definition's edge on
-- 'blocked' would otherwise queue the next step, and the stop would not
-- stop.
UPDATE workflow_runs SET status = 'cancelled', finished_at = now(), updated_at = now() WHERE id = $1 RETURNING *;

-- name: SetWorkflowStepRunOutcome :execrows
UPDATE workflow_step_runs
SET outcome_status = $2, outcome_summary = $3, outcome_payload = $4, updated_at = now()
WHERE id = $1 AND status = 'running';

-- §25.9 ("workflow HITL gate + circuit breaker", §25.9) own additions
-- below: the decide endpoint's lookup/guarded-decision queries, the
-- circuit breaker's own COUNT(*) attempt read (§25.5), and the escalation
-- notice's one-time claim (migrations/000058_workflow_hitl.up.sql).

-- name: GetWorkflowStepRun :one
-- Plain lookup by id -- the decide endpoint's own first read (mirrors
-- plans.Get's identical role in decideplan.go): used both to resolve the
-- target attempt before deciding it and, after the guarded UPDATE below,
-- to re-fetch its REAL current state either way (won or already decided),
-- exactly like DecidePlanOnTx's own plans.Get re-fetch.
SELECT * FROM workflow_step_runs WHERE id = $1;

-- name: CountWorkflowStepRunsForStepDefinition :one
-- §25.5's own "iteration count reads COUNT(*) on workflow_step_runs, no
-- dedicated counter column" -- scoped to ONE run (a count is never summed
-- across a session's separate runs) and ONE step definition (the step
-- about to receive a new attempt). The engine consults this ONLY when a
-- needs_fix edge is about to advance to a step that might already have
-- prior attempts in THIS run -- see internal/app/workflowengine's own
-- advance.go for the exact call site and the "count > 0 means a genuine
-- re-fire" reasoning.
SELECT COUNT(*) FROM workflow_step_runs WHERE workflow_run_id = $1 AND step_definition_id = $2;

-- DecideWorkflowStepRunApprove/Reject/Revise are three narrow,
-- single-purpose guarded UPDATEs -- mirroring plans.sql's own
-- ApprovePlanIfAwaitingApproval/RejectPlanIfAwaitingApproval precedent
-- (two verdicts, two statements) exactly, extended to three here so which
-- resulting status/decision applies to which verdict is enforced by WHICH
-- STATEMENT RUNS, never by a conditional expression a future edit could
-- silently get wrong (this file's own header comment states this
-- discipline for MarkWorkflowStepRunAwaitingDecision/FinishWorkflowStepRun
-- above; the same reasoning applies here). All three share the identical
-- "AND status = 'awaiting_decision'" guard -- :execrows lets the handler
-- distinguish "this call won the decision" from "already decided (or a
-- stale id)" without a separate existence check, exactly like
-- ApprovePlanIfAwaitingApproval's own :execrows shape. decisionText ($2)
-- is nullable: NULL for approve (ignored), optional context for reject,
-- required non-empty for revise (enforced at the application layer, per
-- WorkflowStepDecideRequest.text's own schema doc comment).
--
-- Resulting status: approve/revise both land on 'completed' (the
-- attempt's own execution genuinely finished either way -- what happens
-- NEXT, advance vs re-execute, is the decision column's own job, not the
-- status column's); reject lands on 'failed', mirroring
-- WorkflowStepDecideResponse.runStatus's own documented "'failed' after a
-- winning reject ends the run" example -- the owning run's and the
-- decided attempt's own resulting status agree by construction.

-- name: DecideWorkflowStepRunApprove :execrows
UPDATE workflow_step_runs
SET status = 'completed', decision = 'approve', decision_text = $2, decided_at = now(), decided_by = $3, finished_at = now(), updated_at = now()
WHERE id = $1 AND status = 'awaiting_decision';

-- name: DecideWorkflowStepRunReject :execrows
UPDATE workflow_step_runs
SET status = 'failed', decision = 'reject', decision_text = $2, decided_at = now(), decided_by = $3, finished_at = now(), updated_at = now()
WHERE id = $1 AND status = 'awaiting_decision';

-- name: DecideWorkflowStepRunRevise :execrows
UPDATE workflow_step_runs
SET status = 'completed', decision = 'revise', decision_text = $2, decided_at = now(), decided_by = $3, finished_at = now(), updated_at = now()
WHERE id = $1 AND status = 'awaiting_decision';

-- name: ClaimWorkflowRunEscalationNotice :execrows
-- The §25.9/§24.6-mirroring "one notice, never repeated" claim
-- (migrations/000058_workflow_hitl.up.sql's own doc comment): guarded on
-- needs_review_notified_at IS NULL, so of any number of concurrent/
-- repeated attempts to escalate the SAME run, exactly one ever claims the
-- right to enqueue the notice -- :execrows lets the caller tell "I claimed
-- it, send the notice" (1) from "already claimed/sent by an earlier
-- escalation" (0) apart, without a separate existence check.
UPDATE workflow_runs SET needs_review_notified_at = now(), updated_at = now() WHERE id = $1 AND needs_review_notified_at IS NULL;

-- ---------------------------------------------------------------------
-- "workflow definition & run API" (§25.10/§25.11) own additions below:
-- the definition/binding CRUD + duplicate surface, and
-- the two run-history list reads (session's own runs; one run's own
-- ordered step runs) the run view needs and no query above provides.
--
-- Definition writes (Create/Update/Delete + the two step-insert
-- variants + CreateWorkflowEdge) are always driven from a WithTx store
-- (§25.10: "The PUT is therefore a single transaction"): DeleteWorkflow
-- StepDefinitionsForDefinition relies on workflow_edges' own ON DELETE
-- CASCADE from workflow_step_definitions (migration 000057) to clear a
-- definition's old graph in one statement, never hand-diffed, and the
-- caller re-inserts the complete new graph in the SAME transaction.
--
-- CreateWorkflowStepDefinition takes a CLIENT-SUPPLIED id ($1) -- every
-- POST(whole-document)/PUT request body carries real step ids (a canvas editor's own
-- locally-generated uuid for a brand-new node, or an existing step's own
-- id echoed back), so edges within the SAME request body can reference a
-- step that has never been persisted before. DuplicateWorkflowStepDefinition
-- is the one exception: POST .../workflow-definitions' own
-- {sourceDefinitionId, name} path deep-copies an existing definition, and
-- every copied step must get a genuinely NEW id (the column's own
-- gen_random_uuid() default) so the copy never collides with its source
-- -- the caller remaps (source step id -> new step id) in Go to translate
-- the source's own edges onto the copy. Both hardcode kind = 'agent',
-- the only recognized workflow_step_kind value today
-- (internal/domain/workflow.StepKindAgent).
--
-- ExistsWorkflowBindingForDefinition is the "unbound draft" structural
-- refusal's own read (§25.10/§25.11's amendment): PUT/DELETE both check
-- this BEFORE touching a definition's rows at all. ExistsWorkflowRunForDefinition
-- is a THIRD guard this Step adds beyond the two §25.10/§25.11 name by
-- word: workflow_runs.workflow_definition_id and workflow_step_runs.
-- step_definition_id are both plain NO ACTION references (migration
-- 000057: "history outlives configuration"), so a definition that has
-- EVER run cannot have its steps deleted-and-reinserted (PUT) or the row
-- itself deleted (DELETE) without a raw FK-violation 500 -- reachable
-- even on a definition that is CURRENTLY unbound (rebinding a lane to a
-- duplicate frees the old definition's own workflow_bindings row while
-- its workflow_runs history remains behind). Refused with its own
-- distinct message, the same "validate first, name which rule broke"
-- discipline the other two guards already follow.
--
-- UpsertGlobalWorkflowBinding/UpsertRepoWorkflowBinding mirror
-- opencodeconfigs.sql's own UpsertEnvironmentOpenCodeConfig/
-- UpsertGlobalOpenCodeConfig pair exactly -- see that file's own doc
-- comment for why a value governed by two DIFFERENT partial unique
-- indexes (workflow_bindings_global_uniq/workflow_bindings_repo_uniq,
-- migration 000057) needs two separate upsert statements: Postgres's ON
-- CONFLICT clause names exactly one arbiter index per statement, and a
-- plain UNIQUE never matches on NULL, so a single "ON CONFLICT (lane,
-- repo_full_name)" would silently INSERT a second global row instead of
-- updating the first.
--
-- ListWorkflowStepRunsForRun orders oldest-first by creation -- the
-- chronological execution/re-attempt sequence (§25.10: "a run without
-- its steps answers no question anybody asks"); each retry/revise
-- re-execution is its own row, never an update-in-place (§25.5), so
-- creation order IS execution order.

-- name: ListWorkflowDefinitions :many
-- Carries the two EXISTS a caller would otherwise have to derive itself, so
-- §25.11's three edit refusals have ONE definition (refusalReasonForMutation,
-- httpapi/workflowdefinitions.go) rather than a server copy and a client copy
-- that drift. Computed in-query rather than per-row in Go: a list endpoint
-- issuing two extra round trips per definition is the N+1 this avoids.
--
-- The third refusal, run history, is the one a client could not derive AT ALL
-- -- nothing about runs is on WorkflowDefinition's own wire shape -- so
-- without this an editor only learns a definition is frozen by failing to
-- save it, after the operator has already done the work.
SELECT
    d.*,
    EXISTS (SELECT 1 FROM workflow_bindings b WHERE b.workflow_definition_id = d.id) AS is_bound,
    EXISTS (SELECT 1 FROM workflow_runs r WHERE r.workflow_definition_id = d.id) AS has_runs
FROM workflow_definitions d
ORDER BY d.lane, d.name;

-- name: GetWorkflowDefinitionWithRefusalFacts :one
-- GetWorkflowDefinition's own shape plus the same two EXISTS -- see
-- ListWorkflowDefinitions above for why they travel with the row.
SELECT
    d.*,
    EXISTS (SELECT 1 FROM workflow_bindings b WHERE b.workflow_definition_id = d.id) AS is_bound,
    EXISTS (SELECT 1 FROM workflow_runs r WHERE r.workflow_definition_id = d.id) AS has_runs
FROM workflow_definitions d
WHERE d.id = $1;

-- name: CreateWorkflowDefinition :one
-- is_built_in is hardcoded false: POST /api/workflow-definitions can
-- never mint a built-in row (only migration 000057's own seed does).
-- version is hardcoded 1: every freshly created or duplicated definition
-- starts there (§25.10).
INSERT INTO workflow_definitions (lane, name, is_built_in, version)
VALUES ($1, $2, false, 1)
RETURNING *;

-- name: UpdateWorkflowDefinitionNameAndBumpVersion :one
-- PUT /api/workflow-definitions/{id}'s own definition-row write -- name
-- is the only definition-level column this endpoint may change (lane/
-- is_built_in are immutable post-creation); version always increments by
-- exactly 1 on a successful write, regardless of what (if anything) the
-- caller sent ("Bump version on a successful write", §25.10).
UPDATE workflow_definitions SET name = $2, version = version + 1, updated_at = now() WHERE id = $1 RETURNING *;

-- name: DeleteWorkflowDefinition :execrows
DELETE FROM workflow_definitions WHERE id = $1;

-- name: ExistsWorkflowBindingForDefinition :one
SELECT EXISTS(SELECT 1 FROM workflow_bindings WHERE workflow_definition_id = $1);

-- name: ExistsWorkflowRunForDefinition :one
SELECT EXISTS(SELECT 1 FROM workflow_runs WHERE workflow_definition_id = $1);

-- name: DeleteWorkflowStepDefinitionsForDefinition :exec
DELETE FROM workflow_step_definitions WHERE workflow_definition_id = $1;

-- name: CreateWorkflowStepDefinition :one
INSERT INTO workflow_step_definitions
    (id, workflow_definition_id, step_order, kind, model_id, effort, prompt_template, execution_scope, conversation_continuity, hitl_before, hitl_after, canvas_position)
VALUES
    ($1, $2, $3, 'agent', $4, $5, $6, $7, $8, $9, $10, $11)
RETURNING *;

-- name: DuplicateWorkflowStepDefinition :one
INSERT INTO workflow_step_definitions
    (workflow_definition_id, step_order, kind, model_id, effort, prompt_template, execution_scope, conversation_continuity, hitl_before, hitl_after, canvas_position)
VALUES
    ($1, $2, 'agent', $3, $4, $5, $6, $7, $8, $9, $10)
RETURNING *;

-- name: CreateWorkflowEdge :one
INSERT INTO workflow_edges (workflow_definition_id, from_step_id, to_step_id, on_status)
VALUES ($1, $2, $3, $4)
RETURNING *;

-- name: ListWorkflowBindings :many
SELECT * FROM workflow_bindings ORDER BY lane, repo_full_name NULLS FIRST;

-- name: UpsertGlobalWorkflowBinding :one
INSERT INTO workflow_bindings (lane, repo_full_name, workflow_definition_id, definition_version)
VALUES ($1, NULL, $2, $3)
ON CONFLICT (lane) WHERE repo_full_name IS NULL
DO UPDATE SET workflow_definition_id = EXCLUDED.workflow_definition_id, definition_version = EXCLUDED.definition_version, updated_at = now()
RETURNING *;

-- name: UpsertRepoWorkflowBinding :one
INSERT INTO workflow_bindings (lane, repo_full_name, workflow_definition_id, definition_version)
VALUES ($1, $2, $3, $4)
ON CONFLICT (lane, repo_full_name) WHERE repo_full_name IS NOT NULL
DO UPDATE SET workflow_definition_id = EXCLUDED.workflow_definition_id, definition_version = EXCLUDED.definition_version, updated_at = now()
RETURNING *;

-- name: ListWorkflowRunsForSession :many
SELECT * FROM workflow_runs WHERE session_id = $1 ORDER BY created_at DESC;

-- name: ListWorkflowStepRunsForRun :many
-- §25.15: carries the two facts a run view needs that live on the
-- ordinary turn each attempt dispatched as, not on workflow_step_runs
-- itself -- turn_model_id (the join through turn_id that IS a step's
-- model, turns.model_id, persisted since migration 000018; §25.6 makes
-- every workflow step an ordinary sequential turn, so no new column was
-- needed) and turn_cost_usd (turns.cost_usd, migration 000098's own
-- running total, accumulated as this attempt's own step_finish events
-- land). A LEFT JOIN, not INNER: turn_id is nullable (an
-- awaiting_decision attempt has no turn yet), and a step run with no
-- turn must still be listed, with both extra columns simply absent --
-- mirrors ListWorkflowDefinitions' own "carries the extra columns a
-- caller would otherwise have to derive itself... a list endpoint
-- issuing extra round trips per row is the N+1 this avoids" reasoning
-- exactly, one join away from an EXISTS subquery.
SELECT
    sr.*,
    t.model_id AS turn_model_id,
    t.cost_usd AS turn_cost_usd
FROM workflow_step_runs sr
LEFT JOIN turns t ON t.id = sr.turn_id
WHERE sr.workflow_run_id = $1
ORDER BY sr.created_at ASC, sr.id ASC;
