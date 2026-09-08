-- Queries backing TurnStore (§4.3). Just enough to prove the pipeline end
-- to end (create + get), including exercising the
-- turns_one_processing_per_session partial unique index (§3.3).

-- name: CreateTurn :one
-- prompt/model_id/plan_mode (migrations/000018_session_repos.up.sql,
-- §9.3) are the turn's own dispatch-time inputs -- prompt/model_id are
-- nullable and plan_mode defaults false, so every EXISTING call site
-- (every prior Step's `CreateTurnParams{SessionID, Status}`) keeps
-- compiling and behaving identically: the zero-value nil/nil/false it
-- already implicitly got before this Step's own columns existed.
--
-- effort (migrations/000063_turn_session_effort.up.sql, §29.8)
-- mirrors model_id's own shape exactly, one column over -- plain
-- positional param like model_id itself (this query's own existing style
-- for a nullable column; sqlc generates a keyed struct either way, so
-- every EXISTING call site that never sets it -- a keyed
-- CreateTurnParams{...} literal omitting Effort -- keeps compiling and
-- behaving identically: the zero value, nil, "use the default").
--
-- review_head_sha (migrations/000072_turns_review_head_sha.up.sql, §21
-- review finding C2) mirrors effort's own identical shape one column
-- further -- nil/absent for every non-review turn (every existing call
-- site), set exactly once, at creation, by the two review-turn-creation
-- paths (internal/adapters/inbound/httpapi's createTurnLocked/
-- CreateSessionOnTx) with the commit SHA that turn's own pre-fetched
-- diff was anchored to. See that migration's own doc comment for the
-- full "why".
--
-- answer_only (migrations/000074_plan_followup.up.sql, §23.2)
-- mirrors review_head_sha's own identical shape one column further --
-- nil/absent for every existing call site (every CreateTurnParams
-- literal that predates this Step), set exactly once, at creation, by
-- createTurnLocked's own plan_followup gate (turn.go). See that
-- migration's own doc comment for the full "why NULL vs FALSE" split.
--
-- review_depth (migrations/000080_turns_review_depth.up.sql,
-- §26.3) mirrors review_head_sha's own identical shape one column
-- further -- nil/absent for every non-review turn, set exactly once, at
-- creation, by every review-turn-creation path.
--
-- review_depth_decision (migrations/000083_turns_review_depth_decision.up.sql,
-- §18.4's own precedent) is review_depth's own richer sibling --
-- the full internal/domain/reviewtriage.DecisionRecord, JSON-marshaled by
-- the caller (this query does no encoding of its own).
--
-- review_knowledge_mode/review_knowledge_decision (migrations/
-- 000114_turns_review_knowledge_mode.up.sql,
-- 000116_turns_review_knowledge_decision.up.sql, §31.2/§31.6) mirror
-- review_depth/review_depth_decision's own identical shape two columns
-- further: nil/absent for every non-review turn, set exactly once, at
-- creation, by the SAME review-turn-creation paths.
-- review_knowledge_decision is pre-marshaled JSON (internal/domain/
-- knowledge.InjectedRecord) -- this core does no encoding of its own,
-- mirroring review_depth_decision's own identical convention.
INSERT INTO turns (session_id, status, prompt, model_id, plan_mode, effort, review_head_sha, answer_only, review_depth, review_depth_decision, review_knowledge_mode, review_knowledge_decision)
VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12)
RETURNING *;

-- name: GetTurn :one
SELECT * FROM turns
WHERE id = $1;

-- name: UpdateTurnStatus :one
-- Sets a turn's status, plus dispatched_at/completed_at/
-- dispatched_sandbox_gen when the caller supplies one (sqlc.narg +
-- COALESCE: an absent/NULL argument leaves the existing column value
-- untouched, matching dispatched_at/completed_at's own nullability --
-- each is set at most once, at the (from, trigger) transition that
-- reaches Dispatched or a terminal state respectively).
--
-- dispatched_sandbox_gen (migration 000026_turn_dispatch_gen.up.sql,
-- §3.3 "turn recovery") is stamped by TWO distinct call sites sharing this
-- SAME query, both at the moment a Prompt payload is built and about to be
-- sent: tryPlanDispatch (internal/app/sessionactor/dispatch.go), alongside
-- the SAME status=dispatched write that already sets dispatched_at, for
-- the normal Pending->Dispatched->Processing path; and tryPlanReenqueue
-- (same file), which passes the CURRENT status back unchanged (the turn
-- is already, validly, Processing -- this call re-stamps
-- dispatched_sandbox_gen only, never re-transitions status) for a
-- Processing turn whose prompt needs re-sending to a respawned sandbox
-- incarnation.
--
-- dispatched_event_id (migrations/000089_turns_dispatched_event_id.up.sql)
-- is stamped by those SAME two call sites, in the SAME write, from
-- MaxEventIDForSession (queries/events.sql): the events-log high-water
-- mark at the instant of dispatch, which the §26.4 corroboration
-- queries use as their lower bound instead of a timestamp. It follows the
-- identical sqlc.narg + COALESCE "absent argument leaves the column
-- untouched" convention as the three columns above it.
UPDATE turns
SET status = $2,
    dispatched_at = COALESCE(sqlc.narg('dispatched_at'), dispatched_at),
    completed_at = COALESCE(sqlc.narg('completed_at'), completed_at),
    dispatched_sandbox_gen = COALESCE(sqlc.narg('dispatched_sandbox_gen'), dispatched_sandbox_gen),
    dispatched_event_id = COALESCE(sqlc.narg('dispatched_event_id'), dispatched_event_id)
WHERE id = $1
RETURNING *;

-- name: ListTurnsForSession :many
-- Full turn history for one session, oldest first -- exactly the input
-- shape internal/domain/session.DeriveStatus requires (an ordered slice
-- of turn.Summary derived from these rows).
SELECT * FROM turns
WHERE session_id = $1
ORDER BY created_at ASC;

-- name: MarkTurnProgressNotified :execrows
-- Audit finding M16 ("completeness", internal/adapters/outbound/linearapi/
-- doc.go): atomic, race-safe "has this turn already had its one mid-turn
-- progress milestone fired" guard -- mirrors ApprovePlanIfAwaitingApproval/
-- RejectPlanIfAwaitingApproval's own "guarded UPDATE, observed via
-- :execrows" idiom exactly (queries/plans.sql), just for a nullable
-- timestamp rather than an enum status. 0 rows affected means
-- progress_notified_at was already set for this turn (a second, later
-- tool_call event in the same turn -- the expected, common case once the
-- milestone has already fired once -- or a race); exactly 1 row affected
-- means THIS call is the one that gets to enqueue the Linear progress
-- notification (see internal/app/sessionactor/progressnotify.go).
UPDATE turns
SET progress_notified_at = $2
WHERE id = $1 AND progress_notified_at IS NULL;

-- name: GetProcessingTurnForSession :one
-- §20 ("builder epistemic pre-action check", §20.2) own epistemic-
-- outcome-posting endpoint's first read -- mirrors WorkflowStore's own
-- GetRunningRunForSession/GetLiveStepRunForRun precedent (queries/
-- workflows.sql): the caller (a sandbox-authenticated POST naming no turn
-- id at all, exactly like the workflow-step-outcome endpoint) resolves
-- "the session's own CURRENTLY live turn" itself, from the sandbox-
-- authenticated session id alone. turns_one_processing_per_session
-- (migrations/000005_turns.up.sql) guarantees at most one row can ever
-- match.
SELECT * FROM turns
WHERE session_id = $1 AND status = 'processing';

-- name: SetTurnEpistemicOutcome :execrows
-- The guarded UPDATE backing that same endpoint (§20.2) -- mirrors
-- SetWorkflowStepRunOutcome's own "WHERE ... AND status = 'running'" guard
-- exactly (queries/workflows.sql), one status value over: re-checks the
-- turn is STILL the live processing one at write time, closing the race
-- where it completed/failed/was cancelled between this endpoint's own
-- GetProcessingTurnForSession read and this write. Unguarded by "AND
-- epistemic_outcome IS NULL" -- deliberately, mirroring
-- SetWorkflowStepRunOutcome's own identical choice: an agent that calls
-- this endpoint more than once for the same still-processing turn (e.g.
-- correcting itself) gets last-write-wins, not a rejected second call.
-- 0 rows affected means the turn is no longer processing (a genuine race,
-- or a stale/foreign turn id having somehow been targeted -- this query
-- takes none, so in practice only the race).
UPDATE turns
SET epistemic_outcome = $2
WHERE id = $1 AND status = 'processing';

-- name: RecordTurnStepCost :execrows
-- §25.15's per-step cost accumulation, made idempotent on the key the wire
-- actually gives us for that: step_finish.stepId (§6.1), one per step.
--
-- ONE statement, three jobs. The CTE resolves the turn this event belongs
-- to from the sandbox-authenticated session id alone, with no preceding
-- read -- turns_one_processing_per_session (migrations/000005) guarantees
-- at most one row can match. The INSERT claims (session_id, step_id) or
-- conflicts away to nothing. The UPDATE runs ONLY over rows the INSERT
-- actually produced, so a redelivered step_finish moves no dollars and a
-- genuinely new one moves exactly its own.
--
-- The key is (session_id, step_id) and NOT (turn_id, step_id), which is
-- what it was until migration 000100. turn_id is resolved from whichever
-- turn is processing when the event lands, so it MOVES between a delivery
-- and its replay -- a replay arriving after the turn boundary resolved a
-- different turn, did not conflict, and charged the same step again.
-- Measured: one $5.00 step delivered twice charged $10.00. An idempotency
-- key cannot be derived from state that changes between the two
-- deliveries it exists to tell apart.
--
-- This replaces an earlier version gated on whether appendRawEvent had
-- INSERTED the raw event row. That flag answers "was this (session_id,
-- message_id) new to the events table", which is not the same question --
-- step_start and step_finish are two parts of one assistant message and
-- share its id, so step_start always claimed the row first and every
-- production step_finish was discarded before its cost was ever read. See
-- migrations/000099_turn_step_costs.up.sql for the full account.
--
-- Concurrency: "SET cost_usd = COALESCE(cost_usd, 0) + ..." is computed IN
-- SQL over a row Postgres locks for the duration, so two step_finish events
-- for the same turn always sum and never clobber -- unlike a Go-side
-- read-then-write, which can lose a sibling's increment. COALESCE, never a
-- bare "+", because turns.cost_usd's own migration (000098) keeps NULL as
-- the ONLY representation of "no cost has arrived yet", and a bare sum
-- against NULL stays NULL forever -- the exact "no cost yet reads as free"
-- failure §25.15 exists to prevent.
--
-- Returns rows updated (0 or 1). 0 means either a redelivery (already
-- counted) or no turn currently processing for this session -- the caller
-- distinguishes them only by logging, never by retrying: both are states
-- where adding money again would be the worse error.
--
-- KNOWN LIMIT, stated rather than implied: this attributes cost to
-- whichever turn is processing WHEN THE EVENT LANDS, not to the turn the
-- event was emitted for. They are the same turn in every ordinary case,
-- because step_finish only arrives mid-turn. They are not the same if a
-- turn terminalizes (timeout, cancel) while one of its step_finish events
-- is still in flight and the next turn has already started -- those
-- dollars land on the newer turn. Closing that needs a turn id on the
-- event itself, which §6.1 does not carry today; it is recorded here so
-- the next reader does not mistake the current behaviour for a guarantee.
WITH target AS (
    SELECT t.id FROM turns AS t WHERE t.session_id = $1 AND t.status = 'processing'
), claimed AS (
    INSERT INTO turn_step_costs (session_id, turn_id, step_id, cost_usd)
    SELECT $1, target.id, $2, $3 FROM target
    ON CONFLICT (session_id, step_id) DO NOTHING
    RETURNING turn_id, cost_usd
)
UPDATE turns
SET cost_usd = COALESCE(turns.cost_usd, 0) + claimed.cost_usd
FROM claimed
WHERE turns.id = claimed.turn_id;


-- name: ListSessionCostTotalsWithRepos :many
-- The shadow-operator surface's own LLM-spend line (§30.1: "surfaced, not suppressed" --
-- shadow burns real customer provider credit, and the evaluator must see
-- it). Reuses turns.cost_usd (migration 000098), the SAME running total
-- internal/app/sessionactor's own recordStepFinishCost (stepcost.go)
-- already maintains from step_finish.cost.usd -- this is a READ over
-- that existing figure, never a second cost-computation path.
--
-- SUM ignores a NULL per-turn total, so a session's own total_cost_usd
-- here is NULL only when EVERY one of its turns still has none -- never
-- a fabricated $0 for a session that simply has not reported a figure
-- yet (turns.cost_usd's own migration comment: "NULL, never 0, stays the
-- ONLY representation of 'no cost has arrived yet'" -- this query
-- preserves that discipline rather than collapsing it at the aggregate
-- boundary). The caller (internal/app/shadowoperator) sums these
-- per-session totals with reviewtriage.NumericToFloat64, the SAME
-- pgtype.Numeric-to-float64 conversion httpapi/workflowruns.go's own
-- per-step cost display already uses.
--
-- Joined with sessions.repos for the SAME Go-side repo resolution
-- ListShadowSuppressedOutboxWithSessionRepos uses (outbox.sql's own doc
-- comment) -- every session with at least one turn and at least one
-- named repository, LIVE or shadow alike: LLM spend is surfaced
-- regardless of egress mode (§30.1), so this performs no
-- suppressed_in_shadow filtering at all, unlike the outbox query above.
SELECT s.id AS session_id,
       s.repos AS repos,
       SUM(t.cost_usd)::numeric(14, 6) AS total_cost_usd
FROM sessions s
JOIN turns t ON t.session_id = s.id
WHERE s.repos != '[]'::jsonb
GROUP BY s.id;
