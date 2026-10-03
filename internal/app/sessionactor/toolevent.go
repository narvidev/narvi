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
// The rule, read inside the actor's transaction:
//   - no turn Processing: no row.
//   - the event's message already has a row under its bare id -- its
//     `step_start`, the first event of the message -- at or below the
//     Processing turn's dispatched_event_id: the message entered the log
//     before this turn existed, so it is an earlier turn's. No row.
//   - otherwise the row is added (or, under a key already stored, deduped):
//     the message's `step_start` lies in this turn's window, or no row of
//     the message is stored yet and a turn is Processing.
//
// A late event whose message has no row at all cannot be placed, as a
// `token` part none of whose frames was stored cannot: it is stored in the
// window of whichever turn is Processing when it arrives. A NULL
// dispatched_event_id places no window, as for `token`.
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
// deduped resend, and for an event that arrives with no turn Processing or
// whose message belongs to an earlier turn (see "Only while its turn is
// live" above).
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
	if found && messageFromEarlierTurn(messageRowID, processing.DispatchedEventID) {
		a.logger.Debug("sessionactor: tool or step event of a message from an earlier turn; adding no row",
			"event_type", cmd.Type, "message_id", cmd.MessageID, "message_row_id", messageRowID,
			"turn_id", processing.ID.String())
		return false, nil
	}
	return a.appendRawEvent(ctx, tx, cmd.Type, eventkey.StorageKey(cmd.Type, cmd.MessageID, cmd.CallID, cmd.StepID), cmd.Raw)
}
