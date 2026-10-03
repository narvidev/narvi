// This file (progressnotify.go) implements audit finding M16
// ("completeness", internal/adapters/outbound/linearapi/doc.go): the
// "progressive" half of §8.10 that package's own top comment explicitly
// deferred to "a future Step that builds the real Notifier/outbox
// consumer" -- §5.1 shipped that consumer, but nobody ever came back
// to layer a richer, asynchronous, retried mid-turn AgentActivity update
// on top of it. Before this batch, the ONLY outbox notification any
// Linear-origin session's own agent session ever got was a SINGLE
// terminal one, enqueued by outboxenqueue.go's own enqueueOutboxNotification
// at turn completion (success/fail/cancel) -- never mid-turn. Between the
// synchronous CreateThoughtActivity ack §8.10's ingress handler posts at
// session creation (linearapi/activity.go, doc.go) and that one terminal
// notification, a Linear user watching the session's own native
// AgentActivity feed saw nothing at all, no matter how long the turn ran.
//
// # Milestone chosen
//
// This audit fix deliberately instruments exactly ONE milestone rather
// than every possible turn phase -- a bounded presence signal, not a
// detailed progress feed (that richer surface is left to a future UI
// Step, not this fix). The milestone picked: the FIRST tool_call wire event
// of a turn
// (contracts/sandbox-ws/v1/events.schema.json's own ToolCall def) -- a
// hard, discrete, already-flowing signal that unambiguously means "the
// agent is now actively working". This was chosen over a boot-progress
// phase (e.g. a specific late "boot_progress" phase, or the Booting->Ready
// transition) for two reasons: (1) the sandbox's own status column
// already surfaces boot/connecting state to any client reading session
// state today, so a Linear user gains nothing new from a boot milestone
// that a terminal-outcome-only feed doesn't already imply once the turn
// eventually finishes; (2) "the sandbox finished booting" only says the
// SANDBOX is ready, not that the agent is doing anything with THIS
// prompt -- a tool_call is the first wire event that is unambiguously
// scoped to the turn itself, exactly the kind of update a Linear user
// watching a native AgentActivity feed wants to see between "started" and
// "done".
//
// # Scope containment (Linear-only)
//
// M16 is explicitly Linear-scoped (§8.10 names Linear specifically): a
// GitHub PR's own check-run/comment history and a Slack thread's own
// visible message history each already show progress natively, unlike a
// Linear AgentSession's activity feed, which shows nothing between a
// `thought` ack and a final outcome without this batch.
// maybeEnqueueLinearProgress below is therefore
// gated on sessionRow.SpawnSource == SessionSpawnSourceLinear and is a
// deliberate no-op for every other origin (including 'web', which has no
// external channel to notify at all, exactly like enqueueOutboxNotification's
// own identical guard).
//
// # Wiring
//
// handleSandboxEvent (sandboxevent.go) calls maybeEnqueueLinearProgress
// from its own cmd.Type == "tool_call" case, INSIDE the same transact
// that already persisted this event via appendRawEvent moments ago --
// following outboxenqueue.go's own established "write the outbox row in
// the SAME transaction as the state change" rule (§5.1), even though the
// "state change" here is only the turns.progress_notified_at marker
// itself (migrations/000038_turn_progress_notified.up.sql), not a domain
// state-machine transition.
//
// # Double-enqueue guards (two, independent, both reused rather than invented)
//
// A resent/duplicate underlying wire event must not enqueue a duplicate
// progress notification, and neither must a second, later, genuinely
// DIFFERENT tool_call event in the same turn (an agent that calls
// multiple tools in one turn is the common case, not the exception) --
// two separate concerns, each already answered by existing bookkeeping,
// deliberately NOT collapsed into one new, independently-invented dedupe
// mechanism:
//
//  1. insertedFresh (appendCorrelatedEvent's own result, toolevent.go,
//     threaded through by handleSandboxEvent) is false for a wire-level
//     redelivery of an ALREADY-processed tool_call -- §6.1's own
//     buffer/resend-on-reconnect protocol ("sender buffers 1000 events...
//     re-sends on reconnect until acked; receiver dedupes by
//     upsert-on-messageId") applies to every event type, not just the 6
//     critical ones, so a tool_call CAN be redelivered -- and for one that
//     arrives with no turn live. A deduped resend must never re-run this
//     file's own logic at all -- checked FIRST, before any DB read, so a
//     redelivery costs nothing beyond the append-only persist
//     handleSandboxEvent already always does. A tool_call is stored under
//     a key derived from its callId. Under its wire messageId alone, its
//     enclosing assistant message's id, which that message's step_start
//     stores first, insertedFresh was false for every real tool_call and
//     this notice never fired; its tests had minted a fresh id per event.
//  2. turns.progress_notified_at is a per-TURN marker, set at MOST once,
//     atomically, by TurnStore.MarkProgressNotified's own conditional
//     UPDATE ... WHERE progress_notified_at IS NULL (queries/turns.sql) --
//     guarding against the (expected, common) case of a SECOND,
//     genuinely-distinct tool_call event later in the SAME turn, which
//     insertedFresh alone cannot catch (that later event is itself a
//     perfectly genuine, non-duplicate insert). The session actor
//     processes one command at a time per session (§2's single-writer
//     rule), so no concurrent-update race is actually possible here in
//     practice -- the atomic, guarded UPDATE is still used rather than a
//     plain read-then-write, both because it is already the established
//     idiom for exactly this shape of guard (ApprovePlanIfAwaitingApproval/
//     RejectPlanIfAwaitingApproval, queries/plans.sql) and because it
//     costs nothing extra to be correct even under a future change to that
//     single-writer assumption.

package sessionactor

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/narvidev/narvi/internal/adapters/outbound/linearapi"
	"github.com/narvidev/narvi/internal/adapters/outbound/postgres/sqlcgen"
	"github.com/narvidev/narvi/internal/app/ports"
	"github.com/narvidev/narvi/internal/platform"
)

// linearProgressText is the fixed, human-readable body posted as the
// mid-turn "thought" AgentActivity's own content -- deliberately generic
// (no tool name/input echoed back): this fix's own scope (see this file's
// top doc comment) is a presence signal ("the agent is working"), not a
// detailed live transcript of tool calls.
const linearProgressText = "Still working on it -- the agent is now actively using tools to complete this turn."

// maybeEnqueueLinearProgress implements this file's own top doc comment.
// Called from handleSandboxEvent's own cmd.Type == "tool_call" case, tx is
// the SAME already-open transaction that call's own appendRawEvent just
// persisted this tool_call event in. insertedFresh is that appendRawEvent
// call's own row.Inserted result; now is the timestamp
// progress_notified_at is stamped with when this call is the one that
// wins the per-turn guard.
//
// A no-op, never an error, for every case where there is genuinely
// nothing to enqueue: insertedFresh false (a deduped wire-level resend);
// a session whose spawn_source is not Linear (this finding's own explicit
// scope, §8.10); no turn currently Processing (defensive -- a tool_call
// should only ever arrive while a turn is genuinely Processing, but a
// redelivery arriving after that turn has already completed is a
// legitimate, if unusual, possibility); a Linear-origin session missing
// its own reverse-lookup linear_agent_sessions row (defensive, logged --
// mirrors enqueueOutboxNotification's own identical posture,
// outboxenqueue.go); and a turn whose progress_notified_at is already set
// (a later, genuinely-distinct tool_call in the same turn, or a race).
func (a *Actor) maybeEnqueueLinearProgress(ctx context.Context, tx pgx.Tx, insertedFresh bool, now pgtype.Timestamptz) error {
	if !insertedFresh {
		return nil
	}

	sessionRow, err := a.stores.session.WithTx(tx).Get(ctx, a.sessionID)
	if err != nil {
		return fmt.Errorf("sessionactor: enqueue linear progress: get session: %w", err)
	}
	if sessionRow.SpawnSource != sqlcgen.SessionSpawnSourceLinear {
		return nil
	}

	turns, err := a.stores.turn.WithTx(tx).ListForSession(ctx, a.sessionID)
	if err != nil {
		return fmt.Errorf("sessionactor: enqueue linear progress: list turns: %w", err)
	}
	processing, ok := findProcessingTurn(turns)
	if !ok {
		a.logger.Warn("sessionactor: enqueue linear progress: tool_call arrived with no turn in processing; ignoring")
		return nil
	}

	affected, err := a.stores.turn.WithTx(tx).MarkProgressNotified(ctx, processing.ID, now)
	if err != nil {
		return fmt.Errorf("sessionactor: enqueue linear progress: mark turn progress notified: %w", err)
	}
	if affected == 0 {
		// Already fired for this turn -- see this file's own top doc
		// comment's guard (2).
		return nil
	}

	row, err := a.stores.linearAgentSession.WithTx(tx).GetBySessionID(ctx, a.sessionID)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			a.logger.Warn("sessionactor: enqueue linear progress: linear-origin session has no linear_agent_sessions row; skipping")
			return nil
		}
		return fmt.Errorf("sessionactor: enqueue linear progress: get linear agent session: %w", err)
	}

	payload := linearapi.ProgressPayload{
		AgentSessionID: row.AgentSessionID,
		OrganizationID: row.OrganizationID,
		Text:           linearProgressText,
	}
	rawPayload, err := json.Marshal(payload)
	if err != nil {
		return fmt.Errorf("sessionactor: enqueue linear progress: marshal payload: %w", err)
	}

	// Correlation ID propagation -- mirrors enqueueOutboxNotification's own
	// identical convention (outboxenqueue.go) exactly.
	var correlationID *string
	if id, ok := platform.CorrelationIDFromContext(ctx); ok && id != "" {
		correlationID = &id
	}

	if _, err := a.stores.outbox.WithTx(tx).Create(ctx, sqlcgen.CreateOutboxEntryParams{
		SessionID:     a.sessionID,
		Kind:          string(ports.NotificationKindLinearProgress),
		Payload:       rawPayload,
		CorrelationID: correlationID,
	}); err != nil {
		return fmt.Errorf("sessionactor: enqueue linear progress: create outbox entry: %w", err)
	}
	return nil
}
