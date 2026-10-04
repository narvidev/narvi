// This file (withdrawn.go) implements OnTurnWithdrawn -- sessionactor's
// hook for an attempt whose request was withdrawn before it ran: technical
// plan §24.9's owed review request, dropped because its requester may no
// longer have it run, because it met a moved pull request more times in a
// row than the bound allows, because its session claims no pull request,
// or because a person's stop predates it.
//
// A withdrawal is neither a step outcome nor a configuration problem.
// OnTurnCompleted would read the attempt as an implicit "blocked" and hand
// that to workflow.NextStep: a definition's blocked edge -- a self edge
// included -- would queue the step again with no new input, undoing the
// drop, and a definition with none would escalate the run with a notice of
// its own, a second public word beside the one the drop already posted.
// OnTurnRefused would escalate with a notice too, and leave the run waiting
// on a person's decision about work nobody is owed any more. So the run ends
// the way a person's stop ends it (cancelStoppedRun): the step attempt
// finished cancelled, the run cancelled, no edge followed, no gate
// consulted, no notice posted -- the caller has told the person why, once.

package workflowengine

import (
	"context"
	"errors"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/narvidev/narvi/internal/adapters/outbound/postgres"
	"github.com/narvidev/narvi/internal/domain/turn"
	"github.com/narvidev/narvi/internal/platform"
)

// OnTurnWithdrawn ends, cancelled, the workflow run that tracks turnID's
// live step attempt, when the engine tracks one (see this file's top
// comment): the attempt finished cancelled and the run cancelled, with no
// edge followed and no notice posted. A turn the engine never tracked, or
// whose attempt already ended, is left alone. workflows is the caller's
// store, bound to its transaction. Fail-open like the rest of this package
// (doc.go): a store failure is logged and the run left where it is, never
// allowed to undo the caller's own writes; left there, nothing is queued.
func OnTurnWithdrawn(ctx context.Context, workflows *postgres.WorkflowStore, turnID pgtype.UUID) {
	logger := platform.Logger(ctx)
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
	cancelRun(ctx, workflows, stepRun, runRow, turn.TriggerCancel, "a withdrawn request")
}
