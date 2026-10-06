// This file (dispatch.go) implements ResolveStepForNewTurn/AttachTurn --
// the createTurnLocked wiring point (§25.6, doc.go). Deliberately NOT
// wired into internal/adapters/inbound/httpapi's CreateSessionOnTx (create.go),
// which inserts a session's own very first turn directly (its own
// `turns.WithTx(tx).Create(...)` call, bypassing createTurnLocked/
// CreateTurnCore entirely) -- a genuine, pre-existing third turn-creation
// call site the task's own named wiring points never covered. Two reasons
// this is the right, conservative scope boundary rather than a gap to
// close in this same Step:
//
//  1. sessions.intent_decision (what resolveLane reads) is not always even
//     KNOWN yet at that point: CreateSession (create.go) calls
//     recordExplicitIntentDecision AFTER CreateSessionCore -- i.e. after
//     that session's own first turn already committed -- for the "web"
//     surface specifically. Wiring this call site correctly would need a
//     genuinely different Lane-resolution input (whatever the caller
//     already knows, before classification has even run), not a reuse of
//     this file's own "read it back from the row" approach.
//  2. It has ZERO behavioral consequence for the zero-config proof this
//     Step's own exit criterion cares about: the built-in review/request
//     workflows' single step is PromptTemplate "{{prompt}}"/ModelID nil --
//     a pure identity transform either way -- so a session's first turn
//     dispatches an IDENTICAL sandboxws.Prompt whether or not this package
//     ever sees it. The only real cost is observability: that one turn's
//     own workflow_runs/workflow_step_runs bookkeeping row never gets
//     created. Documented here, and in this Step's own PR description, as
//     a deliberate, narrow, low-risk gap -- not silently glossed over.

package workflowengine

import (
	"context"
	"errors"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/narvidev/narvi/internal/adapters/outbound/postgres"
	"github.com/narvidev/narvi/internal/adapters/outbound/postgres/sqlcgen"
	"github.com/narvidev/narvi/internal/domain/intent"
	"github.com/narvidev/narvi/internal/domain/workflow"
	"github.com/narvidev/narvi/internal/platform"
)

// Resolution is ResolveStepForNewTurn's own result -- the turn to actually
// build, plus enough bookkeeping context for AttachTurn to finish the job
// once the real turn row exists.
type Resolution struct {
	// Prompt is the text to store as the new turn's own prompt -- the
	// resolved step's PromptTemplate rendered against the caller's own
	// text (identical to callerPrompt for every §25.8 built-in shape,
	// whose template is the literal passthrough "{{prompt}}").
	Prompt string
	// ModelID is the model id to store on the new turn -- the resolved
	// step's own ModelID when non-nil (§25.7's per-step override),
	// otherwise the caller's own modelID UNCHANGED (§25.6's zero-config
	// proof: nil stays nil, inheriting turns.model_id/sessions.
	// build_model_id exactly as today, no override).
	ModelID *string
	// Effort mirrors ModelID exactly, one field over (§29.8's
	// "workflow engine echo"): the resolved step's own Effort when
	// non-nil, otherwise the caller's own effort UNCHANGED.
	Effort *string

	// Tracked reports whether CreateStepRun just created a NEW
	// workflow_step_runs attempt for this turn -- AttachTurn is a no-op
	// unless this is true. False means this call resolved a step's
	// PromptTemplate/ModelID but deliberately created no bookkeeping row --
	// see ResolveStepForNewTurn's own doc comment for the cases this
	// covers (a live HITL-awaiting or still-in-flight step, or an internal
	// failure that made this call fail open).
	Tracked bool
	// StepRunID is the newly created attempt's id, valid iff Tracked.
	StepRunID pgtype.UUID
}

// passthrough is Resolution's own safe, always-available fallback: the
// caller's prompt/modelID/effort entirely unchanged, untracked -- exactly
// what createTurnLocked did before this Step existed.
func passthrough(callerPrompt string, callerModelID, callerEffort *string) Resolution {
	return Resolution{Prompt: callerPrompt, ModelID: callerModelID, Effort: callerEffort}
}

// ResolveStepForNewTurn is createTurnLocked's own new first step (§25.6):
// given the session row already locked/read in that SAME transaction and
// the caller's own raw prompt/modelID, resolves which StepDefinition
// governs this new turn and returns the prompt/modelID to actually build
// it with. Never returns an error -- see doc.go's own "fail-open is
// load-bearing" section; any internal failure logs and degrades to
// passthrough(callerPrompt, callerModelID), exactly today's behavior.
//
// Three cases, in order:
//
//  1. No WorkflowRun is currently 'running' for this session (never
//     started, or the last one already completed/failed/cancelled/
//     needs_review -- migration 000057's own "a run parked in needs_review
//     must not freeze the session" invariant means a fresh run starts here
//     too): resolve (lane, binding, definition), start a NEW run pinned to
//     it, and create the FIRST step's (smallest Order) own attempt --
//     Tracked=true. This is the common case: every ordinary review/request
//     turn (§25.8: single-step, completes immediately once its turn
//     finishes) reaches this branch every single time.
//  2. A run IS running, and its own live (running/awaiting_decision)
//     step-run's status is 'awaiting_decision': a HITLAfter-gated step
//     waiting on a human. No BUILT-IN workflow reaches this case as of
//     migration 000088_plan_builtin_passthrough (§25.9's own corrective
//     follow-up, §25.8/§25.9: the built-in plan workflow's original
//     hitl_after step 1 + needs_fix self-loop, migration 000057's seed,
//     was a genuine design incoherence -- it silently double-parked a
//     workflow-level HITL gate against classic plan mode's own
//     pre-existing, unconditional persisted-state awaiting-plan gate on
//     every plan-mode session, so it was corrected to a single-step
//     passthrough, matching review/request) -- this case now exists
//     purely for a CUSTOM (non-built-in) workflow definition's own
//     hitl_after step, e.g. one authored via the Phase 7 canvas editor
//     (§25.12). §25.9 owns the actual decision/re-execution machinery;
//     creating a new attempt here without it would half-reimplement it
//     ahead of scope. Resolves that SAME live step's PromptTemplate/
//     ModelID (inert for every built-in), Tracked=false.
//  3. A run IS running, and its own live step-run's status is 'running'
//     (its own turn has not finished yet -- reachable only via
//     CreateTurnPolicy.AlwaysQueue, GitHub bot ingress's own
//     mention-coalescing backlog): resolves that SAME step identically,
//     Tracked=false -- creating a second live attempt here would violate
//     workflow_step_runs_one_live_per_run (migration 000057).
//
// A fourth case -- a running run with NO live step-run at all -- also
// degrades to passthrough, untracked. It is reachable while the run's
// advance is held by the autonomy freeze (technical plan §40.2,
// release.go), logged at Info: a person's turn is never held, and the
// advance is applied once the freeze lifts. Otherwise it should be
// unreachable given OnTurnCompleted's own invariants (completion.go), and
// is logged as a warning.
func ResolveStepForNewTurn(ctx context.Context, workflows *postgres.WorkflowStore, sessionRow sqlcgen.Session, callerPrompt string, callerModelID, callerEffort *string) Resolution {
	logger := platform.Logger(ctx)

	runRow, err := workflows.GetRunningRunForSession(ctx, sessionRow.ID)
	switch {
	case err == nil:
		return resolveWithinRunningRun(ctx, workflows, runRow, callerPrompt, callerModelID, callerEffort)
	case errors.Is(err, pgx.ErrNoRows):
		return startNewRun(ctx, workflows, sessionRow, callerPrompt, callerModelID, callerEffort)
	default:
		logger.Warn("workflowengine: get running run for session failed; passing turn through unchanged",
			"session_id", sessionRow.ID.String(), "error", err)
		return passthrough(callerPrompt, callerModelID, callerEffort)
	}
}

// startNewRun implements case 1 of ResolveStepForNewTurn's own doc
// comment above.
func startNewRun(ctx context.Context, workflows *postgres.WorkflowStore, sessionRow sqlcgen.Session, callerPrompt string, callerModelID, callerEffort *string) Resolution {
	logger := platform.Logger(ctx)

	lane := resolveLane(sessionRow.IntentDecision)
	repoFullName, hasRepo := repoFullNameFromSessionRepos(sessionRow.Repos)

	binding, err := resolveBinding(ctx, workflows, lane, repoFullName, hasRepo)
	if err != nil {
		logger.Warn("workflowengine: resolve binding failed; passing turn through unchanged",
			"session_id", sessionRow.ID.String(), "lane", string(lane), "error", err)
		return passthrough(callerPrompt, callerModelID, callerEffort)
	}

	def, err := LoadDefinition(ctx, workflows, binding.WorkflowDefinitionID)
	if err != nil {
		logger.Warn("workflowengine: load definition failed; passing turn through unchanged",
			"session_id", sessionRow.ID.String(), "definition_id", binding.WorkflowDefinitionID.String(), "error", err)
		return passthrough(callerPrompt, callerModelID, callerEffort)
	}
	if verr := workflow.ValidateDefinition(def); verr != nil {
		logger.Error("workflowengine: resolved definition fails validation; passing turn through unchanged",
			"definition_id", binding.WorkflowDefinitionID.String(), "error", verr)
		return passthrough(callerPrompt, callerModelID, callerEffort)
	}

	first := firstStepByOrder(def)
	firstID, err := parseWorkflowID(first.ID)
	if err != nil {
		logger.Error("workflowengine: parse first step id failed; passing turn through unchanged", "error", err)
		return passthrough(callerPrompt, callerModelID, callerEffort)
	}

	run, err := workflows.CreateRun(ctx, sessionRow.ID, string(lane), binding.WorkflowDefinitionID, int32(def.Version))
	if err != nil {
		logger.Error("workflowengine: create workflow run failed; passing turn through unchanged",
			"session_id", sessionRow.ID.String(), "error", err)
		return passthrough(callerPrompt, callerModelID, callerEffort)
	}

	stepRun, err := workflows.CreateStepRun(ctx, run.ID, firstID)
	if err != nil {
		logger.Error("workflowengine: create step run failed; passing turn through unchanged",
			"run_id", run.ID.String(), "error", err)
		return passthrough(callerPrompt, callerModelID, callerEffort)
	}

	return applyStep(ctx, first, callerPrompt, callerModelID, callerEffort, true, stepRun.ID)
}

// resolveWithinRunningRun implements cases 2/3 of ResolveStepForNewTurn's
// own doc comment above -- and its fourth case (no live step-run at all:
// an advance the autonomy freeze holds, or a defensive fallback).
func resolveWithinRunningRun(ctx context.Context, workflows *postgres.WorkflowStore, runRow sqlcgen.WorkflowRun, callerPrompt string, callerModelID, callerEffort *string) Resolution {
	logger := platform.Logger(ctx)

	liveStepRun, err := workflows.GetLiveStepRunForRun(ctx, runRow.ID)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			// Technical plan §40.2: a run whose advance the autonomy
			// freeze holds (release.go) has no live step-run until the
			// freeze lifts. A person's turn meanwhile is never held: it
			// passes through untracked, and the held advance is applied
			// after it, queued behind it.
			if _, holdErr := workflows.GetAdvanceHold(ctx, runRow.ID); holdErr == nil {
				logger.Info("workflowengine: advance held by the autonomy freeze; passing turn through untracked",
					"run_id", runRow.ID.String())
				return passthrough(callerPrompt, callerModelID, callerEffort)
			}
		}
		// Defensive: a running run that holds no advance should always
		// have exactly one live step-run (OnTurnCompleted's own
		// invariant, completion.go) -- should be unreachable in practice.
		logger.Warn("workflowengine: running run has no live step-run; passing turn through unchanged",
			"run_id", runRow.ID.String(), "error", err)
		return passthrough(callerPrompt, callerModelID, callerEffort)
	}

	def, err := LoadDefinition(ctx, workflows, runRow.WorkflowDefinitionID)
	if err != nil {
		logger.Warn("workflowengine: load definition for running run failed; passing turn through unchanged",
			"run_id", runRow.ID.String(), "error", err)
		return passthrough(callerPrompt, callerModelID, callerEffort)
	}

	step, ok := stepByID(def, workflow.ID(liveStepRun.StepDefinitionID.String()))
	if !ok {
		logger.Warn("workflowengine: live step-run names a step not in its own definition; passing turn through unchanged",
			"run_id", runRow.ID.String(), "step_definition_id", liveStepRun.StepDefinitionID.String())
		return passthrough(callerPrompt, callerModelID, callerEffort)
	}

	// Whether awaiting_decision (a HITL gate -- as of migration
	// 000088_plan_builtin_passthrough, reachable only via a CUSTOM
	// workflow's own hitl_after step, never a built-in; see this
	// function's own doc comment above) or running (its own turn still in
	// flight, only reachable via AlwaysQueue) -- either way this
	// deliberately creates no new attempt: resolve the SAME step's
	// template/model/effort, untracked.
	return applyStep(ctx, step, callerPrompt, callerModelID, callerEffort, false, pgtype.UUID{})
}

// applyStep renders step's own PromptTemplate against callerPrompt and
// resolves its own ModelID/Effort -- the one place both §25.6's
// passthrough-template and §25.7's per-step-model-override logic (and its
// §8.8 effort twin, §29.8) actually run, shared by every
// ResolveStepForNewTurn branch above.
func applyStep(ctx context.Context, step workflow.StepDefinition, callerPrompt string, callerModelID, callerEffort *string, tracked bool, stepRunID pgtype.UUID) Resolution {
	rendered, err := intent.AssembleTemplate(step.PromptTemplate, map[string]string{"prompt": callerPrompt})
	if err != nil {
		// A custom (non-built-in) step's own malformed PromptTemplate --
		// never reachable for any §25.8 built-in shape, whose template is
		// always the literal "{{prompt}}". Fail open to the caller's own
		// raw text rather than ever blocking dispatch on it.
		platform.Logger(ctx).Error("workflowengine: render step prompt template failed; using the caller's own text unchanged",
			"step_id", string(step.ID), "error", err)
		rendered = callerPrompt
	}

	modelID := callerModelID
	if step.ModelID != nil {
		modelID = step.ModelID
	}

	effort := callerEffort
	if step.Effort != nil {
		effort = step.Effort
	}

	return Resolution{Prompt: rendered, ModelID: modelID, Effort: effort, Tracked: tracked, StepRunID: stepRunID}
}

// AttachTurn backfills res's own newly created step-run with turnID's real
// id, once the turn insert createTurnLocked performs right after calling
// ResolveStepForNewTurn has actually succeeded -- a no-op when
// !res.Tracked. Logs and swallows any failure (fail-open, see doc.go): a
// step-run's own turn_id is bookkeeping metadata, never allowed to fail
// the turn creation it describes.
func AttachTurn(ctx context.Context, workflows *postgres.WorkflowStore, res Resolution, turnID pgtype.UUID) {
	if !res.Tracked {
		return
	}
	if err := workflows.AttachTurn(ctx, res.StepRunID, turnID); err != nil {
		platform.Logger(ctx).Error("workflowengine: attach turn to step run failed",
			"step_run_id", res.StepRunID.String(), "turn_id", turnID.String(), "error", err)
	}
}
