package postgres

import (
	"context"
	"fmt"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/narvidev/narvi/internal/adapters/outbound/postgres/sqlcgen"
)

// WorkflowStore is a thin, pass-through wrapper around the sqlc-generated
// workflow_* queries ("workflow execution engine", §25.6/§25.7/
// §25.8) -- the first real reader/writer of §25.4's own dark schema
// (migrations/000057_workflows.up.sql). No caching, no retries, no
// business rules: definition/binding resolution, run/step-run lifecycle
// decisions, and every workflow.NextStep consultation live in
// internal/app/workflowengine, this store's only caller. Every method
// returns/accepts plain sqlcgen types (never a domain/workflow value) --
// mirroring PlanStore/TurnStore's own identical convention -- so
// workflowengine, not this package, owns converting between the two.
type WorkflowStore struct {
	q *sqlcgen.Queries
}

// NewWorkflowStore builds a WorkflowStore backed by pool.
func NewWorkflowStore(pool *pgxpool.Pool) *WorkflowStore {
	return &WorkflowStore{q: sqlcgen.New(pool)}
}

// WithTx returns a WorkflowStore whose queries run on tx instead of the
// pool this store was built with -- every real caller uses this: engine
// resolution/bookkeeping always runs inside the SAME transaction as the
// turn insert (createTurnLocked) or the turn's own terminal-state write
// (sessionactor's completeProcessingTurn/handleTurnDeadlineTimer/
// failDispatchedTurn), never a second, independently-committed write.
func (s *WorkflowStore) WithTx(tx pgx.Tx) *WorkflowStore {
	return &WorkflowStore{q: s.q.WithTx(tx)}
}

// GetBindingForRepo fetches the repo-specific workflow_bindings row for
// (lane, repoFullName), if one exists -- returns pgx.ErrNoRows (unwrapped)
// when it doesn't, the caller's own signal to fall back to
// GetGlobalBinding (§25.4: a repo override shadows the global binding for
// that one repo only; it is optional).
func (s *WorkflowStore) GetBindingForRepo(ctx context.Context, lane, repoFullName string) (sqlcgen.WorkflowBinding, error) {
	return s.q.GetWorkflowBindingForRepo(ctx, sqlcgen.GetWorkflowBindingForRepoParams{
		Lane:         sqlcgen.WorkflowLane(lane),
		RepoFullName: &repoFullName,
	})
}

// GetGlobalBinding fetches the guaranteed (lane, repo_full_name = NULL)
// workflow_bindings row -- §25.4: seeded by migration 000057 for every
// lane, so this is never truly expected to return pgx.ErrNoRows in a
// correctly-migrated database (see workflowengine's own fail-open handling
// for the defensive case where it somehow does).
func (s *WorkflowStore) GetGlobalBinding(ctx context.Context, lane string) (sqlcgen.WorkflowBinding, error) {
	return s.q.GetGlobalWorkflowBinding(ctx, sqlcgen.WorkflowLane(lane))
}

// GetDefinition fetches one workflow_definitions row by id (minus its
// steps/edges -- see ListStepDefinitions/ListEdgesForDefinition).
func (s *WorkflowStore) GetDefinition(ctx context.Context, id pgtype.UUID) (sqlcgen.WorkflowDefinition, error) {
	return s.q.GetWorkflowDefinition(ctx, id)
}

// LockDefinitionForUpdate reads a definition row and holds a row-level lock
// on it for the rest of the calling transaction -- see the query's own
// comment (queries/workflows.sql) for why §25.11's bound-definition refusal
// needs it to be more than a read-then-write. Must be called on a WithTx
// store; on the pool-backed store it takes a lock that is released
// immediately, which is useless rather than harmful.
func (s *WorkflowStore) LockDefinitionForUpdate(ctx context.Context, id pgtype.UUID) (sqlcgen.WorkflowDefinition, error) {
	return s.q.LockWorkflowDefinitionForUpdate(ctx, id)
}

// ListStepDefinitions fetches every workflow_step_definitions row for
// definitionID, ordered by step_order.
func (s *WorkflowStore) ListStepDefinitions(ctx context.Context, definitionID pgtype.UUID) ([]sqlcgen.WorkflowStepDefinition, error) {
	return s.q.ListWorkflowStepDefinitions(ctx, definitionID)
}

// ListEdgesForDefinition fetches every workflow_edges row for
// definitionID -- the caller groups these by FromStepID when assembling a
// domain/workflow.Definition value (each step carries its own outgoing
// edges there; the flat workflow_edges table has no such per-step
// grouping).
func (s *WorkflowStore) ListEdgesForDefinition(ctx context.Context, definitionID pgtype.UUID) ([]sqlcgen.WorkflowEdge, error) {
	return s.q.ListWorkflowEdgesForDefinition(ctx, definitionID)
}

// GetRunningRunForSession fetches sessionID's own single running
// workflow_runs row, if one exists -- returns pgx.ErrNoRows (unwrapped)
// when it doesn't (no run ever started, or the last one already reached a
// terminal/needs_review state) -- the caller's own signal to start a new
// run via CreateRun.
func (s *WorkflowStore) GetRunningRunForSession(ctx context.Context, sessionID pgtype.UUID) (sqlcgen.WorkflowRun, error) {
	return s.q.GetRunningWorkflowRunForSession(ctx, sessionID)
}

// GetRun fetches one workflow_runs row by id.
func (s *WorkflowStore) GetRun(ctx context.Context, id pgtype.UUID) (sqlcgen.WorkflowRun, error) {
	return s.q.GetWorkflowRun(ctx, id)
}

// CreateRun inserts a new workflow_runs row (status defaults to
// 'running') -- lane/definitionID/definitionVersion are pinned as
// provenance at start time (§25.6), never re-resolved later for this same
// run.
func (s *WorkflowStore) CreateRun(ctx context.Context, sessionID pgtype.UUID, lane string, definitionID pgtype.UUID, definitionVersion int32) (sqlcgen.WorkflowRun, error) {
	return s.q.CreateWorkflowRun(ctx, sqlcgen.CreateWorkflowRunParams{
		SessionID:            sessionID,
		Lane:                 sqlcgen.WorkflowLane(lane),
		WorkflowDefinitionID: definitionID,
		DefinitionVersion:    definitionVersion,
	})
}

// GetLiveStepRunForRun fetches runID's own single live (running or
// awaiting_decision) workflow_step_runs attempt, if one exists -- returns
// pgx.ErrNoRows (unwrapped) when it doesn't (defensive only: a running run
// should always have exactly one live attempt, per this Step's own
// invariant -- see workflowengine's own fail-open handling for this case).
func (s *WorkflowStore) GetLiveStepRunForRun(ctx context.Context, runID pgtype.UUID) (sqlcgen.WorkflowStepRun, error) {
	return s.q.GetLiveWorkflowStepRunForRun(ctx, runID)
}

// GetLiveStepRunByTurnID fetches the running workflow_step_runs attempt
// whose turn_id is turnID, if one exists -- returns pgx.ErrNoRows
// (unwrapped) when it doesn't: turnID was never engine-tracked at all (a
// session's very first turn, inserted by CreateSessionOnTx rather than
// createTurnLocked -- see internal/app/workflowengine/doc.go), or it was a
// turn ResolveStepForNewTurn deliberately resolved-but-did-not-track (see
// that function's own doc comment). Either way, the caller's own signal
// that OnTurnCompleted has nothing to do for this turn.
func (s *WorkflowStore) GetLiveStepRunByTurnID(ctx context.Context, turnID pgtype.UUID) (sqlcgen.WorkflowStepRun, error) {
	return s.q.GetLiveWorkflowStepRunByTurnID(ctx, turnID)
}

// CreateStepRun inserts a new workflow_step_runs row for stepDefinitionID
// within runID (status defaults to 'running', turn_id starts NULL) -- the
// caller attaches the real turn id once it exists (AttachTurn below).
func (s *WorkflowStore) CreateStepRun(ctx context.Context, runID, stepDefinitionID pgtype.UUID) (sqlcgen.WorkflowStepRun, error) {
	return s.q.CreateWorkflowStepRun(ctx, sqlcgen.CreateWorkflowStepRunParams{
		WorkflowRunID:    runID,
		StepDefinitionID: stepDefinitionID,
	})
}

// AttachTurn backfills stepRunID's own turn_id -- called right after the
// real turn insert succeeds (ResolveStepForNewTurn itself runs BEFORE that
// insert, since it must decide the step's PromptTemplate/ModelID to build
// the turn with in the first place).
func (s *WorkflowStore) AttachTurn(ctx context.Context, stepRunID, turnID pgtype.UUID) error {
	return s.q.AttachTurnToWorkflowStepRun(ctx, sqlcgen.AttachTurnToWorkflowStepRunParams{
		ID:     stepRunID,
		TurnID: turnID,
	})
}

// MarkAwaitingDecision transitions stepRunID to 'awaiting_decision'
// (§25.9's HITL gate -- the actual decision mechanics are §25.9's own
// job, but a HITLAfter-gated step's own attempt must still land here, not
// 'completed', once its turn finishes) -- outcomeStatus is recorded via
// COALESCE so an outcome ALREADY posted via the step-outcome tool during
// the turn is never overwritten. finished_at stays NULL (this attempt is
// still "live" per migration 000057's own doc comment).
func (s *WorkflowStore) MarkAwaitingDecision(ctx context.Context, stepRunID pgtype.UUID, outcomeStatus string) (sqlcgen.WorkflowStepRun, error) {
	status := sqlcgen.WorkflowStepOutcomeStatus(outcomeStatus)
	return s.q.MarkWorkflowStepRunAwaitingDecision(ctx, sqlcgen.MarkWorkflowStepRunAwaitingDecisionParams{
		ID:            stepRunID,
		OutcomeStatus: &status,
	})
}

// FinishStepRun transitions stepRunID to a terminal status (completed,
// failed, or cancelled -- never awaiting_decision, MarkAwaitingDecision's
// own exclusive job) and stamps finished_at. outcomeStatus is recorded via
// the SAME COALESCE-never-overwrite discipline as MarkAwaitingDecision.
func (s *WorkflowStore) FinishStepRun(ctx context.Context, stepRunID pgtype.UUID, status, outcomeStatus string) (sqlcgen.WorkflowStepRun, error) {
	os := sqlcgen.WorkflowStepOutcomeStatus(outcomeStatus)
	return s.q.FinishWorkflowStepRun(ctx, sqlcgen.FinishWorkflowStepRunParams{
		ID:            stepRunID,
		Status:        sqlcgen.WorkflowStepRunStatus(status),
		OutcomeStatus: &os,
	})
}

// CompleteRun transitions runID to 'completed' and stamps finished_at --
// workflow.NextComplete's own consequence (§25.4/§25.6).
func (s *WorkflowStore) CompleteRun(ctx context.Context, runID pgtype.UUID) (sqlcgen.WorkflowRun, error) {
	return s.q.CompleteWorkflowRun(ctx, runID)
}

// EscalateRun transitions runID to 'needs_review' -- workflow.NextEscalate's
// own consequence (§25.4/§25.9): non-terminal (finished_at stays NULL),
// parked for a human decision. Posting an actual notice about this
// escalation is §25.9's own job (§25.9: "one notice, never repeated");
// this method only ever flips the status column.
func (s *WorkflowStore) EscalateRun(ctx context.Context, runID pgtype.UUID) (sqlcgen.WorkflowRun, error) {
	return s.q.EscalateWorkflowRun(ctx, runID)
}

// FailRun transitions runID to 'failed' and stamps finished_at -- §25.9's
// own ("workflow HITL gate + circuit breaker", §25.9) consequence of a
// winning HITL reject verdict: mirrors CompleteRun's own shape exactly,
// landing on 'failed' instead of 'completed' (a human's reject IS the run's
// own final word, a terminal outcome like completion, never
// 'needs_review's "waiting on a human" semantics).
func (s *WorkflowStore) FailRun(ctx context.Context, runID pgtype.UUID) (sqlcgen.WorkflowRun, error) {
	return s.q.FailWorkflowRun(ctx, runID)
}

// CancelRun transitions runID to 'cancelled' and stamps finished_at -- the
// consequence of a person's stop (technical plan §3.3) cancelling the turn
// of the run's live attempt: terminal like FailRun, and never followed by
// workflow.NextStep (workflowengine.OnTurnCompleted).
func (s *WorkflowStore) CancelRun(ctx context.Context, runID pgtype.UUID) (sqlcgen.WorkflowRun, error) {
	return s.q.CancelWorkflowRun(ctx, runID)
}

// SetStepRunOutcome persists the generic step-outcome-posting tool's own
// typed payload ({status, summary, structuredPayload}, §25.6) onto
// stepRunID -- guarded to the attempt currently actually 'running' (the
// underlying SQL's own "AND status = 'running'"), so rowsAffected == 0
// tells the caller there was no live attempt to post onto (already
// finished, or awaiting a HITL decision) without a separate existence
// check. outcomePayload is opaque json.RawMessage, stored and later
// consumed verbatim -- never re-parsed here (§25.6's own typed-handoff
// discipline, review.RenderTurnPrompt's precedent).
func (s *WorkflowStore) SetStepRunOutcome(ctx context.Context, stepRunID pgtype.UUID, outcomeStatus, summary string, payload []byte) (int64, error) {
	os := sqlcgen.WorkflowStepOutcomeStatus(outcomeStatus)
	return s.q.SetWorkflowStepRunOutcome(ctx, sqlcgen.SetWorkflowStepRunOutcomeParams{
		ID:             stepRunID,
		OutcomeStatus:  &os,
		OutcomeSummary: &summary,
		OutcomePayload: payload,
	})
}

// GetStepRun fetches one workflow_step_runs row by id -- §25.9's own
// ("workflow HITL gate + circuit breaker", §25.9) decide endpoint's first
// read (mirrors PlanStore.Get's identical role in decideplan.go): resolves
// the target attempt the caller named (POST /api/workflow-runs/:runId/
// steps/:stepRunId/decide's own stepRunId) before deciding it, and re-fetches
// it after the guarded decision UPDATE to learn its REAL current state
// either way (won or already decided).
func (s *WorkflowStore) GetStepRun(ctx context.Context, id pgtype.UUID) (sqlcgen.WorkflowStepRun, error) {
	return s.q.GetWorkflowStepRun(ctx, id)
}

// CountStepRunsForStepDefinition returns how many workflow_step_runs rows
// ALREADY exist for stepDefinitionID within runID -- §25.5's own "iteration
// count reads COUNT(*) on workflow_step_runs, no dedicated counter column",
// scoped to the ONE run and ONE step definition about to receive a new
// attempt. The engine (internal/app/workflowengine) consults this ONLY when
// a needs_fix edge is about to advance to this step, to decide whether
// loopguard.Evaluate even needs consulting at all: zero means this is the
// step's first-ever attempt in this run (not a re-fire, proceed directly);
// more than zero means it is, and the returned count is exactly the
// loopguard.State.AttemptCount to evaluate against.
func (s *WorkflowStore) CountStepRunsForStepDefinition(ctx context.Context, runID, stepDefinitionID pgtype.UUID) (int64, error) {
	return s.q.CountWorkflowStepRunsForStepDefinition(ctx, sqlcgen.CountWorkflowStepRunsForStepDefinitionParams{
		WorkflowRunID:    runID,
		StepDefinitionID: stepDefinitionID,
	})
}

// DecideStepRun renders verdict (approve/reject/revise) on stepRunID,
// guarded to the SAME "AND status = 'awaiting_decision'" precondition for
// all three (§25.9) -- mirrors PlanStore's own ApproveIfAwaitingApproval/
// RejectIfAwaitingApproval :execrows shape exactly: rowsAffected == 0 means
// this call did NOT win the decision (already decided by an earlier call,
// or a stale/foreign id), rowsAffected == 1 means it did. decisionText is
// nil for approve, optional for reject, and the caller's own job to require
// non-empty for revise (this store applies no such validation itself,
// mirroring SetStepRunOutcome's own "store persists, callers validate"
// division of labor). decidedBy mirrors DecidePlanOnTx's own decidedBy
// convention (an explicitly invalid pgtype.UUID for a not-yet-supported
// bot/channel-attributed decision -- every current caller of this endpoint
// is REST, always passing a real authenticated actorUserID).
func (s *WorkflowStore) DecideStepRun(ctx context.Context, stepRunID pgtype.UUID, verdict string, decisionText *string, decidedBy pgtype.UUID) (int64, error) {
	switch verdict {
	case "approve":
		return s.q.DecideWorkflowStepRunApprove(ctx, sqlcgen.DecideWorkflowStepRunApproveParams{ID: stepRunID, DecisionText: decisionText, DecidedBy: decidedBy})
	case "reject":
		return s.q.DecideWorkflowStepRunReject(ctx, sqlcgen.DecideWorkflowStepRunRejectParams{ID: stepRunID, DecisionText: decisionText, DecidedBy: decidedBy})
	case "revise":
		return s.q.DecideWorkflowStepRunRevise(ctx, sqlcgen.DecideWorkflowStepRunReviseParams{ID: stepRunID, DecisionText: decisionText, DecidedBy: decidedBy})
	default:
		return 0, fmt.Errorf("postgres: unrecognized workflow step decision verdict %q", verdict)
	}
}

// ClaimEscalationNotice atomically claims the right to send runID's own
// ONE-TIME "this run now needs a human's attention" notice (§25.9,
// mirroring §24.6's own "never repeated" exemption mechanism) -- guarded to
// "AND needs_review_notified_at IS NULL" (migrations/000058_workflow_hitl.up.sql),
// so of any number of concurrent or repeated calls against the SAME run,
// rowsAffected == 1 for exactly one of them (the caller that must actually
// enqueue the notification) and == 0 for every other (already claimed --
// send nothing further). Independent of, and always called alongside,
// EscalateRun -- claiming is idempotent regardless of how many times a run
// is (redundantly) escalated.
func (s *WorkflowStore) ClaimEscalationNotice(ctx context.Context, runID pgtype.UUID) (int64, error) {
	return s.q.ClaimWorkflowRunEscalationNotice(ctx, runID)
}

// --- The autonomy freeze's held advances (technical plan §40.2, §25.9):
// workflow_advance_holds, one row per run whose automatic advance the
// freeze holds. internal/app/workflowengine writes and releases them.

// HoldAdvance records that runID's advance past its finished attempt
// stepRunID is held, at now on the database's clock. It reports whether a
// row was written: false when the run already holds one, which a run with
// no live attempt cannot reach.
func (s *WorkflowStore) HoldAdvance(ctx context.Context, runID, stepRunID, sessionID pgtype.UUID) (bool, error) {
	n, err := s.q.HoldWorkflowAdvance(ctx, sqlcgen.HoldWorkflowAdvanceParams{
		WorkflowRunID: runID,
		StepRunID:     stepRunID,
		SessionID:     sessionID,
	})
	return n == 1, err
}

// GetAdvanceHold returns runID's held advance; pgx.ErrNoRows (unwrapped)
// when it holds none.
func (s *WorkflowStore) GetAdvanceHold(ctx context.Context, runID pgtype.UUID) (sqlcgen.WorkflowAdvanceHold, error) {
	return s.q.GetWorkflowAdvanceHold(ctx, runID)
}

// AdvanceHoldCursor is where a page of held advances starts: after the
// hold at (HeldAt, RunID) in (held_at, workflow_run_id) order.
type AdvanceHoldCursor struct {
	HeldAt pgtype.Timestamptz
	RunID  pgtype.UUID
}

// FirstAdvanceHolds is the cursor of the first page: -infinity and the nil
// uuid, before every hold.
var FirstAdvanceHolds = AdvanceHoldCursor{
	HeldAt: pgtype.Timestamptz{InfinityModifier: pgtype.NegativeInfinity, Valid: true},
	RunID:  pgtype.UUID{Valid: true},
}

// AdvanceHoldCursorAfter is the cursor of the page that follows the one h
// ended.
func AdvanceHoldCursorAfter(h sqlcgen.WorkflowAdvanceHold) AdvanceHoldCursor {
	return AdvanceHoldCursor{HeldAt: h.HeldAt, RunID: h.WorkflowRunID}
}

// ListAdvanceHolds returns at most limit held advances after cursor,
// oldest first: one page.
func (s *WorkflowStore) ListAdvanceHolds(ctx context.Context, cursor AdvanceHoldCursor, limit int32) ([]sqlcgen.WorkflowAdvanceHold, error) {
	return s.q.ListWorkflowAdvanceHolds(ctx, sqlcgen.ListWorkflowAdvanceHoldsParams{
		AfterHeldAt: cursor.HeldAt,
		AfterRunID:  cursor.RunID,
		MaxHolds:    limit,
	})
}

// ReleaseAdvanceHold deletes runID's held advance and returns it: the
// release's compare-and-swap. pgx.ErrNoRows (unwrapped) when it is gone --
// another replica released it, or a person's stop dropped it.
func (s *WorkflowStore) ReleaseAdvanceHold(ctx context.Context, runID pgtype.UUID) (sqlcgen.WorkflowAdvanceHold, error) {
	return s.q.ReleaseWorkflowAdvanceHold(ctx, runID)
}

// DeleteAdvanceHoldsForStop deletes every advance sessionID held at or
// before stopRequestedAt -- a person's stop -- and returns their runs.
func (s *WorkflowStore) DeleteAdvanceHoldsForStop(ctx context.Context, sessionID pgtype.UUID, stopRequestedAt pgtype.Timestamptz) ([]pgtype.UUID, error) {
	return s.q.DeleteWorkflowAdvanceHoldsForStop(ctx, sqlcgen.DeleteWorkflowAdvanceHoldsForStopParams{
		SessionID:       sessionID,
		StopRequestedAt: stopRequestedAt,
	})
}

// --- "workflow definition & run API" (§25.10/§25.11) own additions
// below: the definition/binding CRUD + duplicate surface, and
// the two run-history list reads the run view needs. Definition writes
// always run through WithTx (above) -- httpapi's own handlers open one
// transaction per PUT/POST/DELETE, exactly like DecideWorkflowStep does
// for the run/step-run writes above.

// ListDefinitions fetches every workflow_definitions row, built-in and
// custom alike, ordered (lane, name) -- GET /api/workflow-definitions.
// Each row carries is_bound/has_runs alongside the definition -- see the
// query's own comment for why those two facts travel with the row rather
// than being re-derived per definition by a caller.
func (s *WorkflowStore) ListDefinitions(ctx context.Context) ([]sqlcgen.ListWorkflowDefinitionsRow, error) {
	return s.q.ListWorkflowDefinitions(ctx)
}

// GetDefinitionWithRefusalFacts is GetDefinition plus the same two EXISTS
// ListDefinitions carries, for the single-definition read.
func (s *WorkflowStore) GetDefinitionWithRefusalFacts(ctx context.Context, id pgtype.UUID) (sqlcgen.GetWorkflowDefinitionWithRefusalFactsRow, error) {
	return s.q.GetWorkflowDefinitionWithRefusalFacts(ctx, id)
}

// CreateDefinition inserts a new workflow_definitions row -- is_built_in
// always false, version always 1 (§25.10: "always lands is_built_in =
// false, unbound, at version 1"), for both POST /api/workflow-definitions
// paths (whole-document and {sourceDefinitionId, name} duplicate).
func (s *WorkflowStore) CreateDefinition(ctx context.Context, lane, name string) (sqlcgen.WorkflowDefinition, error) {
	return s.q.CreateWorkflowDefinition(ctx, sqlcgen.CreateWorkflowDefinitionParams{
		Lane: sqlcgen.WorkflowLane(lane),
		Name: name,
	})
}

// UpdateDefinitionNameAndBumpVersion updates id's own name and increments
// version by exactly 1 -- PUT /api/workflow-definitions/{id}'s own
// definition-row write (the steps/edges half of that same transaction is
// DeleteStepDefinitionsForDefinition + CreateStepDefinition + CreateEdge,
// below).
func (s *WorkflowStore) UpdateDefinitionNameAndBumpVersion(ctx context.Context, id pgtype.UUID, name string) (sqlcgen.WorkflowDefinition, error) {
	return s.q.UpdateWorkflowDefinitionNameAndBumpVersion(ctx, sqlcgen.UpdateWorkflowDefinitionNameAndBumpVersionParams{ID: id, Name: name})
}

// DeleteDefinition deletes id's own workflow_definitions row -- DELETE
// /api/workflow-definitions/{id}, called only once the caller's own
// is-built-in/is-bound/has-run-history pre-checks have all passed (see
// ExistsBindingForDefinition/ExistsRunForDefinition below).
func (s *WorkflowStore) DeleteDefinition(ctx context.Context, id pgtype.UUID) (int64, error) {
	return s.q.DeleteWorkflowDefinition(ctx, id)
}

// ExistsBindingForDefinition reports whether any workflow_bindings row
// still resolves to id -- the "unbound draft" structural refusal's own
// read (§25.10/§25.11's own amendment): PUT/DELETE both consult this
// BEFORE touching this definition's own rows at all.
func (s *WorkflowStore) ExistsBindingForDefinition(ctx context.Context, id pgtype.UUID) (bool, error) {
	return s.q.ExistsWorkflowBindingForDefinition(ctx, id)
}

// ExistsRunForDefinition reports whether any workflow_runs row has EVER
// run id -- a THIRD structural guard PUT/DELETE both also consult, beyond
// the two §25.10/§25.11 name by word: workflow_runs.workflow_definition_id
// and workflow_step_runs.step_definition_id are both plain NO ACTION
// references (migration 000057: "history outlives configuration"), so a
// definition with run history cannot have its steps deleted-and-reinserted
// (PUT) or the row itself deleted (DELETE) without a raw FK-violation 500
// -- reachable even when the definition is CURRENTLY unbound (rebinding a
// lane to a duplicate frees the old definition's own workflow_bindings row
// while its workflow_runs history remains behind). Refused with its own
// distinct message, the same "validate first, name which rule broke"
// discipline the other two guards already follow.
func (s *WorkflowStore) ExistsRunForDefinition(ctx context.Context, id pgtype.UUID) (bool, error) {
	return s.q.ExistsWorkflowRunForDefinition(ctx, id)
}

// DeleteStepDefinitionsForDefinition deletes every workflow_step_definitions
// row for definitionID -- workflow_edges cascades away with them
// (migration 000057's own ON DELETE CASCADE), so this single statement
// clears a definition's ENTIRE existing graph in one step: always the
// first half of PUT's own "replace wholesale, never hand-diff" write
// (§25.10), inside the same transaction as the CreateStepDefinition/
// CreateEdge calls that re-populate it.
func (s *WorkflowStore) DeleteStepDefinitionsForDefinition(ctx context.Context, definitionID pgtype.UUID) error {
	return s.q.DeleteWorkflowStepDefinitionsForDefinition(ctx, definitionID)
}

// CreateStepDefinition inserts one step carrying a CLIENT-SUPPLIED id --
// every POST(whole-document)/PUT request body carries real step ids, so
// an edge within the SAME request body can reference a step that has
// never been persisted before (a canvas editor's own locally-generated
// uuid for a brand-new node, or an existing step's own id echoed back).
func (s *WorkflowStore) CreateStepDefinition(ctx context.Context, arg sqlcgen.CreateWorkflowStepDefinitionParams) (sqlcgen.WorkflowStepDefinition, error) {
	return s.q.CreateWorkflowStepDefinition(ctx, arg)
}

// DuplicateStepDefinition inserts one step with a SERVER-GENERATED id
// (the column's own gen_random_uuid() default) -- POST /api/workflow-
// definitions' own {sourceDefinitionId, name} duplicate path: every
// copied step gets a genuinely NEW id, never reusing its source's own, so
// the caller remaps (source step id -> new step id) in Go to translate
// the source's own edges onto the copy.
func (s *WorkflowStore) DuplicateStepDefinition(ctx context.Context, arg sqlcgen.DuplicateWorkflowStepDefinitionParams) (sqlcgen.WorkflowStepDefinition, error) {
	return s.q.DuplicateWorkflowStepDefinition(ctx, arg)
}

// CreateEdge inserts one workflow_edges row -- shared by every write path
// (whole-document create, PUT, duplicate): an edge never carries a
// client-supplied id (WorkflowEdge's own wire shape has none), so this is
// the ONE insert variant every caller needs.
func (s *WorkflowStore) CreateEdge(ctx context.Context, definitionID, fromStepID, toStepID pgtype.UUID, onStatus string) (sqlcgen.WorkflowEdge, error) {
	return s.q.CreateWorkflowEdge(ctx, sqlcgen.CreateWorkflowEdgeParams{
		WorkflowDefinitionID: definitionID,
		FromStepID:           fromStepID,
		ToStepID:             toStepID,
		OnStatus:             sqlcgen.WorkflowStepOutcomeStatus(onStatus),
	})
}

// ListBindings fetches every workflow_bindings row -- GET /api/workflow-bindings.
func (s *WorkflowStore) ListBindings(ctx context.Context) ([]sqlcgen.WorkflowBinding, error) {
	return s.q.ListWorkflowBindings(ctx)
}

// UpsertGlobalBinding create-or-replaces the (lane, NULL) global binding
// -- one of TWO scope-specific upserts PutWorkflowBinding's own handler
// picks between (see UpsertRepoBinding below), never a single ON
// CONFLICT spanning both partial unique indexes (queries/workflows.sql's
// own doc comment states the full "why" -- mirrors postgres.
// OpenCodeConfigStore's own identical two-query precedent).
func (s *WorkflowStore) UpsertGlobalBinding(ctx context.Context, lane string, definitionID pgtype.UUID, definitionVersion int32) (sqlcgen.WorkflowBinding, error) {
	return s.q.UpsertGlobalWorkflowBinding(ctx, sqlcgen.UpsertGlobalWorkflowBindingParams{
		Lane:                 sqlcgen.WorkflowLane(lane),
		WorkflowDefinitionID: definitionID,
		DefinitionVersion:    definitionVersion,
	})
}

// UpsertRepoBinding create-or-replaces the (lane, repoFullName) repo-scoped
// binding -- the repo-scoped counterpart to UpsertGlobalBinding above.
func (s *WorkflowStore) UpsertRepoBinding(ctx context.Context, lane, repoFullName string, definitionID pgtype.UUID, definitionVersion int32) (sqlcgen.WorkflowBinding, error) {
	return s.q.UpsertRepoWorkflowBinding(ctx, sqlcgen.UpsertRepoWorkflowBindingParams{
		Lane:                 sqlcgen.WorkflowLane(lane),
		RepoFullName:         &repoFullName,
		WorkflowDefinitionID: definitionID,
		DefinitionVersion:    definitionVersion,
	})
}

// ListRunsForSession fetches sessionID's own workflow_runs rows, newest
// first -- GET /api/sessions/{id}/workflow-runs.
func (s *WorkflowStore) ListRunsForSession(ctx context.Context, sessionID pgtype.UUID) ([]sqlcgen.WorkflowRun, error) {
	return s.q.ListWorkflowRunsForSession(ctx, sessionID)
}

// ListStepRunsForRun fetches every workflow_step_runs row for runID,
// oldest first -- GET /api/workflow-runs/{runId}: the chronological
// execution/re-attempt sequence a run detail view renders (§25.10: "a run
// without its steps answers no question anybody asks"). Each row also
// carries its own turn's model_id/cost_usd via a LEFT JOIN (§25.15) --
// see ListWorkflowStepRunsForRun's own generated doc comment for why a
// join, not a per-row extra query.
func (s *WorkflowStore) ListStepRunsForRun(ctx context.Context, runID pgtype.UUID) ([]sqlcgen.ListWorkflowStepRunsForRunRow, error) {
	return s.q.ListWorkflowStepRunsForRun(ctx, runID)
}
