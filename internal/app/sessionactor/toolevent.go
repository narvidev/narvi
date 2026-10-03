package sessionactor

import (
	"context"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"

	"github.com/narvidev/narvi/internal/domain/eventkey"
)

// This file decides how a `tool_call`, `tool_result` or `step_finish`
// reaches the append-only event log (technical plan §6.1).
//
// # Why these three have keys of their own
//
// The runtime adapter gives every event it derives from a part of an
// assistant message that message's id as its wire messageId -- `token` is
// the one exception, using its part's id (translate.go,
// internal/adapters/outbound/opencode) -- and a message's `step-start`
// part comes before its other parts, so its `step_start` is the first
// event stored under that id. Every other event type is stored under its
// wire messageId, first wins (CreateEvent's ON CONFLICT, migrations/000019),
// so every `tool_call`, `tool_result` and `step_finish` of a real turn
// deduped onto its message's `step_start` row: none was ever stored, and
// since appendRawEvent broadcasts only a fresh insert, none reached a live
// page either. The Linear progress notice on a turn's first `tool_call`
// (progressnotify.go) never fired, the web timeline showed no tool call and
// no per-step cost, and the page's cost panel, which sums `step_finish`
// rows, read nothing on every real session. The step's cost reached
// turns.cost_usd all along, keyed on its stepId (stepcost.go).
//
// Each of the three now has a key of its own, derived from its type, its
// wire messageId and its own correlator -- callId or stepId
// (eventkey.StorageKey) -- so each is stored once, and a resend, carrying
// the same correlator, still dedupes. A key the control plane derives fixes
// every agent at once, an agent in an older snapshot or repo image too,
// where a per-event id the adapter minted would fix only agents built after
// it, and it leaves the wire, and what messageId means to every reader of
// it, as it is. Readers take the messageId from the payload, never from the
// storage key. Every other type keeps its key: re-keying a type already
// stored would make a sandbox's replay after the deploy store each such
// event again, at the tail of the log. These three have no stored rows to
// collide with.
//
// # Only while its turn is live
//
// A row of these three is added only while the turn that produced it is
// the one Processing, by `token`'s own rule (tokenframe.go, "Only while
// its turn is live"): readers place a row by its id, and one stored after
// its turn's execution_complete lands at the tail of the log, where the web
// timeline opens a new turn at it, shown as running. The sandbox-agent
// replays its buffer, best-effort events included, on every reconnect, and
// on its first reconnect after this file shipped it replays tool events the
// first-wins rule swallowed, of turns long over.
//
// The rule, read inside the actor's transaction, anchors each of the three
// on its message's `step_start`, the first event of the message, stored
// under its bare id:
//   - no turn Processing: no row.
//   - no row of the message under its bare id: no row. The message's
//     `step_start` was never stored, so nothing places the message in a
//     turn.
//   - that row at or below the Processing turn's dispatched_event_id: the
//     message entered the log before this turn existed, so it is an
//     earlier turn's. No row.
//   - otherwise the row is added (or, under a key already stored, deduped):
//     the message's `step_start` lies in this turn's window. A NULL
//     dispatched_event_id places no window, as for `token`, and any stored
//     `step_start` anchors the message.
//
// The anchor is what keeps a message the agent of an ended turn began
// after that turn ended out of the next turn's window: turn_deadline ends a
// turn without stopping its agent, and that message's `step_start`, sent
// while no turn is Processing, is stored nowhere
// (storedOnlyWhileATurnIsProcessing), so without the anchor its later tool
// calls, results and step end landed in the window of the turn dispatched
// next. The cost is a message whose
// `step_start` was lost -- none of the four is critical, and the
// sandbox-agent evicts best-effort events from its replay buffer when it
// is full -- which then stores none of its tool calls, results or step
// end. A `step_start` is a few short ids, which the sandbox-agent never
// cuts (it cuts only a `token`, a `tool_call` or a `tool_result`). The
// step's cost still reaches turns.cost_usd (stepcost.go), which reads the
// event and not its row.
//
// History stored before this file is not backfilled: those payloads were
// never kept. A sandbox alive at the deploy replays its buffered events on
// its next reconnect, and the rule stores only the Processing turn's, at
// the tail of its window.
//
// A `tool_call` or `tool_result` the sandbox-agent wrote cut to fit what a
// connection reads (technical plan §6.1) is stored under its key first and
// keeps its `cut`, and a whole copy written later, on a connection that
// reads it, dedupes onto it: first wins, as for every non-token event.

// messageFromEarlierTurn reports whether an assistant message whose row
// under its bare id has id messageRowID entered the log before the
// Processing turn whose dispatched_event_id is dispatchedEventID -- belongs
// to an earlier turn, so that a late `tool_call`, `tool_result` or
// `step_finish` of it must add no row. nil places no window.
func messageFromEarlierTurn(messageRowID int64, dispatchedEventID *int64) bool {
	return dispatchedEventID != nil && messageRowID <= *dispatchedEventID
}

// appendCorrelatedEvent stores one inbound `tool_call`, `tool_result` or
// `step_finish` inside tx under the key eventkey.StorageKey derives from it:
// the branch of handleSandboxEvent's "persist ALWAYS" step for the three
// types keyed by their correlator. Returns whether a row was inserted (and
// so queued for broadcast), exactly like appendRawEvent: false for a
// deduped resend, and for an event that arrives with no turn Processing,
// whose message has no stored `step_start`, or whose message belongs to an
// earlier turn (see "Only while its turn is live" above).
func (a *Actor) appendCorrelatedEvent(ctx context.Context, tx pgx.Tx, cmd SandboxEvent) (bool, error) {
	processing, err := a.stores.turn.WithTx(tx).GetProcessingTurnForSession(ctx, a.sessionID)
	if errors.Is(err, pgx.ErrNoRows) {
		a.logger.Debug("sessionactor: tool or step event with no turn processing; adding no row",
			"event_type", cmd.Type, "message_id", cmd.MessageID)
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("sessionactor: read processing turn for %s: %w", cmd.Type, err)
	}

	messageRowID, found, err := a.stores.event.WithTx(tx).IDForMessageID(ctx, a.sessionID, cmd.MessageID)
	if err != nil {
		return false, fmt.Errorf("sessionactor: read the stored row of %s's message: %w", cmd.Type, err)
	}
	if !found {
		a.logger.Debug("sessionactor: tool or step event of a message with no stored step_start; adding no row",
			"event_type", cmd.Type, "message_id", cmd.MessageID, "turn_id", processing.ID.String())
		return false, nil
	}
	if messageFromEarlierTurn(messageRowID, processing.DispatchedEventID) {
		a.logger.Debug("sessionactor: tool or step event of a message from an earlier turn; adding no row",
			"event_type", cmd.Type, "message_id", cmd.MessageID, "message_row_id", messageRowID,
			"turn_id", processing.ID.String())
		return false, nil
	}
	return a.appendRawEvent(ctx, tx, cmd.Type, eventkey.StorageKey(cmd.Type, cmd.MessageID, cmd.CallID, cmd.StepID), cmd.Raw)
}

// storedOnlyWhileATurnIsProcessing reports whether eventType is the one
// other turn-scoped type the page opens a turn at that the agent can still
// send after the control plane has ended its turn: a `step_start`.
// turn_deadline ends a turn without stopping its agent, which can begin a
// new message and send its `step_start`, and the page, which ends the turn
// it is showing at the turn's synthetic execution_complete, opened a turn
// of its own at it -- one that never ended, its calls running and its
// composer locked. A `step_start` is never sent while no turn is Processing
// but late, so with no turn Processing it adds no row, as `token` frames
// and the three correlated types do not (appendLiveTurnEvent), and the
// rest of its message, anchored on it, adds none either
// (appendCorrelatedEvent).
//
// A `sub_task_start` is stored however late, as a `sub_task_finish` is: a
// timed-out turn's late review verdict is accepted (PostReviewVerdict), and
// the counter-review and fact-check corroboration of technical plan §26.4
// reads that turn's sub-task starts from its dispatched_event_id up to the
// next dispatched turn's, so a late start counts while no later turn has
// been dispatched; one stored after that lies past the bound, as it does on
// a control plane that never gated it. A finish stored without its start
// reads as uncorroborated.
// The page reads a late one into the turn that ended, and opens no turn at
// it (web/src/session/timelineModel.ts).
func storedOnlyWhileATurnIsProcessing(eventType string) bool {
	return eventType == "step_start"
}

// appendLiveTurnEvent stores one inbound `step_start` inside tx under its
// wire messageId, as every other type is stored, but only while a turn is
// Processing (storedOnlyWhileATurnIsProcessing). Returns whether a row was
// inserted, exactly like appendRawEvent.
func (a *Actor) appendLiveTurnEvent(ctx context.Context, tx pgx.Tx, cmd SandboxEvent) (bool, error) {
	if _, err := a.stores.turn.WithTx(tx).GetProcessingTurnForSession(ctx, a.sessionID); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			a.logger.Debug("sessionactor: turn-scoped event with no turn processing; adding no row",
				"event_type", cmd.Type, "message_id", cmd.MessageID)
			return false, nil
		}
		return false, fmt.Errorf("sessionactor: read processing turn for %s: %w", cmd.Type, err)
	}
	return a.appendRawEvent(ctx, tx, cmd.Type, cmd.MessageID, cmd.Raw)
}
