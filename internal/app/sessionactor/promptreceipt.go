// This file (promptreceipt.go) is technical plan §3.3's prompt receipts: a
// prompt the sandbox never received is sent again, once per same-gen
// reconnect, to an agent that says it can tell.
//
// tryPlanDispatch commits a turn Pending -> Dispatched -> Processing and
// arms turn_deadline, and only then, outside the transaction,
// executeDispatch writes the prompt to the sandbox's socket: one frame, with
// no acknowledgement. A control plane that dies between that commit and the
// write, or a frame lost with its socket after a write that returned, leaves
// a turn Processing on a sandbox that never received it. Both losses end the
// socket. The agent then reconnects on the same gen and sends `ready` first,
// and before this file nothing on that gen noticed: the ready moves no Ready
// sandbox, heartbeats carry no busy state, planReenqueueOrRespawn re-sends
// only across a changed gen, and inactivity defers while a turn is
// Processing. The turn waited out turn_deadline.
//
// # The capability, per gen
//
// Every ready a capable agent sends advertises capabilities.promptReceipt.
// The control plane never infers it from the agent's build: every agent
// reports agentVersion "dev", and agents older than the capability keep
// running from snapshots and repo images. handleSandboxEvent records each
// ready of the live gen (RecordSandboxReady): it bumps sandboxes.ready_seq,
// and sets sandboxes.prompt_receipt_gen to the gen when the ready advertised
// the capability and to NULL when it did not -- the latest ready decides.
// Stored against the gen, like boot_evidence_gen (bootevidence.go): a
// respawn, restore or resume bumps the gen, and the value stops matching.
//
// # The request, in the dispatch's commit
//
// A dispatch to a capable gen sets receiptRequested on the prompt, and
// records in the same commit which messageId asked
// (turns.receipt_requested_message_id), when, on the database's clock
// (receipt_requested_at), and the gen's ready_seq at that moment
// (receipt_checked_ready_seq). A dispatch to an incapable gen records
// nothing and clears an earlier request, and its prompt is byte-identical to
// one a control plane without receipts sends. A dispatch by such a control
// plane changes dispatched_message_id and leaves the request alone, so the
// two no longer match: that dispatch reads as not asked.
//
// # The receipt is data, never a transition
//
// The agent answers every prompt that asked with a prompt_received event --
// a duplicate too -- whose messageId is 'prompt_received:{prompt
// messageId}', and runs a messageId at most once. The control plane writes
// nothing of its own for it: handleSandboxEvent stores it through the
// generic path, under that wire messageId, as a control plane that does not
// know the type also does. The stored row is the receipt, found by that key
// whichever binary stored it. Dispatched -> Processing still commits with
// the dispatch: a turn held in Dispatched until its receipt came back would
// never complete on a control plane that completes only a Processing turn
// (completeProcessingTurn), and nothing here adds an edge to the turn's
// transition table.
//
// # The re-send
//
// The trigger is the same-gen ready, the reconnect, made durable as
// ready_seq: a timer would re-send on the connection that already carried
// the frame. Every committed sandbox event runs a dispatch evaluation, so
// the ready's own post-commit evaluation answers it at once; if that
// evaluation fails, its transaction rolls back and the next heartbeat's
// answers it. planReenqueueOrRespawn's same-gen branch calls
// tryPlanReceiptResend, which reads nothing more unless
// turn.PromptReconnectToAnswer holds -- the turn is Processing, its own
// dispatch asked, no stop flagged it, the live gen is capable and owes no
// stop its retirement, and a ready has been recorded since the last one
// answered. It then reads, in one statement on the database's clock,
// whether the receipt is stored and how long ago the dispatch asked, claims
// the ready with a compare-and-set on receipt_checked_ready_seq, and
// turn.DecidePromptResend says what follows: a stored receipt sends
// nothing; past PromptResendWindow nothing is sent either, and one WARN says
// so; past PromptResendMaxPerTurn re-sends of the dispatch nothing is sent
// either, with one WARN; otherwise executeReceiptResend writes the prompt
// again. So a prompt is re-sent at most once per reconnect, at most
// PromptResendMaxPerTurn times in all, and never once its receipt is stored
// or its turn has left Processing. The cap is the backstop for a loss that
// is not a one-off: each re-send is answered only after the next
// reconnect, and a frame the sandbox can never take -- one over its read
// limit, before platform.MaxPromptFrameBytes existed -- causes the very
// reconnect that would re-send it.
//
// The re-send is the original prompt: the same messageId, which the agent
// dedups on and httpapi.PostReviewVerdict resolves the turn by. It re-arms
// nothing -- turn_deadline stays a firm bound from the original dispatch --
// and moves none of dispatched_at, dispatched_event_id or
// dispatched_sandbox_gen: dispatched_event_id bounds the stored token frames
// and §26.4's corroboration reads, and the prompt may in fact be running. A
// re-send that fails, or that the rollout re-check refuses, is logged and
// counted and fails nothing: the original may be running, and the next
// reconnect asks again. At the window's bound the turn ends as it always
// did, at turn_deadline or by a person's stop.

package sessionactor

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/narvidev/narvi/contracts/gen/go/sandboxws"
	"github.com/narvidev/narvi/internal/adapters/outbound/postgres/sqlcgen"
	"github.com/narvidev/narvi/internal/domain/turn"
	"github.com/narvidev/narvi/internal/platform"
)

// The outcome attribute of turn_prompt_resend_total (opsmetrics.go).
const (
	promptResendOutcomeSent          = "sent"
	promptResendOutcomeSendFailed    = "send_failed"
	promptResendOutcomeRefused       = "refused"
	promptResendOutcomeWindowExpired = "window_expired"
	promptResendOutcomeCapReached    = "cap_reached"
	promptResendOutcomeFrameTooLarge = "frame_too_large"
)

// receiptResendPlan is what tryPlanReceiptResend hands executeDispatch, on
// a dispatchPlan, once its transaction has claimed a same-gen reconnect:
// what the check found and decided, acted on (logged, counted, sent) only
// after that transaction has committed.
type receiptResendPlan struct {
	// outcome is turn.DecidePromptResend's verdict.
	outcome turn.PromptResendOutcome
	// gen is the live gen, the turn's dispatched gen.
	gen int32
	// messageID is the turn's dispatched_message_id, the prompt's own.
	messageID string
	// checkedReadySeq and readySeq are the claim: the ready the turn's check
	// had answered last, and the one this check answered.
	checkedReadySeq int32
	readySeq        int32
	// sinceRequest is how long before the check the dispatch asked for a
	// receipt, on the database's clock.
	sinceRequest time.Duration
	// resends is how many times the dispatch's prompt had been re-sent
	// before this check.
	resends int32
}

// readyAdvertisesPromptReceipt reports whether a ready event advertises
// capabilities.promptReceipt. A ready that fails its schema decode counts
// as not advertising it -- the safe direction, in which nothing is ever
// re-sent -- as a boot_timing that fails its decode is no boot evidence
// (bootevidence.go).
func readyAdvertisesPromptReceipt(raw json.RawMessage) bool {
	var evt sandboxws.Ready
	if err := json.Unmarshal(raw, &evt); err != nil {
		return false
	}
	return evt.Capabilities != nil && evt.Capabilities.PromptReceipt != nil && *evt.Capabilities.PromptReceipt
}

// promptReceiptCapable reports whether row's live gen advertised the
// prompt-receipt capability in its latest ready.
func promptReceiptCapable(row sqlcgen.Sandbox) bool {
	return row.PromptReceiptGen != nil && *row.PromptReceiptGen == row.Gen
}

// recordPromptReceiptRequest records, in tx -- the dispatch's own
// transaction, right after the write that stamped messageID as turnID's
// dispatched_message_id -- whether this dispatch asks sandboxRow's gen for
// a receipt, and returns that answer, which is the one the prompt's
// receiptRequested must carry. It asks when, and only when, the gen is
// capable; otherwise it records NULL, clearing a request an earlier
// dispatch of the turn made.
func (a *Actor) recordPromptReceiptRequest(ctx context.Context, tx pgx.Tx, turnID pgtype.UUID, sandboxRow sqlcgen.Sandbox, messageID string) (bool, error) {
	requested := promptReceiptCapable(sandboxRow)
	var asked *string
	if requested {
		asked = &messageID
	}
	if err := a.stores.turn.WithTx(tx).SetPromptReceiptRequest(ctx, turnID, asked, sandboxRow.ReadySeq); err != nil {
		return false, fmt.Errorf("sessionactor: record the prompt receipt request: %w", err)
	}
	return requested, nil
}

// promptResendFacts reads, from the rows planDispatch already loaded, the
// facts turn.PromptReconnectToAnswer decides on, for target, the turn in
// flight on sandboxRow's live gen.
func promptResendFacts(sandboxRow sqlcgen.Sandbox, target sqlcgen.Turn) turn.PromptResendFacts {
	return turn.PromptResendFacts{
		Processing: turn.State(target.Status) == turn.StateProcessing,
		ReceiptRequested: target.ReceiptRequestedMessageID != nil && target.DispatchedMessageID != nil &&
			*target.ReceiptRequestedMessageID == *target.DispatchedMessageID,
		StopRequested:         target.StopRequestedAt.Valid,
		GenCapable:            promptReceiptCapable(sandboxRow),
		RetirementOwed:        retirementOwed(sandboxRow),
		ReconnectedSinceCheck: target.ReceiptCheckedReadySeq != nil && sandboxRow.ReadySeq > *target.ReceiptCheckedReadySeq,
	}
}

// tryPlanReceiptResend answers, inside planDispatch's transaction, a
// same-gen reconnect of the sandbox target is in flight on -- this file's
// top comment, "The re-send". It returns a plan for executeDispatch once it
// has claimed the reconnect, and nil when there is none to answer, which
// costs no query: the facts come from rows planDispatch already read.
//
// The lookup and the claim run in the evaluation's transaction, which holds
// the session row FOR UPDATE under the actor's epoch (transact), and every
// events insert takes that row's lock first (CreateEvent), so no receipt
// commits between the two. One that lands after this commit and before the
// frame is written costs one duplicate prompt, which the agent answers
// without running. Another replica's evaluation cannot claim the same
// ready: only the actor holding the session's advisory lock evaluates, a
// stale one's transact fails on its epoch before it reads anything, and
// the compare-and-set lets exactly one evaluation move the mark.
func (a *Actor) tryPlanReceiptResend(
	ctx context.Context, tx pgx.Tx,
	sessionRow sqlcgen.Session, sandboxRow sqlcgen.Sandbox, target sqlcgen.Turn,
) (*dispatchPlan, error) {
	if !turn.PromptReconnectToAnswer(promptResendFacts(sandboxRow, target)) {
		return nil, nil
	}
	if a.commander == nil {
		// Defensive, as tryPlanReenqueue's own guard: nothing could be sent.
		a.logger.Warn("sessionactor: prompt receipt check would proceed but no SandboxCommander is configured; skipping")
		return nil, nil
	}

	turns := a.stores.turn.WithTx(tx)
	stored, since, ok, err := turns.PromptReceiptState(ctx, target.ID)
	if err != nil {
		return nil, fmt.Errorf("sessionactor: read the prompt receipt state: %w", err)
	}
	if !ok {
		// Unreachable while the facts above hold: the request and its
		// instant are written together (SetTurnPromptReceiptRequest).
		return nil, nil
	}
	outcome := turn.DecidePromptResend(stored, since, a.timeouts.PromptResendWindow,
		int(target.ReceiptResendCount), a.timeouts.PromptResendMaxPerTurn)

	// The claim counts the re-send it decides on in the same statement,
	// and holds only while the count is the one decided on: the cap is
	// enforced on the row state this evaluation read.
	messageID := *target.DispatchedMessageID
	checked := *target.ReceiptCheckedReadySeq
	moved, err := turns.MarkPromptReconnectAnswered(ctx, target.ID, messageID, checked, sandboxRow.ReadySeq,
		target.ReceiptResendCount, outcome == turn.PromptResendSend)
	if err != nil {
		return nil, fmt.Errorf("sessionactor: claim the reconnect for the prompt receipt check: %w", err)
	}
	if moved == 0 {
		a.logger.Info("sessionactor: prompt receipt check: the reconnect was already answered, or the turn moved on; nothing sent",
			"turn_id", target.ID.String(), "gen", sandboxRow.Gen, "message_id", messageID,
			"checked_ready_seq", checked, "ready_seq", sandboxRow.ReadySeq)
		return nil, nil
	}

	// The re-send is the original prompt: the turn's own messageId, never a
	// new one (tryPlanReenqueue's fresh one is for a different gen), and
	// receiptRequested set. Nothing else of the dispatch moves -- no
	// turn_deadline re-arm, no dispatched_at, dispatched_event_id or
	// dispatched_sandbox_gen -- this file's top comment says why.
	var payload json.RawMessage
	if outcome == turn.PromptResendSend {
		if payload, err = BuildPromptPayload(a.sessionID.String(), sessionRow, sandboxRow, target, messageID, true); err != nil {
			return nil, fmt.Errorf("sessionactor: build prompt payload (receipt re-send): %w", err)
		}
	}

	return &dispatchPlan{
		turnID:     target.ID,
		payload:    payload,
		sessionRow: sessionRow,
		receiptResend: &receiptResendPlan{
			outcome:         outcome,
			gen:             sandboxRow.Gen,
			messageID:       messageID,
			checkedReadySeq: checked,
			readySeq:        sandboxRow.ReadySeq,
			sinceRequest:    since,
			resends:         target.ReceiptResendCount,
		},
	}, nil
}

// executeReceiptResend acts, outside any transaction, on what
// tryPlanReceiptResend decided and committed: nothing for a stored
// receipt, one WARN and a count past the window or the cap
// (PromptResendMaxPerTurn), and otherwise the original prompt written
// again. A re-send passes the same turn-dispatch-time checks every
// dispatch does (executeDispatch) -- the rollout re-check and the frame's
// size against platform.MaxPromptFrameBytes -- but neither a refusal nor a
// failed write fails the turn, as executeDispatch's would: the prompt may
// already be running, and the next reconnect asks again. The size check
// cannot refuse a frame the same dispatch already sent; it is there so no
// frame over the limit is ever written, whatever produced it.
//
// The payload tryPlanReceiptResend built carries the turn's own
// dispatched_message_id -- never a new one -- and receiptRequested: the
// agent dedups on that id, and httpapi.PostReviewVerdict resolves the turn
// by it.
func (a *Actor) executeReceiptResend(ctx context.Context, plan *dispatchPlan) error {
	rr := plan.receiptResend
	switch rr.outcome {
	case turn.PromptResendReceiptStored:
		a.logger.Info("sessionactor: prompt receipt check after a same-gen reconnect: receipt stored; nothing sent",
			"turn_id", plan.turnID.String(), "gen", rr.gen, "message_id", rr.messageID, "ready_seq", rr.readySeq)
		return nil
	case turn.PromptResendWindowExpired:
		a.logger.Warn("sessionactor: prompt not receipted by its sandbox, but its dispatch asked longer ago than the re-send window; not re-sent, the turn ends at its deadline",
			"turn_id", plan.turnID.String(), "gen", rr.gen, "message_id", rr.messageID,
			"since_request", rr.sinceRequest.String(), "window", a.timeouts.PromptResendWindow.String())
		a.recordPromptResend(ctx, promptResendOutcomeWindowExpired)
		return nil
	case turn.PromptResendCapReached:
		a.logger.Warn("sessionactor: prompt not receipted by its sandbox, but it has been re-sent the most times a turn allows; not re-sent, the turn ends at its deadline",
			"turn_id", plan.turnID.String(), "gen", rr.gen, "message_id", rr.messageID,
			"resends", rr.resends, "max", a.timeouts.PromptResendMaxPerTurn)
		a.recordPromptResend(ctx, promptResendOutcomeCapReached)
		return nil
	case turn.PromptResendSend:
	default:
		return fmt.Errorf("sessionactor: unknown prompt resend outcome %v", rr.outcome)
	}

	if repo, refused, transient := a.rolloutRefusalForDispatch(ctx, plan.sessionRow); refused {
		a.logger.Warn("sessionactor: prompt re-send refused: configured repo is not enrolled in the cohort rollout; the turn stays processing",
			"turn_id", plan.turnID.String(), "gen", rr.gen, "message_id", rr.messageID, "repo", repo, "transient", transient)
		a.recordPromptResend(ctx, promptResendOutcomeRefused)
		return nil
	}
	if len(plan.payload) > platform.MaxPromptFrameBytes {
		a.logger.Warn("sessionactor: prompt re-send refused: the frame is larger than a sandbox accepts; the turn stays processing",
			"turn_id", plan.turnID.String(), "gen", rr.gen, "message_id", rr.messageID,
			"frame_bytes", len(plan.payload), "max_frame_bytes", platform.MaxPromptFrameBytes)
		a.recordPromptResend(ctx, promptResendOutcomeFrameTooLarge)
		return nil
	}

	a.logger.Info("sessionactor: re-sending a prompt its sandbox has not receipted, after a same-gen reconnect",
		"turn_id", plan.turnID.String(), "gen", rr.gen, "message_id", rr.messageID,
		"checked_ready_seq", rr.checkedReadySeq, "ready_seq", rr.readySeq,
		"since_request", rr.sinceRequest.String(), "window", a.timeouts.PromptResendWindow.String(),
		"resend", rr.resends+1, "max", a.timeouts.PromptResendMaxPerTurn)
	if err := a.commander.SendCommand(a.sessionID.String(), plan.payload); err != nil {
		a.logger.Warn("sessionactor: prompt re-send failed; the turn stays processing and the next reconnect asks again",
			"turn_id", plan.turnID.String(), "gen", rr.gen, "message_id", rr.messageID, "error", err)
		a.recordPromptResend(ctx, promptResendOutcomeSendFailed)
		return nil
	}
	a.recordPromptResend(ctx, promptResendOutcomeSent)
	return nil
}
