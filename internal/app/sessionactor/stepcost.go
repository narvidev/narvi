// This file (stepcost.go) implements §25.15's own "cost does need
// persistence" half: handleSandboxEvent's own "step_finish" case
// (sandboxevent.go), called from INSIDE that function's own transact,
// AFTER appendRawEvent has already persisted the raw event verbatim
// (that file's own top comment: "persist ALWAYS, for every recognized
// event type"). A decode failure here therefore never needs to
// retroactively touch that persisted row -- mirroring recordBootTiming's
// own identical decode-failure handling (boottiming.go): warn, record
// nothing, never return an error that would roll back the WHOLE
// transact.
//
// §25.15 is explicit about what this is NOT: it is not, and must never
// become, internal/adapters/outbound/opencode's own adapter-local
// turnState.spentUSD accumulator (§7.1/§26.7) -- that lives and dies
// with one sandbox-agent process's own in-memory turn state, answers one
// sandbox-local question, and is never read from here. This file sums
// the SAME wire signal (step_finish.cost.usd) into an independent,
// Postgres-durable running total for a different, control-plane-visible
// purpose: turns.cost_usd (migration 000098), joined into a workflow
// step's own cost via workflow_step_runs.turn_id (queries/workflows.sql,
// ListWorkflowStepRunsForRun) exactly as turns.model_id already is for a
// step's model.

package sessionactor

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/jackc/pgx/v5"

	"github.com/narvidev/narvi/contracts/gen/go/sandboxws"
)

// recordStepFinishCost adds this step_finish event's own cost.usd (when
// present) onto the running total of whichever turn is currently
// processing for this session -- see RecordTurnStepCost's own generated
// doc comment (sqlcgen/turns.sql.go) for why that is one statement, and
// migrations/000099_turn_step_costs.up.sql for why it is idempotent on
// the step id.
//
// The wire's own reconnect-resend buffer (§6.1: "buffers (1000 events,
// evict oldest non-critical) and re-sends on reconnect") can replay an
// already-processed step_finish verbatim, so the write MUST be idempotent
// or a forced reconnect inflates the bill. The key for that is
// step_finish.stepId (§6.1), which is one per step.
//
// It is deliberately NOT the `inserted` flag appendRawEvent returns, which
// this function used to gate on. That flag answers "was this event's row
// new to the events table" -- a different question. Until a step_finish
// had a storage key of its own it was false for every step_finish ever
// sent: step_start and step_finish are two parts of the same assistant
// message and carry that message's id (translate.go: the token event is the
// sole part-derived event that uses its own part id), so step_start always
// claimed the row first, and gated that way no production step_finish
// reached the cost write at all. A step_finish is now stored under a key
// derived from its stepId (toolevent.go), but only while its turn is live,
// and the cost write keeps its own key, the stepId, so that whether the
// event's row is stored and whether its money is counted stay two answers
// to two questions.
func (a *Actor) recordStepFinishCost(ctx context.Context, tx pgx.Tx, raw json.RawMessage) error {
	var evt sandboxws.StepFinish
	if err := json.Unmarshal(raw, &evt); err != nil {
		// Defensive, not fatal -- mirrors recordBootTiming/
		// completeProcessingTurn's own identical decode-failure handling:
		// wshub's own read loop only peeks a small, permissive envelope
		// struct before ever constructing this SandboxEvent (dispatch.go),
		// so a genuinely malformed-per-schema step_finish CAN reach this
		// point in production. Returning an error here would roll back
		// this function's own caller's WHOLE transact, including the raw
		// event this same transact already persisted moments ago.
		a.logger.Warn("sessionactor: step_finish failed schema decode; persisted verbatim, recording no cost",
			"error", err)
		return nil
	}

	usd := (*float64)(evt.Cost.Usd)
	if usd == nil {
		// §6.1: cost.usd is OPTIONAL/nullable on the wire. Absent/null
		// means this step_finish carries no dollar figure at all -- never
		// treated as "$0", and never touching turns.cost_usd: that column
		// must stay NULL until a REAL figure (even a genuine $0.00) has
		// arrived, so "no cost yet" never renders as "free" (§25.15).
		return nil
	}

	if evt.StepId == "" {
		// §6.1 makes stepId required on step_finish, so an empty one is a
		// malformed event, not a shape this code should quietly absorb.
		// Without it there is no key to be idempotent on, and counting
		// money that a reconnect could count again is the worse of the two
		// errors -- so this drops the figure and says so.
		a.logger.Warn("sessionactor: step_finish carries no stepId; cost not recorded (no idempotency key)")
		return nil
	}

	affected, err := a.stores.turn.WithTx(tx).RecordStepCostUSD(ctx, a.sessionID, evt.StepId, *usd)
	if err != nil {
		return fmt.Errorf("sessionactor: record step cost: %w", err)
	}
	if affected == 0 {
		// Either this stepId was already counted (a reconnect replay --
		// the intended, healthy path) or the session has no turn in
		// processing. Both are ordinary; neither is worth an error, and
		// the two are not worth distinguishing, because the response to
		// each is the same: do not add the money again.
		a.logger.Debug("sessionactor: step_finish cost not applied; already counted or no turn processing",
			"step_id", evt.StepId)
	}
	return nil
}
