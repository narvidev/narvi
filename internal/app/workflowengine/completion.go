// This file (completion.go) implements OnTurnCompleted -- the second
// wiring point (§25.6, doc.go), called from FOUR places in
// internal/app/sessionactor, every one where a turn genuinely reaches a
// terminal state (Completed/Failed/Cancelled):
//
//   - pushpr.go's completeProcessingTurn -- a real execution_complete wire
//     event (§3.3), the common case.
//   - timerfired.go's handleTurnDeadlineTimer -- turn_deadline expiring
//     with no terminal event ever arriving.
//   - dispatch.go's failDispatchedTurn -- SandboxCommander.SendCommand
//     itself failing, so the prompt never even reached the sandbox.
//   - stop.go's cancelStoppedTurns -- a person's stop (§3.3) cancelling a
//     flagged turn, from the stop timer or the dispatch gate. While the
//     session's stop request stands, an attempt that ends -- however it
//     ends, whichever of these paths reports it -- ends its run cancelled
//     and never consults NextStep; so does a cancel a stop asked for.
//
// All of them matter: a step's own workflow_step_runs attempt must be
// finalized (or parked awaiting_decision) no matter WHICH of these three
// paths ends its turn, or a bug/outage on the other two would leave that
// session's WorkflowRun stuck 'running' forever -- workflow_runs_
// one_running_per_session (migration 000057) would then block EVERY
// future turn on that session from ever starting a fresh run (see
// dispatch.go's own ResolveStepForNewTurn: it only ever starts a new run
// when none is currently running).
//
// # loopguard, HITL notifications, and real auto-dispatch (§25.9)
//
// §25.6 shipped this file with loopguard deliberately unconsulted (no
// built-in workflow could ever reach a needs_fix re-fire through this
// path) and NextAdvance creating a step-run's own bookkeeping row without
// ever dispatching a turn for it. §25.9 is "whichever future Step ...
// adds the actual auto-continuation dispatch" that gap's own doc comment
// named: the non-HITLAfter tail of this function now calls
// ApplyStepOutcome (advance.go), the SAME shared authority the HITL decide
// endpoint's own approve verdict calls, which consults loopguard.Evaluate
// on a genuine needs_fix re-fire and actually dispatches the next attempt's
// turn when proceeding -- see advance.go's own top doc comment for the
// full design. The HITLAfter branch below now also enqueues this run's
// "please decide" notice (deps.Workflows.MarkAwaitingDecision's own
// pre-existing job is otherwise unchanged).

package workflowengine

import (
	"context"
	"errors"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/narvidev/narvi/internal/adapters/outbound/postgres"
	"github.com/narvidev/narvi/internal/adapters/outbound/postgres/sqlcgen"
	"github.com/narvidev/narvi/internal/domain/turn"
	"github.com/narvidev/narvi/internal/domain/workflow"
	"github.com/narvidev/narvi/internal/platform"
)

// implicitOutcome derives a StepOutcomeStatus from a turn's own terminal
// trigger -- used only when no explicit outcome was already posted via the
// generic step-outcome-posting tool during the turn (OnTurnCompleted's own
// COALESCE-never-overwrite discipline, WorkflowStore.MarkAwaitingDecision/
// FinishStepRun: a posted outcome always wins). Pure, no I/O,
// table-driven-testable directly.
//
// turn.TriggerComplete -> StepOutcomeOK: the step's own turn finished
// normally. Every other terminal trigger (TriggerFail, TriggerTimeout,
// TriggerCancel) -> StepOutcomeBlocked: the step could not make progress
// at all -- never StepOutcomeNeedsFix, which is reserved for an agent's
// own explicit, structured report of a FIXABLE problem, never inferred
// from a bare turn-level failure the agent itself never characterized.
func implicitOutcome(trig turn.Trigger) workflow.StepOutcomeStatus {
	if trig == turn.TriggerComplete {
		return workflow.StepOutcomeOK
	}
	return workflow.StepOutcomeBlocked
}

// stepRunTerminalStatus maps a turn's own terminal trigger onto the
// workflow_step_runs status its attempt lands in when the step has no HITL
// gate (see OnTurnCompleted below). Pure, no I/O.
func stepRunTerminalStatus(trig turn.Trigger) string {
	switch trig {
	case turn.TriggerComplete:
		return "completed"
	case turn.TriggerCancel:
		return "cancelled"
	default: // turn.TriggerFail, turn.TriggerTimeout
		return "failed"
	}
}

// OnTurnCompleted is sessionactor's own turn-completion hook (§25.6): looks
// up whether turnID is a live, engine-tracked attempt; if so, finalizes it
// and -- unless the step is HITLAfter-gated -- consults workflow.NextStep
// (via ApplyStepOutcome, advance.go) to advance, complete, or escalate the
// owning run. sessionRow is the SAME row the caller (pushpr.go/timerfired.go/
// dispatch.go/stop.go, all four already fetch or hold it for their own
// unrelated reasons) already has in scope -- used for BuildModelID on an
// advance, for notification-destination resolution, and for the session's
// standing stop request (sessions.stop_requested_at). Every caller reads it
// in its own transaction, after the actor-epoch lock on the session row that
// the stop route's write also takes, so that request is the one standing
// now. Never returns an error (see
// doc.go's own "fail-open is load-bearing" section): any internal failure
// is logged and this simply does nothing further -- a turn's own completion
// (already durably persisted by the caller before this runs) must never be
// undone or blocked by a bug in this bookkeeping.
func OnTurnCompleted(ctx context.Context, deps Deps, sessionRow sqlcgen.Session, turnID pgtype.UUID, trig turn.Trigger) {
	logger := platform.Logger(ctx)
	workflows := deps.Workflows

	stepRun, err := workflows.GetLiveStepRunByTurnID(ctx, turnID)
	if err != nil {
		if !errors.Is(err, pgx.ErrNoRows) {
			logger.Warn("workflowengine: get live step run by turn id failed", "turn_id", turnID.String(), "error", err)
		}
		// pgx.ErrNoRows is the expected, common case for a turn this
		// package never tracked (see GetLiveStepRunByTurnID's own doc
		// comment) -- nothing to do either way.
		return
	}

	runRow, err := workflows.GetRun(ctx, stepRun.WorkflowRunID)
	if err != nil {
		logger.Error("workflowengine: get workflow run failed", "run_id", stepRun.WorkflowRunID.String(), "error", err)
		return
	}

	// Technical plan §3.3's stop. While the session's stop request stands
	// -- sessions.stop_requested_at, which only a person's next turn or
	// plan approval clears -- an attempt that ends, however it ends, ends
	// its run cancelled, before a HITL gate or workflow.NextStep is ever
	// reached. An advance is a turn created with no new input: an attempt
	// that completed before the stop reached it, failed, timed out, or was
	// created after the request would otherwise queue the next step, and
	// the stop would not stop. A cancel the stop asked for (the turn's own
	// flag) ends the run the same way even once a person has resumed the
	// session: read as 'blocked', it would let a custom definition's edge
	// queue the next step. Any other cancel keeps the path below.
	stopped := sessionRow.StopRequestedAt.Valid
	if !stopped && trig == turn.TriggerCancel {
		flagged, err := turnStopRequested(ctx, deps, turnID)
		if err != nil {
			// Fail toward the stop: a cancelled attempt whose stop flag
			// cannot be read never advances its run. (Inside the caller's
			// transaction a failed read has already aborted it, so the
			// caller's commit fails and the whole terminal write is retried.)
			logger.Error("workflowengine: read cancelled turn's stop request failed; ending its run as a stop",
				"turn_id", turnID.String(), "run_id", runRow.ID.String(), "error", err)
			flagged = true
		}
		stopped = flagged
	}
	if stopped {
		cancelStoppedRun(ctx, workflows, stepRun, runRow, trig)
		return
	}

	def, err := LoadDefinition(ctx, workflows, runRow.WorkflowDefinitionID)
	if err != nil {
		logger.Error("workflowengine: load definition for completed turn failed", "run_id", runRow.ID.String(), "error", err)
		return
	}

	stepID := workflow.ID(stepRun.StepDefinitionID.String())
	step, ok := stepByID(def, stepID)
	if !ok {
		logger.Error("workflowengine: completed step-run names a step not in its own definition",
			"run_id", runRow.ID.String(), "step_definition_id", stepRun.StepDefinitionID.String())
		return
	}

	outcome := workflow.StepOutcomeStatus("")
	if stepRun.OutcomeStatus != nil {
		outcome = workflow.StepOutcomeStatus(*stepRun.OutcomeStatus)
	}
	if !workflow.IsValidStepOutcomeStatus(outcome) {
		// No outcome was posted via the step-outcome tool during this
		// turn (true for every §25.8 built-in -- none of their prompts
		// change to instruct the agent to call it) -- derive one from how
		// the turn itself ended.
		outcome = implicitOutcome(trig)
	}

	if step.HITLAfter {
		// §25.9's HITL gate: this attempt is done, but the RUN does not
		// advance/complete/escalate until a human decides (the decide
		// endpoint, internal/adapters/inbound/httpapi). NextStep is
		// deliberately never consulted here.
		markedRun, err := workflows.MarkAwaitingDecision(ctx, stepRun.ID, string(outcome))
		if err != nil {
			logger.Error("workflowengine: mark step run awaiting decision failed", "step_run_id", stepRun.ID.String(), "error", err)
			return
		}
		// §25.9's own addition (§25.9): notify a human that this step
		// now needs a decision -- best-effort, logged, never allowed to
		// undo the awaiting_decision transition that just committed.
		if err := enqueueWorkflowNotice(ctx, deps, sessionRow, awaitingDecisionNoticeText(runRow.ID, markedRun.ID)); err != nil {
			logger.Error("workflowengine: enqueue awaiting-decision notice failed", "step_run_id", stepRun.ID.String(), "error", err)
		}
		return
	}

	finished, err := workflows.FinishStepRun(ctx, stepRun.ID, stepRunTerminalStatus(trig), string(outcome))
	if err != nil {
		logger.Error("workflowengine: finish step run failed", "step_run_id", stepRun.ID.String(), "error", err)
		return
	}

	// FinishStepRun's own UPDATE ... SET outcome_status = COALESCE(outcome_status, $3)
	// (queries/workflows.sql) preserves whatever outcome a concurrent call to
	// the generic step-outcome-posting tool (SetStepRunOutcome --
	// workflowstepoutcome.go's own store method, writing the SAME column on
	// the pool, unserialized with this function's own caller-owned
	// transaction) may have posted onto this row AFTER the lock-free
	// GetLiveStepRunByTurnID read above. finished.OutcomeStatus is that
	// authoritative, post-COALESCE value actually now in the row -- which
	// can differ from the pre-read/implicit-derived `outcome` local computed
	// above, if such a concurrent post landed in the multi-round-trip window
	// between that read and this call. NextStep below must be consulted
	// with THIS value: re-deriving `outcome` from FinishStepRun's own
	// RETURNING row here -- rather than trusting the earlier, possibly-stale
	// local -- is what actually closes that read-then-act race (a FOR
	// UPDATE lock on the earlier read would not help: it would still leave
	// a window between that lock's release and this call, so it would only
	// be a second, weaker mitigation for the same problem).
	if finished.OutcomeStatus == nil {
		// Should be unreachable: $3 above is always one of the three valid
		// StepOutcomeStatus values by this point (outcome was already
		// resolved via IsValidStepOutcomeStatus/implicitOutcome earlier in
		// this function), so COALESCE(outcome_status, $3) can only resolve
		// to NULL if that invariant somehow broke. Logged defensively;
		// falls back to the pre-read value rather than guessing further.
		logger.Error("workflowengine: finish step run returned nil outcome_status; using pre-read value",
			"step_run_id", stepRun.ID.String())
	} else if authoritative := workflow.StepOutcomeStatus(*finished.OutcomeStatus); !workflow.IsValidStepOutcomeStatus(authoritative) {
		logger.Error("workflowengine: finish step run returned invalid outcome_status; using pre-read value",
			"step_run_id", stepRun.ID.String(), "outcome_status", string(authoritative))
	} else if authoritative != outcome {
		logger.Info("workflowengine: outcome posted concurrently during turn completion; using finish step run's own authoritative value instead of the earlier stale read",
			"step_run_id", stepRun.ID.String(), "pre_read_outcome", string(outcome), "authoritative_outcome", string(authoritative))
		outcome = authoritative
	}

	// (§25.9): ApplyStepOutcome (advance.go) is the SAME shared
	// authority the HITL decide endpoint's own approve verdict calls --
	// consults workflow.NextStep, wires loopguard.Evaluate on a genuine
	// needs_fix re-fire, and actually dispatches the next attempt's turn on
	// an ordinary advance, preferring the just-finished attempt's own
	// advisory outcome summary (e.g. the audit step's own account of what
	// needs fixing) over its own generic fallback, so a re-dispatched "fix"
	// step's own turn is told WHAT to address, not just THAT it should
	// continue.
	if _, err := ApplyStepOutcome(ctx, deps, runRow, def, sessionRow, stepID, outcome, finished.OutcomeSummary); err != nil {
		// Fail open (doc.go): the run is left exactly where FinishStepRun
		// above already left it (its live step-run finished, the run's own
		// status untouched) rather than guessing at a further write.
		logger.Error("workflowengine: apply step outcome failed; leaving run as-is",
			"run_id", runRow.ID.String(), "step_id", string(stepID), "outcome", string(outcome), "error", err)
	}
}

// turnStopRequested reports whether turnID carries a person's stop request
// (turns.stop_requested_at, technical plan §3.3). A nil Turns store -- a
// caller wired without one -- reads as not stopped, the path every caller
// took before the stop existed.
func turnStopRequested(ctx context.Context, deps Deps, turnID pgtype.UUID) (bool, error) {
	if deps.Turns == nil {
		return false, nil
	}
	row, err := deps.Turns.Get(ctx, turnID)
	if err != nil {
		return false, err
	}
	return row.StopRequestedAt.Valid, nil
}

// cancelStoppedRun finishes stepRun with the status its turn really ended
// with (stepRunTerminalStatus(trig): an attempt that completed before the
// stop reached it stays completed) -- its outcome the one posted during the
// turn, else the one implied by trig -- and ends runRow cancelled.
// workflow.NextStep is never consulted: nothing follows a stop. Fail-open
// like the rest of this file: a failed write is logged and the run left
// where it is.
func cancelStoppedRun(ctx context.Context, workflows *postgres.WorkflowStore, stepRun sqlcgen.WorkflowStepRun, runRow sqlcgen.WorkflowRun, trig turn.Trigger) {
	logger := platform.Logger(ctx)
	if _, err := workflows.FinishStepRun(ctx, stepRun.ID, stepRunTerminalStatus(trig), string(implicitOutcome(trig))); err != nil {
		logger.Error("workflowengine: finish stopped step run failed", "step_run_id", stepRun.ID.String(), "error", err)
		return
	}
	if _, err := workflows.CancelRun(ctx, runRow.ID); err != nil {
		logger.Error("workflowengine: cancel stopped workflow run failed", "run_id", runRow.ID.String(), "error", err)
		return
	}
	logger.Info("workflowengine: workflow run cancelled by a stop; next step not consulted",
		"run_id", runRow.ID.String(), "step_run_id", stepRun.ID.String())
}
