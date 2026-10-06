// This file (decideworkflowstep.go) implements §25.9's ("workflow HITL
// gate + circuit breaker", §25.9/§25.10/§25.11) own decide endpoint: POST
// /api/workflow-runs/:runId/steps/:stepRunId/decide -- the SAME shape
// discipline as decideplan.go's approve/reject (contracts/rest/v1/
// dtos.schema.json's own WorkflowStepDecideRequest doc comment: "the same
// shape discipline as decideplan.go's approve/reject"): parse path params,
// look up the target row (404 if missing, defensively re-checking it
// belongs to the named run), authorize (own/joined-aware,
// authz.ActionDecideWorkflowStep -- the SAME matrix row as
// ActionApprovePlan, §25.11), attempt a guarded UPDATE keyed on
// "AND status = 'awaiting_decision'" (the SAME idempotency/re-submission
// guard decideplan.go's own ApproveIfAwaitingApproval/
// RejectIfAwaitingApproval establish -- rowsAffected == 0 means already
// decided, or a stale id, reported as 409, never a silent no-op), apply
// the verdict's own consequence, commit, then fire-and-forget
// TriggerDispatch exactly like every other turn-creating entry point in
// this package.
//
// Three verdicts, mirroring restdtos.WorkflowStepDecideRequestVerdict's own
// closed enum exactly:
//
//   - approve: consults workflow.NextStep with the step's own already-
//     posted outcome (internal/app/workflowengine.ApplyStepOutcome, the
//     SAME shared authority OnTurnCompleted's own ordinary path calls) --
//     complete/escalate/advance-and-dispatch, loopguard.Evaluate included
//     on a genuine needs_fix re-fire. Exactly what OnTurnCompleted would
//     have done had this step not been HITLAfter-gated in the first place.
//   - reject: ends the run (WorkflowStore.FailRun, WorkflowRun.Status =
//     'failed' per WorkflowStepDecideResponse.runStatus's own documented
//     example) -- never consults NextStep or loopguard at all; nothing
//     further dispatches.
//   - revise: ALWAYS re-executes the SAME step, folding text in as the new
//     attempt's own prompt -- mirrors plan mode's own existing "prompt =
//     feedback" mechanism (internal/adapters/inbound/slack/handler.go,
//     internal/adapters/inbound/linear/webhook.go: a revise reply becomes a
//     brand-new turn whose ENTIRE prompt is the extracted feedback text,
//     the SAME ongoing conversation/session already carrying full context
//     of what the step just did) one level more general: workflowengine's
//     own dispatchNextAttempt renders the step's PromptTemplate against
//     that text as "{{prompt}}", which is the identity transform for every
//     §25.8 built-in shape, exactly reproducing plan mode's own literal
//     behavior for the concrete case that matters, while staying correct
//     for a hypothetical custom step with a richer template. Deliberately
//     NEVER calls ApplyStepOutcome/workflow.NextStep/loopguard.Evaluate AT
//     ALL -- a STRUCTURAL exemption from the circuit breaker (§25.9,
//     mirroring §24.6's own "a human's manual re-trigger is never subject
//     to [the automatic budget], regardless of how many automatic
//     re-reviews already fired" exemption): this code path simply never
//     reaches the function that consults loopguard, not a flag some other
//     path could accidentally bypass.
//
// Technical plan §3.3's stop: approve and revise are a person's act that
// sets the run going again, like approving a plan, so each resumes a
// stopped session -- the session's stop request is cleared in this same
// transaction, as DecidePlanOnTx clears it. Left standing, it would let the
// decided attempt run and then end the run cancelled when that attempt ends
// (workflowengine.OnTurnCompleted). Reject sets nothing going, as a plan's
// reject does not: it ends the run failed, as before, and leaves the
// request standing. The transaction takes the session's actor-epoch lock
// before anything else, the lock the stop route writes the request under,
// so a stop and a decision are ordered: a stop committed first is the
// request this decision clears, and one committed after it flags the turn
// this decision inserted.

package httpapi

import (
	"encoding/json"
	"errors"
	"net/http"

	"github.com/go-chi/chi/v5"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/narvidev/narvi/contracts/gen/go/restdtos"
	"github.com/narvidev/narvi/internal/adapters/outbound/postgres"
	"github.com/narvidev/narvi/internal/app/sessionactor"
	"github.com/narvidev/narvi/internal/app/turnguard"
	"github.com/narvidev/narvi/internal/app/workflowengine"
	plandomain "github.com/narvidev/narvi/internal/domain/plan"
	"github.com/narvidev/narvi/internal/domain/sessionguard"
	"github.com/narvidev/narvi/internal/domain/workflow"
	"github.com/narvidev/narvi/internal/platform"
)

// parseWorkflowRunID/parseWorkflowStepRunID parse chi's own "runId"/
// "stepRunId" URL path params as UUIDs -- mirror parsePlanID's own exact
// shape (planapprove.go).
func parseWorkflowRunID(w http.ResponseWriter, r *http.Request) (pgtype.UUID, bool) {
	raw := chi.URLParam(r, "runId")
	var id pgtype.UUID
	if err := id.Scan(raw); err != nil {
		writeError(w, http.StatusBadRequest, "malformed workflow run id")
		return pgtype.UUID{}, false
	}
	return id, true
}

func parseWorkflowStepRunID(w http.ResponseWriter, r *http.Request) (pgtype.UUID, bool) {
	raw := chi.URLParam(r, "stepRunId")
	var id pgtype.UUID
	if err := id.Scan(raw); err != nil {
		writeError(w, http.StatusBadRequest, "malformed workflow step run id")
		return pgtype.UUID{}, false
	}
	return id, true
}

// DecideWorkflowStep backs POST /api/workflow-runs/:runId/steps/:stepRunId/decide
// (§25.9/§25.10/§25.11). See this file's own top doc comment for the full
// per-verdict design.
func DecideWorkflowStep(
	pool *pgxpool.Pool,
	sessions *postgres.SessionStore,
	turns *postgres.TurnStore,
	participants *postgres.ParticipantStore,
	workflows *postgres.WorkflowStore,
	slackThreadSessions *postgres.SlackThreadSessionStore,
	linearAgentSessions *postgres.LinearAgentSessionStore,
	githubPRSessions *postgres.GitHubPRSessionStore,
	outbox *postgres.OutboxStore,
	registry *sessionactor.Registry,
	guard *turnguard.Guard,
	epistemicCheckDefault bool,
) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		ctx := r.Context()
		logger := platform.Logger(ctx)

		runID, ok := parseWorkflowRunID(w, r)
		if !ok {
			return
		}
		stepRunID, ok := parseWorkflowStepRunID(w, r)
		if !ok {
			return
		}

		actorUserID, ok := authenticatedUserID(w, r)
		if !ok {
			return
		}
		authUser, ok := platform.UserFromContext(ctx)
		if !ok {
			// Unreachable behind auth.Middleware -- see authenticatedUserID's
			// own identical defensive precedent (helpers.go).
			logger.Error("httpapi: no authenticated user in context (route not mounted behind auth.Middleware?)")
			writeError(w, http.StatusInternalServerError, "internal error")
			return
		}

		r.Body = http.MaxBytesReader(w, r.Body, maxRequestBodyBytes)
		var req restdtos.WorkflowStepDecideRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			writeError(w, http.StatusBadRequest, "malformed request body: "+err.Error())
			return
		}

		var text *string
		if req.Text != nil {
			t := string(*req.Text)
			text = &t
		}
		if req.Verdict == restdtos.WorkflowStepDecideRequestVerdictRevise && (text == nil || plandomain.IsBlankFeedback(*text)) {
			// The specific 400 message this file's own schema doc comment
			// promises ("required non-empty for verdict 'revise' ... this
			// Step's handler, which owns the specific 400 message") --
			// plandomain.IsBlankFeedback is the SAME shared "is this
			// effectively empty" definition every other revise-style guard
			// in this codebase already calls (verdict.go's own doc comment),
			// now a fourth caller.
			writeError(w, http.StatusBadRequest, "revise requires non-empty text")
			return
		}

		tx, err := pool.Begin(ctx)
		if err != nil {
			logger.Error("httpapi: begin decide-workflow-step tx failed", "error", err)
			writeError(w, http.StatusInternalServerError, "internal error")
			return
		}
		defer func() { _ = tx.Rollback(ctx) }()

		txWorkflows := workflows.WithTx(tx)

		runRow, err := txWorkflows.GetRun(ctx, runID)
		if err != nil {
			if errors.Is(err, pgx.ErrNoRows) {
				writeError(w, http.StatusNotFound, "workflow run not found")
				return
			}
			logger.Error("httpapi: get workflow run failed", "error", err)
			writeError(w, http.StatusInternalServerError, "internal error")
			return
		}

		// The session's actor-epoch lock, before any write: see this file's
		// top comment (§3.3's stop). The actor takes it first in every
		// transaction too, so the order of locks is the same.
		if _, err := sessions.WithTx(tx).GetActorEpochForUpdate(ctx, runRow.SessionID); err != nil {
			if errors.Is(err, pgx.ErrNoRows) {
				// Defensive: see the session read below.
				writeError(w, http.StatusNotFound, "workflow run not found")
				return
			}
			logger.Error("httpapi: lock session row for workflow step decision failed", "error", err)
			writeError(w, http.StatusInternalServerError, "internal error")
			return
		}

		stepRun, err := txWorkflows.GetStepRun(ctx, stepRunID)
		if err != nil {
			if errors.Is(err, pgx.ErrNoRows) {
				writeError(w, http.StatusNotFound, "workflow step run not found")
				return
			}
			logger.Error("httpapi: get workflow step run failed", "error", err)
			writeError(w, http.StatusInternalServerError, "internal error")
			return
		}
		if stepRun.WorkflowRunID != runRow.ID {
			// Defensive, security-relevant: mirrors DecidePlanOnTx's own
			// identical cross-session mismatch check (decideplan.go) --
			// never leak a foreign step-run's real status through THIS
			// run's own URL.
			logger.Warn("httpapi: decide workflow step: stepRunId belongs to a different run than the caller's own; refusing to leak its status",
				"run_id", runID.String(), "step_run_id", stepRunID.String(), "actual_run_id", stepRun.WorkflowRunID.String())
			writeError(w, http.StatusNotFound, "workflow step run not found")
			return
		}

		sessionRow, err := sessions.WithTx(tx).Get(ctx, runRow.SessionID)
		if err != nil {
			if errors.Is(err, pgx.ErrNoRows) {
				// Defensive: workflow_runs.session_id is ON DELETE CASCADE
				// (migration 000057), so a run whose session was deleted
				// would already be gone too -- should be unreachable.
				writeError(w, http.StatusNotFound, "workflow run not found")
				return
			}
			logger.Error("httpapi: get session for workflow step decision failed", "error", err)
			writeError(w, http.StatusInternalServerError, "internal error")
			return
		}

		allowed, err := canActOnWorkflowStep(ctx, participants.WithTx(tx), sessionRow, actorUserID, authUser.Role)
		if err != nil {
			logger.Error("httpapi: canActOnWorkflowStep failed", "error", err)
			writeError(w, http.StatusInternalServerError, "internal error")
			return
		}
		if !allowed {
			writeError(w, http.StatusForbidden, "not authorized to decide this workflow step")
			return
		}

		rowsAffected, err := txWorkflows.DecideStepRun(ctx, stepRunID, string(req.Verdict), text, actorUserID)
		if err != nil {
			logger.Error("httpapi: decide workflow step run failed", "error", err)
			writeError(w, http.StatusInternalServerError, "internal error")
			return
		}
		if rowsAffected == 0 {
			// Idempotency/re-submission guard (§25.9, mirroring
			// decideplan.go's own identical "already decided, or a stale
			// id" 409): the guarded UPDATE's own "AND status =
			// 'awaiting_decision'" clause matched zero rows -- an earlier
			// call already decided this same attempt (whichever verdict),
			// or stepRunID/runId never named a genuinely awaiting_decision
			// row to begin with.
			writeError(w, http.StatusConflict, "workflow step is not awaiting decision (already decided, or a stale id)")
			return
		}

		deps := workflowengine.Deps{
			Workflows:           txWorkflows,
			Turns:               turns.WithTx(tx),
			SlackThreadSessions: slackThreadSessions.WithTx(tx),
			LinearAgentSessions: linearAgentSessions.WithTx(tx),
			GitHubPRSessions:    githubPRSessions.WithTx(tx),
			Outbox:              outbox.WithTx(tx),
			// Technical plan §40.1: the attempt an approve or a revise
			// dispatches is admitted by the session guard first, and a
			// refusal -- a person's decision, so never an escalation -- is
			// answered 409 below and rolled back (refuseWorkflowDecision).
			Guard:                 guard.WithTx(tx, nil),
			Origin:                sessionguard.OriginPerson,
			EpistemicCheckDefault: epistemicCheckDefault,
		}
		// refuseWorkflowDecision answers a refusal of the session guard: the
		// decision rolls back -- the step stays awaiting a decision, the
		// stop request stays as it was -- the refusal is recorded once this
		// transaction, which holds the session row's lock, is gone, and the
		// person is told why, with the typed reason.
		refuseWorkflowDecision := func(err error) bool {
			refusal, ok := sessionguard.AsRefusal(err)
			if !ok {
				return false
			}
			_ = tx.Rollback(ctx)
			guard.RecordRefusal(ctx, sessionRow.ID, refusal)
			writeErrorWithReason(w, http.StatusConflict, refusal.Error(), string(refusal.Reason))
			return true
		}

		// §3.3: approve and revise resume a stopped session (this file's
		// top comment); reject leaves its stop request standing.
		if req.Verdict == restdtos.WorkflowStepDecideRequestVerdictApprove || req.Verdict == restdtos.WorkflowStepDecideRequestVerdictRevise {
			if _, err := sessions.WithTx(tx).ClearStopRequest(ctx, sessionRow.ID); err != nil {
				logger.Error("httpapi: clear session stop request for workflow step decision failed", "error", err)
				writeError(w, http.StatusInternalServerError, "internal error")
				return
			}
		}

		var runStatus, stepRunStatus string
		var dispatchedTurnID *pgtype.UUID

		switch req.Verdict {
		case restdtos.WorkflowStepDecideRequestVerdictApprove:
			stepRunStatus = "completed"
			outcome := workflow.StepOutcomeStatus("")
			if stepRun.OutcomeStatus != nil {
				outcome = workflow.StepOutcomeStatus(*stepRun.OutcomeStatus)
			}
			def, derr := workflowengine.LoadDefinition(ctx, txWorkflows, runRow.WorkflowDefinitionID)
			if derr != nil {
				logger.Error("httpapi: load workflow definition for decide (approve) failed", "error", derr)
				writeError(w, http.StatusInternalServerError, "internal error")
				return
			}
			stepID := workflow.ID(stepRun.StepDefinitionID.String())
			result, aerr := workflowengine.ApplyStepOutcome(ctx, deps, runRow, def, sessionRow, stepID, outcome, stepRun.OutcomeSummary)
			if refuseWorkflowDecision(aerr) {
				return
			}
			if aerr != nil {
				logger.Error("httpapi: apply step outcome for decide (approve) failed", "error", aerr)
				writeError(w, http.StatusInternalServerError, "internal error")
				return
			}
			runStatus = result.RunStatus
			dispatchedTurnID = result.DispatchedTurnID

		case restdtos.WorkflowStepDecideRequestVerdictReject:
			stepRunStatus = "failed"
			run, rerr := txWorkflows.FailRun(ctx, runRow.ID)
			if rerr != nil {
				logger.Error("httpapi: fail workflow run for decide (reject) failed", "error", rerr)
				writeError(w, http.StatusInternalServerError, "internal error")
				return
			}
			runStatus = string(run.Status)

		case restdtos.WorkflowStepDecideRequestVerdictRevise:
			// Structural circuit-breaker exemption (§25.9, see this file's
			// own top doc comment): NEVER calls ApplyStepOutcome/
			// workflow.NextStep/loopguard.Evaluate. Re-executes the SAME
			// step (stepRun.StepDefinitionID), never workflow.NextStep's
			// verdict -- text is guaranteed non-empty by the validation
			// above.
			stepRunStatus = "completed"
			def, derr := workflowengine.LoadDefinition(ctx, txWorkflows, runRow.WorkflowDefinitionID)
			if derr != nil {
				logger.Error("httpapi: load workflow definition for decide (revise) failed", "error", derr)
				writeError(w, http.StatusInternalServerError, "internal error")
				return
			}
			sameStep, sok := workflowStepByID(def, workflow.ID(stepRun.StepDefinitionID.String()))
			if !sok {
				logger.Error("httpapi: decide (revise): current step not found in its own definition",
					"run_id", runRow.ID.String(), "step_definition_id", stepRun.StepDefinitionID.String())
				writeError(w, http.StatusInternalServerError, "internal error")
				return
			}
			newTurnID, derr := workflowengine.DispatchSameStepRevision(ctx, deps, runRow.ID, sameStep, *text, sessionRow)
			if refuseWorkflowDecision(derr) {
				return
			}
			if derr != nil {
				logger.Error("httpapi: dispatch same-step revision failed", "error", derr)
				writeError(w, http.StatusInternalServerError, "internal error")
				return
			}
			runStatus = string(runRow.Status)
			dispatchedTurnID = &newTurnID

		default:
			// Unreachable: restdtos.WorkflowStepDecideRequest's own
			// generated UnmarshalJSON already rejects any verdict outside
			// its 3-value enum before this handler ever runs.
			writeError(w, http.StatusBadRequest, "unrecognized verdict")
			return
		}

		if err := tx.Commit(ctx); err != nil {
			logger.Error("httpapi: commit decide-workflow-step tx failed", "error", err)
			writeError(w, http.StatusInternalServerError, "internal error")
			return
		}

		// Fire-and-forget, OUTSIDE the transaction above, exactly mirroring
		// every other turn-creating entry point in this package (ApprovePlan/
		// DecidePlan's own identical post-commit TriggerDispatch sequencing)
		// -- a no-op (GetOrSpawn/Send are cheap idempotent calls) when this
		// verdict dispatched no new turn at all (reject).
		TriggerDispatch(ctx, registry, sessionRow.ID)

		var turnIDStr *string
		if dispatchedTurnID != nil {
			s := dispatchedTurnID.String()
			turnIDStr = &s
		}
		logger.Info("httpapi: decided workflow step", "run_id", runID.String(), "step_run_id", stepRunID.String(),
			"verdict", string(req.Verdict), "run_status", runStatus, "step_run_status", stepRunStatus)

		writeJSON(w, http.StatusOK, restdtos.WorkflowStepDecideResponse{
			StepRunId:     stepRunID.String(),
			StepRunStatus: restdtos.WorkflowStepDecideResponseStepRunStatus(stepRunStatus),
			RunStatus:     restdtos.WorkflowStepDecideResponseRunStatus(runStatus),
			TurnId:        restdtos.WorkflowStepDecideResponseTurnId(turnIDStr),
		})
	}
}

// workflowStepByID finds the step with the given id within def -- a small,
// package-local mirror of workflow.NextStep's own unexported stepByID
// (internal/domain/workflow/nextstep.go) and workflowengine's own identical
// unexported helper (dispatch.go/definition.go): each of the three packages
// that need this exact lookup keeps its own trivial linear scan rather than
// exporting a fourth cross-package dependency for a two-line loop over a
// slice ValidateDefinition already guarantees has unique ids.
func workflowStepByID(def workflow.Definition, id workflow.ID) (workflow.StepDefinition, bool) {
	for _, s := range def.Steps {
		if s.ID == id {
			return s, true
		}
	}
	return workflow.StepDefinition{}, false
}
