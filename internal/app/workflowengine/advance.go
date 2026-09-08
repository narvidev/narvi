// This file (advance.go) implements §25.9's ("workflow HITL gate +
// circuit breaker", §25.9) own central authority for what happens after a
// workflow.NextStep verdict: ApplyStepOutcome is called from BOTH
// OnTurnCompleted's own ordinary (agent-driven) advance path (completion.go)
// and the HTTP decide endpoint's approve verdict (internal/adapters/inbound/
// httpapi) -- exactly one place decides "complete the run, escalate it, or
// advance to (and actually dispatch) the next attempt", never two
// independently-evolving copies (CLAUDE.md, §11: every state transition
// through the owning transition table).
//
// # loopguard is consulted ONLY on a genuine needs_fix re-fire
//
// workflow.NextStep itself never consults internal/domain/loopguard (its
// own doc comment is explicit: that is the ENGINE's job, layered on top).
// This file is the first real call site (§25.9's audit -> fix loop is the
// motivating, though non-built-in, example): when the verdict is
// NextAdvance AND the posted outcome is StepOutcomeNeedsFix, this checks
// whether the TARGET step-definition already has at least one prior
// attempt within THIS run (WorkflowStore.CountStepRunsForStepDefinition) --
// zero means this is that step's first-ever attempt (no loop exists yet,
// proceed directly, no breaker consultation at all); more than zero means
// this needs_fix edge is genuinely RE-firing (the target step has looped
// back around at least once already), so loopguard.Evaluate decides
// whether to proceed (create one more attempt) or escalate. This is
// deliberately NOT a static, definition-only "does this edge point
// backward in Order" check: §25.9's own audit -> fix example has fix
// appear AFTER audit in Order (an ordinary forward edge on its first
// firing), with the LOOP formed by fix's own separate ok -> audit edge --
// so "has this step already been attempted in this run" is the only
// signal that actually distinguishes a fresh advance from a re-fire,
// regardless of which direction either edge happens to point.
//
// # Auto-dispatch closes §25.6's own documented gap
//
// §25.6's OnTurnCompleted shipped with NextAdvance creating the next
// attempt's bookkeeping row but never dispatching a turn for it ("ready for
// whichever future Step ... adds the actual auto-continuation dispatch") --
// this Step is that future Step. dispatchNextAttempt below creates the
// attempt AND a real ordinary turn for it, directly (never through
// createTurnLocked/CreateTurnCore, which would re-run checks -- the
// open-turn/busy gate, the awaiting-plan gate -- that make no sense for a
// system/decision-triggered turn), mirroring decideplan.go's own identical
// choice to insert the post-approval implementation turn directly via
// turns.Create. The new turn needs no explicit dispatch trigger of its own:
// it lands Pending, and internal/app/sessionactor's own pre-existing
// "re-evaluate dispatch state right after commit" step (§3.3,
// handleEnsureDispatched, run unconditionally after every one of
// OnTurnCompleted's three call sites' own transactions commits already, and
// after the decide endpoint's own commit via httpapi.TriggerDispatch) picks
// it up exactly like any other queued turn -- no new dispatch-triggering
// code is needed here at all.

package workflowengine

import (
	"context"
	"fmt"

	"github.com/jackc/pgx/v5/pgtype"

	"github.com/narvidev/narvi/internal/adapters/outbound/postgres/sqlcgen"
	"github.com/narvidev/narvi/internal/domain/loopguard"
	"github.com/narvidev/narvi/internal/domain/turn"
	"github.com/narvidev/narvi/internal/domain/workflow"
	"github.com/narvidev/narvi/internal/platform"
)

// genericAdvanceInstruction is the prompt text a dispatched advance carries
// when the caller has no more specific text to relay (an approved step
// posted no advisory outcome summary, say) -- mirrors decideplan.go's own
// implementPlanPrompt precedent ("Implement the plan you just proposed.")
// one level more generic, since this Step's own advance is never
// plan-specific.
const genericAdvanceInstruction = "Continue the workflow: proceed with the next step now that the previous one was approved."

// Outcome is ApplyStepOutcome's own report of what happened.
type Outcome struct {
	// RunStatus is runRow's OWN resulting workflow_run_status (unchanged --
	// still 'running' -- on an ordinary advance; 'completed' on
	// NextComplete; 'needs_review' on NextEscalate).
	RunStatus string
	// DispatchedTurnID is the newly created turn's id, set iff this call
	// actually advanced to a new attempt (a fresh forward advance, or a
	// loopguard-permitted needs_fix re-fire) -- nil for NextComplete,
	// NextEscalate, or an escalated (breaker-tripped) would-be advance.
	DispatchedTurnID *pgtype.UUID
}

// ApplyStepOutcome consults workflow.NextStep for (def, currentStepID,
// outcome) and applies its consequence -- see this file's own top doc
// comment for the full loopguard/auto-dispatch design. sessionRow supplies
// BuildModelID (a dispatched attempt's own model, absent a step-level
// override) and the destination an escalation notice resolves against.
// advancePromptText is the prompt an ordinary NextAdvance dispatches with
// (never consulted for NextComplete/NextEscalate, and never used at all
// when loopguard escalates instead of advancing) -- callers pass the
// finishing step's own outcome summary when one exists, falling back to
// genericAdvanceInstruction otherwise -- computed once, here, so neither
// caller (completion.go, the HITL decide endpoint) duplicates the fallback
// logic.
//
// Returns an error only for a genuine store failure -- never for a
// "business as usual" escalation (that is a valid Outcome, not an error).
// OnTurnCompleted wraps this call with its own fail-open discipline (log
// and stop, per doc.go); the HITL decide endpoint propagates a real error
// into a 500 response, since a human is synchronously waiting on it.
func ApplyStepOutcome(ctx context.Context, deps Deps, runRow sqlcgen.WorkflowRun, def workflow.Definition, sessionRow sqlcgen.Session, currentStepID workflow.ID, outcome workflow.StepOutcomeStatus, outcomeSummary *string) (Outcome, error) {
	next, err := workflow.NextStep(def, currentStepID, outcome)
	if err != nil {
		return Outcome{}, fmt.Errorf("workflowengine: next step: %w", err)
	}

	switch next.Kind {
	case workflow.NextComplete:
		run, err := deps.Workflows.CompleteRun(ctx, runRow.ID)
		if err != nil {
			return Outcome{}, fmt.Errorf("workflowengine: complete workflow run: %w", err)
		}
		return Outcome{RunStatus: string(run.Status)}, nil

	case workflow.NextEscalate:
		return escalateRun(ctx, deps, runRow, sessionRow)

	case workflow.NextAdvance:
		promptText := genericAdvanceInstruction
		if outcomeSummary != nil && *outcomeSummary != "" {
			promptText = *outcomeSummary
		}
		return advance(ctx, deps, runRow, def, sessionRow, currentStepID, outcome, next.ToStepID, promptText)

	default:
		// Unreachable: workflow.NextKind is a closed 3-value enum and
		// NextStep never returns a fourth -- defended anyway rather than
		// silently doing nothing, mirroring authz.ErrUnknownAction's own
		// "default case is unreachable dead-code protection" stance.
		return Outcome{}, fmt.Errorf("workflowengine: unrecognized next-step kind %v", next.Kind)
	}
}

// advance implements ApplyStepOutcome's own NextAdvance case: the
// loopguard gate (only for a genuine needs_fix re-fire -- see this file's
// own top doc comment) followed by dispatchNextAttempt.
func advance(ctx context.Context, deps Deps, runRow sqlcgen.WorkflowRun, def workflow.Definition, sessionRow sqlcgen.Session, currentStepID workflow.ID, outcome workflow.StepOutcomeStatus, toStepID workflow.ID, promptText string) (Outcome, error) {
	logger := platform.Logger(ctx)

	toStep, ok := stepByID(def, toStepID)
	if !ok {
		// Should be unreachable: workflow.NextStep itself already refuses a
		// dangling edge target (ErrDanglingEdge) before ever returning
		// NextAdvance -- defended anyway rather than trusting that
		// invariant silently.
		return Outcome{}, fmt.Errorf("workflowengine: next step %q not found in definition %q", toStepID, def.ID)
	}
	toID, err := parseWorkflowID(toStepID)
	if err != nil {
		return Outcome{}, fmt.Errorf("workflowengine: parse next step id: %w", err)
	}

	if outcome == workflow.StepOutcomeNeedsFix {
		attempts, err := deps.Workflows.CountStepRunsForStepDefinition(ctx, runRow.ID, toID)
		if err != nil {
			return Outcome{}, fmt.Errorf("workflowengine: count step runs for step definition: %w", err)
		}
		if attempts > 0 {
			// A genuine re-fire (§25.9): toStepID already has at least one
			// prior attempt in this run, so creating another one now would
			// be this SAME needs_fix edge firing again -- consult the
			// circuit breaker before proceeding.
			decision := loopguard.Evaluate(
				loopguard.State{AttemptCount: int(attempts)},
				loopguard.Config{MaxAttempts: loopguard.DefaultMaxAttempts},
			)
			if decision.ShouldEscalate {
				logger.Info("workflowengine: circuit breaker escalated a re-firing needs_fix edge",
					"run_id", runRow.ID.String(), "from_step_id", string(currentStepID), "to_step_id", string(toStepID), "attempt_count", attempts)
				return escalateRun(ctx, deps, runRow, sessionRow)
			}
		}
	}

	turnID, err := dispatchNextAttempt(ctx, deps, runRow.ID, toStep, promptText, sessionRow)
	if err != nil {
		return Outcome{}, fmt.Errorf("workflowengine: dispatch next attempt: %w", err)
	}
	return Outcome{RunStatus: string(runRow.Status), DispatchedTurnID: &turnID}, nil
}

// DispatchSameStepRevision is the HITL decide endpoint's own EXCLUSIVE entry
// point for a "revise" verdict (§25.9, internal/adapters/inbound/httpapi):
// re-executes step (the SAME step definition the just-decided attempt
// belonged to -- never workflow.NextStep's own verdict, never a step
// resolved via an edge) with feedback folded in as the new attempt's own
// prompt, mirroring plan mode's own existing "prompt = feedback" mechanism
// one level more general (see decideworkflowstep.go's own top doc comment
// for the full "why" and the exact precedent this mirrors).
//
// Deliberately calls dispatchNextAttempt directly -- the SAME helper
// ApplyStepOutcome's own NextAdvance case uses -- WITHOUT ever going
// through ApplyStepOutcome/workflow.NextStep/loopguard.Evaluate. This is
// the structural circuit-breaker exemption §25.9 requires ("human-revision
// loops are EXEMPT from the circuit breaker ... never a bypassable flag"):
// there is no branch, flag, or parameter anywhere in this call chain that
// COULD route a revise through loopguard even by mistake -- the code path
// itself simply never calls the function that consults it, mirroring
// §24.6's own "a human's manual re-trigger is never subject to [the
// automatic budget]" exemption exactly.
func DispatchSameStepRevision(ctx context.Context, deps Deps, runID pgtype.UUID, step workflow.StepDefinition, feedback string, sessionRow sqlcgen.Session) (pgtype.UUID, error) {
	return dispatchNextAttempt(ctx, deps, runID, step, feedback, sessionRow)
}

// dispatchNextAttempt creates toStep's own NEXT workflow_step_runs attempt
// within runID and dispatches a real ordinary turn for it -- see this
// file's own top doc comment for why this bypasses createTurnLocked/
// CreateTurnCore entirely and needs no explicit dispatch-trigger call of
// its own.
func dispatchNextAttempt(ctx context.Context, deps Deps, runID pgtype.UUID, toStep workflow.StepDefinition, promptText string, sessionRow sqlcgen.Session) (pgtype.UUID, error) {
	toID, err := parseWorkflowID(toStep.ID)
	if err != nil {
		return pgtype.UUID{}, fmt.Errorf("parse to-step id: %w", err)
	}

	stepRun, err := deps.Workflows.CreateStepRun(ctx, runID, toID)
	if err != nil {
		return pgtype.UUID{}, fmt.Errorf("create next step run: %w", err)
	}

	// applyStep (dispatch.go) is this package's own existing "render a
	// step's PromptTemplate against incoming text, resolve its ModelID/
	// Effort override" logic -- reused verbatim here so a decision/re-fire-
	// driven dispatch renders identically to an ordinary new turn's own
	// resolution (§25.7/§29.8: modelID/effort fall back to sessionRow.
	// BuildModelID/BuildEffort, never a Narvi-side default of their own).
	res := applyStep(ctx, toStep, promptText, sessionRow.BuildModelID, sessionRow.BuildEffort, true, stepRun.ID)

	// F6 (adversarial review): the SAME shared gate createTurnLocked/
	// CreateSessionOnTx/DecidePlanOnTx also route through
	// (internal/domain/turn.MaybeInjectEpistemicPreamble) -- this call
	// site used to bypass it entirely (this file's own top doc comment
	// explains why dispatchNextAttempt never goes through createTurnLocked/
	// CreateTurnCore), so a machine-triggered workflow-advance turn NEVER
	// got the devil's-advocate preamble regardless of platform/session
	// config, and always recorded epistemic_outcome = NULL -- indistinguishable
	// from feature-off even with the check enabled, corrupting the
	// false-alarm-rate telemetry §20.2 exists to collect. planMode is
	// passed literally false, matching CreateTurnParams.PlanMode
	// immediately below (a workflow-engine-dispatched turn is never
	// plan-mode).
	dispatchedPrompt := turn.MaybeInjectEpistemicPreamble(deps.EpistemicCheckDefault, sessionRow.EpistemicCheckEnabled, false, res.Prompt)

	// correlationID (§12.2 item 1's own session-rail gap, migrations/
	// 000121_turns_correlation_id.up.sql) -- nil when the workflow engine's
	// own dispatch loop carries no live request context, mirroring every
	// other CreateTurnParams call site's identical convention.
	var correlationID *string
	if id, ok := platform.CorrelationIDFromContext(ctx); ok && id != "" {
		correlationID = &id
	}
	created, err := deps.Turns.Create(ctx, sqlcgen.CreateTurnParams{
		SessionID:     sessionRow.ID,
		Status:        sqlcgen.TurnStatusPending,
		Prompt:        &dispatchedPrompt,
		ModelID:       res.ModelID,
		Effort:        res.Effort,
		PlanMode:      false,
		CorrelationID: correlationID,
	})
	if err != nil {
		return pgtype.UUID{}, fmt.Errorf("create next turn: %w", err)
	}

	if err := deps.Workflows.AttachTurn(ctx, stepRun.ID, created.ID); err != nil {
		return pgtype.UUID{}, fmt.Errorf("attach turn to next step run: %w", err)
	}

	return created.ID, nil
}

// escalateRun applies workflow.NextEscalate's own consequence (§25.9):
// flips runRow to 'needs_review', then claims and (only if won) sends this
// run's own ONE-TIME escalation notice -- mirrors §24.6's own "never
// repeated" exemption mechanism exactly (migrations/000058_workflow_hitl.up.sql's
// own doc comment). Of any number of times this function runs for the SAME
// run (a genuine repeat escalation attempt, however that might arise), only
// the FIRST to win ClaimEscalationNotice's own guarded UPDATE ever enqueues
// a notice -- every later call finds claimed == 0 and sends nothing
// further, so hitting needs_review is observable exactly once.
func escalateRun(ctx context.Context, deps Deps, runRow sqlcgen.WorkflowRun, sessionRow sqlcgen.Session) (Outcome, error) {
	logger := platform.Logger(ctx)

	run, err := deps.Workflows.EscalateRun(ctx, runRow.ID)
	if err != nil {
		return Outcome{}, fmt.Errorf("workflowengine: escalate workflow run: %w", err)
	}

	claimed, err := deps.Workflows.ClaimEscalationNotice(ctx, runRow.ID)
	if err != nil {
		// The escalation itself already applies (in the caller's own
		// still-open transaction) -- a failure to even ATTEMPT the notice
		// claim is logged, never allowed to undo or block the escalation
		// itself (fail-open, mirroring OnTurnCompleted's own overall
		// discipline for this class of bookkeeping/notification concern).
		logger.Error("workflowengine: claim workflow run escalation notice failed", "run_id", runRow.ID.String(), "error", err)
		return Outcome{RunStatus: string(run.Status)}, nil
	}
	if claimed == 0 {
		// Already notified by an earlier escalation of this SAME run.
		return Outcome{RunStatus: string(run.Status)}, nil
	}

	if err := enqueueWorkflowNotice(ctx, deps, sessionRow, escalationNoticeText(runRow.ID)); err != nil {
		logger.Error("workflowengine: enqueue workflow run escalation notice failed", "run_id", runRow.ID.String(), "error", err)
	}
	return Outcome{RunStatus: string(run.Status)}, nil
}

// escalationNoticeText renders the ONE-TIME notice a human sees when a run
// reaches needs_review -- server-rendered, never re-parsed (§5.2), covering
// BOTH ways a run gets here (the circuit breaker tripping on a re-firing
// needs_fix edge, or workflow.NextStep's own plain fail-conservative
// default-escalate on an unrouted needs_fix/blocked outcome) with one
// honest message, since either way the run is now waiting on a human and
// nothing further will happen automatically.
func escalationNoticeText(runID pgtype.UUID) string {
	return fmt.Sprintf("This workflow run (%s) now needs your review: automatic progress has stopped (either a bounded retry loop reached its limit, or a step's outcome had no automatic next step configured). No further automatic action will be taken.", runID.String())
}

// awaitingDecisionNoticeText renders the notice a human sees when a step
// reaches awaiting_decision (§25.9's HITL gate) -- server-rendered, never
// re-parsed (§5.2), pointing at the real decide endpoint rather than
// claiming any deterministic reply keyword works here (unlike plan mode's
// own approve/reject/revise: text-reply support, this Step's own decide
// endpoint has no text-parsing entry point on Slack/Linear/GitHub at all --
// a deterministic GitHub `EditPrefix` keyword was drafted for this Step but
// never had a reachable call site (no built-in workflow ever parks a HITL
// decision post migration 000088_plan_builtin_passthrough's own corrective
// fix, §25.8/§25.9) and was removed as dead code rather than shipped
// speculatively; a future Step reintroduces an equivalent GitHub-comment
// affordance if/when the Phase 7 canvas editor (§25.12) makes a custom,
// HITL-carrying workflow definition reachable).
func awaitingDecisionNoticeText(runID, stepRunID pgtype.UUID) string {
	return fmt.Sprintf("A workflow step (run %s, attempt %s) is awaiting your decision. Approve, reject, or revise it via POST /api/workflow-runs/%s/steps/%s/decide.",
		runID.String(), stepRunID.String(), runID.String(), stepRunID.String())
}
