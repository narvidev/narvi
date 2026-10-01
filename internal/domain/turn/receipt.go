package turn

import "time"

// Prompt receipts (technical plan §3.3). A turn is committed Processing
// before its prompt is written to the sandbox's socket, and the write is a
// single frame with no acknowledgement. A control plane that dies between
// the commit and the write, or a frame lost with its socket after a write
// that returned, leaves the turn Processing on a sandbox that never
// received it. Both losses end the socket, so the sandbox's next ready on
// the same gen -- its reconnect -- is when the prompt can be lost no
// further, and when the session actor asks whether the agent answered it.
//
// The receipt is data, never a transition: Dispatched -> Processing still
// commits with the dispatch, and nothing here adds an edge to the turn's
// transition table. A replica that does not know receipts completes only a
// Processing turn, so a turn held in Dispatched until its receipt came
// back would never complete there.
//
// These two functions are the decision; the session actor reads the facts
// and acts on the verdict (internal/app/sessionactor/promptreceipt.go).

// PromptResendFacts are what the session actor reads, for the one turn in
// flight on a live Ready or Suspect sandbox whose gen is the turn's own
// dispatched gen, before it considers re-sending that turn's prompt.
type PromptResendFacts struct {
	// Processing is true when the turn's status is processing.
	Processing bool
	// ReceiptRequested is true when the turn's current dispatch asked for
	// a receipt: receipt_requested_message_id is set and equals
	// dispatched_message_id. A dispatch by a binary that does not know
	// receipts changes dispatched_message_id and leaves the request alone,
	// so the two differ and the dispatch reads as not asked.
	ReceiptRequested bool
	// StopRequested is true when a person's stop flagged the turn
	// (stop_requested_at set): it is never re-sent.
	StopRequested bool
	// GenCapable is true when the live gen's latest ready advertised the
	// prompt-receipt capability (sandboxes.prompt_receipt_gen = gen).
	GenCapable bool
	// RetirementOwed is true when a person's stop has still to retire the
	// live gen (sandboxes.stop_retire_gen set): it takes nothing more.
	RetirementOwed bool
	// ReconnectedSinceCheck is true when the sandbox has recorded a ready
	// since the last one this turn's check answered
	// (sandboxes.ready_seq > turns.receipt_checked_ready_seq).
	ReconnectedSinceCheck bool
}

// PromptReconnectToAnswer reports whether f describes a same-gen reconnect
// the session actor must answer for the turn: look its receipt up and,
// within PromptResendWindow and with none stored, re-send its prompt. All
// of the facts must hold; with any one missing the answer is false, and
// the actor reads nothing more.
func PromptReconnectToAnswer(f PromptResendFacts) bool {
	return f.Processing &&
		f.ReceiptRequested &&
		!f.StopRequested &&
		f.GenCapable &&
		!f.RetirementOwed &&
		f.ReconnectedSinceCheck
}

// PromptResendOutcome is DecidePromptResend's verdict.
type PromptResendOutcome int

const (
	// PromptResendReceiptStored means the agent's receipt is stored, so it
	// holds the prompt, and nothing is sent.
	PromptResendReceiptStored PromptResendOutcome = iota + 1
	// PromptResendWindowExpired means no receipt is stored, but the dispatch
	// asked for one at least the window ago. Nothing is sent; the turn
	// ends as it would have without receipts, at turn_deadline or by a
	// person's stop.
	PromptResendWindowExpired
	// PromptResendSend means no receipt is stored, inside the window, under
	// the cap. The prompt is sent again, with the same messageId, which the
	// agent runs at most once.
	PromptResendSend
	// PromptResendCapReached means no receipt is stored, inside the window,
	// but the dispatch's prompt has already been re-sent the most times
	// allowed: a loss that recurs on every reconnect -- a frame the sandbox
	// can never take -- is not fed by more re-sends. Nothing is sent; the
	// turn ends as one whose window ran out.
	PromptResendCapReached
)

// String names the outcome as the session actor logs and counts it.
func (o PromptResendOutcome) String() string {
	switch o {
	case PromptResendReceiptStored:
		return "receipt_stored"
	case PromptResendWindowExpired:
		return "window_expired"
	case PromptResendSend:
		return "send"
	case PromptResendCapReached:
		return "cap_reached"
	default:
		return "unknown"
	}
}

// DecidePromptResend decides what to do about a reconnect
// PromptReconnectToAnswer said to answer: receiptStored is whether the
// turn's receipt is stored, sinceRequest how long ago its dispatch asked
// for one, window PromptResendWindow, resends how many times the
// dispatch's prompt has been re-sent already and maxResends
// PromptResendMaxPerTurn. A stored receipt wins; without one, an elapsed
// time at or past the window refuses, then resends at or past the cap
// refuse, and anything else -- a negative elapsed time included -- sends.
func DecidePromptResend(receiptStored bool, sinceRequest, window time.Duration, resends, maxResends int) PromptResendOutcome {
	switch {
	case receiptStored:
		return PromptResendReceiptStored
	case sinceRequest >= window:
		return PromptResendWindowExpired
	case resends >= maxResends:
		return PromptResendCapReached
	default:
		return PromptResendSend
	}
}
