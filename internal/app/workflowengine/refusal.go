// This file (refusal.go) implements OnTurnRefused -- sessionactor's hook
// for an attempt the platform refused before it ever ran (its dispatch
// gate, credentialgate.go: the turn's model could run only on a personal
// provider link the session never resolves, technical plan §29.4).
//
// A refusal is a configuration problem, not a step outcome. OnTurnCompleted
// would read the refused turn as an implicit "blocked" and hand that to
// workflow.NextStep, which follows any blocked edge a definition wires -- a
// self edge included -- and queues the same step, with the same model,
// again: refused again, queued again, with nothing to end it (loopguard
// only bounds a needs_fix re-fire). So a refused attempt goes through the
// machine's own refusal row instead, workflow.NextStepAfterRefusal, which
// always escalates: the step run is finished failed, and the run waits on a
// human in needs_review with a notice naming the refusal, exactly as an
// unrouted blocked outcome escalates (§25.4, §25.9). Nothing is queued, so
// no later round starts the cycle again. A HITL-after gate is not consulted:
// there is no attempt for a person to approve or revise.

package workflowengine

import (
	"context"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/narvidev/narvi/internal/adapters/outbound/postgres/sqlcgen"
	"github.com/narvidev/narvi/internal/domain/turn"
	"github.com/narvidev/narvi/internal/domain/workflow"
	"github.com/narvidev/narvi/internal/platform"
)

// OnTurnRefused finalizes turnID's live step run, when the engine tracks
// one, as an attempt the platform refused before it ran, and applies
// workflow.NextStepAfterRefusal's verdict to its run. reason is the
// refusal as the session names it; it is carried into the escalation
// notice. sessionRow is the caller's own row, read in its transaction
// after the session-row lock, like OnTurnCompleted's. A person's standing
// stop wins, as it does in OnTurnCompleted: the run ends cancelled.
//
// Fail-open like the rest of this package (doc.go): a store failure is
// logged and the run left where it is, never allowed to undo the turn's
// own terminal write. Left there, it is still not re-queued.
func OnTurnRefused(ctx context.Context, deps Deps, sessionRow sqlcgen.Session, turnID pgtype.UUID, reason string) {
	logger := platform.Logger(ctx)
	workflows := deps.Workflows

	stepRun, err := workflows.GetLiveStepRunByTurnID(ctx, turnID)
	if err != nil {
		if !errors.Is(err, pgx.ErrNoRows) {
			logger.Warn("workflowengine: get live step run by turn id failed", "turn_id", turnID.String(), "error", err)
		}
		return
	}
	runRow, err := workflows.GetRun(ctx, stepRun.WorkflowRunID)
	if err != nil {
		logger.Error("workflowengine: get workflow run failed", "run_id", stepRun.WorkflowRunID.String(), "error", err)
		return
	}
	if sessionRow.StopRequestedAt.Valid {
		cancelStoppedRun(ctx, workflows, stepRun, runRow, turn.TriggerAbandon)
		return
	}

	def, err := LoadDefinition(ctx, workflows, runRow.WorkflowDefinitionID)
	if err != nil {
		logger.Error("workflowengine: load definition for refused turn failed", "run_id", runRow.ID.String(), "error", err)
		return
	}
	stepID := workflow.ID(stepRun.StepDefinitionID.String())
	next, err := workflow.NextStepAfterRefusal(def, stepID)
	if err != nil {
		logger.Error("workflowengine: next step after refusal failed", "run_id", runRow.ID.String(), "step_id", string(stepID), "error", err)
		return
	}

	if _, err := workflows.FinishStepRun(ctx, stepRun.ID, stepRunTerminalStatus(turn.TriggerAbandon), string(implicitOutcome(turn.TriggerAbandon))); err != nil {
		logger.Error("workflowengine: finish refused step run failed", "step_run_id", stepRun.ID.String(), "error", err)
		return
	}

	if next.Kind != workflow.NextEscalate {
		// Unreachable: NextStepAfterRefusal only ever escalates. Refused
		// rather than applied, so no future change to it can quietly start
		// advancing a refused run.
		logger.Error("workflowengine: refusal verdict is not an escalation; leaving run as-is",
			"run_id", runRow.ID.String(), "kind", next.Kind.String())
		return
	}
	if _, err := escalateRunWithNotice(ctx, deps, runRow, sessionRow, refusalNoticeText(runRow.ID, reason)); err != nil {
		logger.Error("workflowengine: escalate refused workflow run failed", "run_id", runRow.ID.String(), "error", err)
		return
	}
	logger.Info("workflowengine: workflow run escalated: its step was refused before it ran; no edge followed",
		"run_id", runRow.ID.String(), "step_run_id", stepRun.ID.String())
}

// refusalNoticeText renders the one-time notice a refused run's escalation
// sends -- server-rendered, never re-parsed (§5.2), naming the refusal
// rather than escalationNoticeText's retry-loop or unrouted-outcome causes.
func refusalNoticeText(runID pgtype.UUID, reason string) string {
	return fmt.Sprintf("This workflow run (%s) now needs your review: its step was refused before it ran (%s). No further automatic action will be taken until the configuration changes.", runID.String(), reason)
}
