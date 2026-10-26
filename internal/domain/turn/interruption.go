package turn

import "time"

// Interruptions (technical plan §35.4). A turn whose sandbox stops under
// it -- the sandbox reached the end of its lifetime, or its provider
// stopped it -- has not failed, and nothing downstream may be told that it
// has: the turn stays Processing and is sent again, as the same turn, to
// the sandbox that replaces the stopped one, carrying a short context that
// tells the agent to check what the earlier attempt left. The
// interruption is data on the turn, never a transition: nothing here adds
// an edge to the turn's transition table.
//
// Three sources lead to that one handling:
//
//   - The typed key, execution_complete's interruption (contracts
//     1.30.0). An agent that reports it sets it when its process was told
//     to stop while the turn ran. It reports only what it saw; whether
//     the stop was the sandbox's lifetime deadline is the control plane's
//     call, from the deadline it persisted (AttributeInterruption), never
//     from text.
//   - An older agent's cancellation text, compared whole against
//     LegacyInterruptionReason, never parsed and never matched by pattern.
//     This reader is permanent, never a migration aid to remove later: a
//     snapshot carries the agent it was minted with, and a session's
//     lineage restores that older agent for as long as the lineage lives.
//   - Silence: a sandbox declared dead while the turn ran on it, past its
//     recorded deadline (DecideSilentInterruption).
//
// No classifier here reads a reason's digits or a diagnostic. A deadline
// message that embeds its own elapsed seconds reads as the turn's own end,
// never as an interruption and never as a transient error to retry: the
// agent runtime's retry reads only the runtime's typed isRetryable, and a
// re-send only ever goes to a newer gen. The texts this file writes carry
// no duration.
//
// Every function here is pure per §11.

// LegacyInterruptionReason is the reason the runtime adapter writes on a
// turn's end when the agent's process context was cancelled -- the
// process was told to stop -- before the turn finished: its
// finalizeCanceled (internal/adapters/outbound/opencode), whose own test
// pins the two strings together. It is written on no other path, so it
// means exactly that, and it is all an agent that predates the typed key
// says of a stop.
const LegacyInterruptionReason = "opencode: turn context canceled before completion"

// InterruptionSource is where an interruption was recognised.
type InterruptionSource string

const (
	// InterruptionSourceTyped is execution_complete's interruption key.
	InterruptionSourceTyped InterruptionSource = "typed"
	// InterruptionSourceLegacyText is an older agent's cancellation text,
	// LegacyInterruptionReason, compared whole.
	InterruptionSourceLegacyText InterruptionSource = "legacy_text"
	// InterruptionSourceSilent is a sandbox declared dead under the turn
	// past its recorded deadline, with no end reported.
	InterruptionSourceSilent InterruptionSource = "silent"
)

// InterruptionReason is why the control plane holds a turn was
// interrupted, attributed from the stopped gen's persisted deadline
// (AttributeInterruption). Stored on the turn (turns.interruption_reason)
// and read back to pick the texts below.
type InterruptionReason string

const (
	// InterruptionReasonSandboxLifetime is a stop at or after the gen's
	// recorded lifetime deadline.
	InterruptionReasonSandboxLifetime InterruptionReason = "sandbox_lifetime"
	// InterruptionReasonSandboxStopped is any other stop: the deadline
	// is not known, or was still ahead.
	InterruptionReasonSandboxStopped InterruptionReason = "sandbox_stopped"
)

// AgentEnd is what ReadAgentEnd reads of an agent's execution_complete,
// and of the turn it ends.
type AgentEnd struct {
	// Outcome is the frame's outcome: completed, failed or cancelled, the
	// turn's own terminal states' names.
	Outcome string
	// Reason is the frame's reason; nil when it has none.
	Reason *string
	// Interruption is the frame's interruption key as the control plane
	// reads it, leniently: nil when the key is absent, or is not a JSON
	// string. An empty string counts as absent.
	Interruption *string
	// SubTask is true when the frame ends a sub-task's lane (a non-null
	// subTaskId), not the turn.
	SubTask bool
	// StopRequested is true when a person's stop flagged the turn the
	// frame ends (turns.stop_requested_at set).
	StopRequested bool
}

// EndReading is ReadAgentEnd's answer.
type EndReading struct {
	// Interrupted is true when the frame reports that the sandbox's stop
	// interrupted the turn; false when it is the turn's own end.
	Interrupted bool
	// Source is where the interruption was recognised; empty when the end
	// is the turn's own.
	Source InterruptionSource
	// Kind is the typed key's value, for a log line; empty for any other
	// source and for the turn's own end. Any value counts, a value this
	// release does not know included.
	Kind string
	// KeyOnCompletion is true when a completed end carried the key: the
	// end is the turn's completion all the same, and the caller logs the
	// key at WARN.
	KeyOnCompletion bool
}

// ReadAgentEnd decides whether end reports the turn's own end or an
// interruption by its sandbox's stop. The first row that matches decides:
//
//  1. A sub-task's end is the turn's own (it ends no turn).
//  2. A completed end is a completion, whatever key it carries.
//  3. A turn a person's stop flagged gets its own end: the stop asked for
//     it.
//  4. A non-empty interruption key is an interruption, from the typed
//     source, whatever the reason says.
//  5. With no key, a cancelled end whose reason is exactly
//     LegacyInterruptionReason is an interruption, from the legacy text.
//  6. Anything else is the turn's own end.
//
// So the key always decides when it is present, and the text is read only
// when it is absent, compared whole: a reason that adds, drops or changes
// one character -- elapsed seconds appended, say -- is the turn's own end.
func ReadAgentEnd(end AgentEnd) EndReading {
	key := ""
	if end.Interruption != nil {
		key = *end.Interruption
	}
	switch {
	case end.SubTask:
		return EndReading{}
	case end.Outcome == string(StateCompleted):
		return EndReading{KeyOnCompletion: key != ""}
	case end.StopRequested:
		return EndReading{}
	case key != "":
		return EndReading{Interrupted: true, Source: InterruptionSourceTyped, Kind: key}
	case end.Outcome == string(StateCancelled) && end.Reason != nil && *end.Reason == LegacyInterruptionReason:
		return EndReading{Interrupted: true, Source: InterruptionSourceLegacyText}
	default:
		return EndReading{}
	}
}

// AttributeInterruption names why the control plane holds a turn was
// interrupted, from the stopped gen's persisted deadline, never from text:
// sandbox_lifetime when the deadline is known and its runway -- the time
// left until it, on the database's clock -- is zero or less, and
// sandbox_stopped otherwise. The deadline is the claim's conservative
// estimate, which a provider that keeps its stated limit never undercuts,
// so a stop at the provider's real deadline always finds it passed.
func AttributeInterruption(deadlineKnown bool, runway time.Duration) InterruptionReason {
	if deadlineKnown && runway <= 0 {
		return InterruptionReasonSandboxLifetime
	}
	return InterruptionReasonSandboxStopped
}

// The labels DecideSilentInterruption gives a lost gen's deadline.
const (
	SilentLossDeadlinePassed  = "passed"
	SilentLossDeadlineAhead   = "ahead"
	SilentLossDeadlineUnknown = "unknown"
)

// SilentLossFacts are what the session actor reads, in the transaction
// that declares a gen dead at its terminal grace, of the one turn in
// flight and of that gen.
type SilentLossFacts struct {
	// OnGen is true when the turn ran on the gen being declared dead (its
	// dispatched gen is that gen, or it has none recorded).
	OnGen bool
	// StopRequested is true when a person's stop flagged the turn.
	StopRequested bool
	// AlreadyInterruptedOnGen is true when an interruption on this very gen
	// is already recorded on the turn.
	AlreadyInterruptedOnGen bool
	// DeadlineKnown is true when the gen's lifetime deadline is recorded.
	DeadlineKnown bool
	// Runway is the time left until that deadline, on the database's
	// clock; zero or less once it has passed. Meaningless when the
	// deadline is not known.
	Runway time.Duration
}

// DecideSilentInterruption decides whether a gen lost in silence under its
// turn interrupted that turn: only when the turn ran on the gen, no stop
// flagged it, no interruption on that gen is recorded yet, and the gen's
// recorded deadline is known and has passed. A loss before the deadline
// stays ordinary turn recovery, with no warning and no context. deadline
// labels the gen's deadline in every case -- passed, ahead or unknown --
// so the loss can be counted either way.
func DecideSilentInterruption(f SilentLossFacts) (record bool, deadline string) {
	switch {
	case !f.DeadlineKnown:
		deadline = SilentLossDeadlineUnknown
	case f.Runway <= 0:
		deadline = SilentLossDeadlinePassed
	default:
		deadline = SilentLossDeadlineAhead
	}
	record = f.OnGen && !f.StopRequested && !f.AlreadyInterruptedOnGen && deadline == SilentLossDeadlinePassed
	return record, deadline
}

// The interrupted-turn context's fixed lines: the text a turn sent again
// after an interruption carries ahead of its prompt. Fixed text, true for
// a restored replacement and a fresh one alike, with no duration and no
// fact the control plane would have to read.
const (
	// InterruptionContextHeader opens the context.
	InterruptionContextHeader = "[Narvi: this request was interrupted]"
	// interruptionContextLifetime and interruptionContextStopped say what
	// happened, one per reason.
	interruptionContextLifetime = "An earlier attempt at the request below was under way in another sandbox when that sandbox reached the end of its lifetime, before the attempt finished. The request is sent to you again, as the same turn."
	interruptionContextStopped  = "An earlier attempt at the request below was under way in another sandbox when that sandbox was stopped by its provider, before the attempt finished. The request is sent to you again, as the same turn."
	// InterruptionContextCheck asks the agent to check what the earlier
	// attempt left before going on.
	InterruptionContextCheck = "Go on with the request rather than start it over, but first check what the earlier attempt left: run git status and git log, read the files involved, and compare the branch with its remote. Changes the earlier attempt made inside its sandbox may not be in this one."
	// InterruptionContextOutsideActions asks the agent not to repeat an
	// action outside the sandbox the earlier attempt may already have
	// taken.
	InterruptionContextOutsideActions = "Before you take any action outside this sandbox, such as recording a verdict or reporting a step outcome, check whether the earlier attempt already took it, and do not take it twice."
	// InterruptionContextClosing closes the context; the prompt follows it.
	InterruptionContextClosing = "[End of the note. The request follows.]"
	// InterruptionContextSeparator stands between the context and the
	// prompt it opens.
	InterruptionContextSeparator = "\n\n"
)

// RenderInterruptedTurnContext is the context a turn interrupted for
// reason r carries when it is sent again. A reason this release does not
// know reads as sandbox_stopped, the text that names no cause.
func RenderInterruptedTurnContext(r InterruptionReason) string {
	what := interruptionContextStopped
	if r == InterruptionReasonSandboxLifetime {
		what = interruptionContextLifetime
	}
	return InterruptionContextHeader + "\n" +
		what + "\n" +
		InterruptionContextCheck + "\n" +
		InterruptionContextOutsideActions + "\n" +
		InterruptionContextClosing
}

// PromptWithInterruptionContext is prompt, opened by the context for
// reason r: the context, InterruptionContextSeparator, then prompt
// byte for byte.
func PromptWithInterruptionContext(prompt string, r InterruptionReason) string {
	return RenderInterruptedTurnContext(r) + InterruptionContextSeparator + prompt
}

// The persisted warning's texts, one per reason.
const (
	interruptionWarningLifetime = "This turn was interrupted: the sandbox running it reached the end of its lifetime before the turn finished. The turn is still running: it is sent again, as the same turn, to the sandbox that replaces this one, and told to check what the earlier attempt left before going on. Changes that attempt made inside its sandbox may not be carried over."
	interruptionWarningStopped  = "This turn was interrupted: the sandbox running it was stopped by its provider before the turn finished. The turn is still running: it is sent again, as the same turn, to the sandbox that replaces this one, and told to check what the earlier attempt left before going on. Changes that attempt made inside its sandbox may not be carried over."
)

// InterruptionWarning is the neutral warning a person reads when a turn is
// interrupted for reason r: no failure word and no duration, since the
// turn has not failed. A reason this release does not know reads as
// sandbox_stopped.
func InterruptionWarning(r InterruptionReason) string {
	if r == InterruptionReasonSandboxLifetime {
		return interruptionWarningLifetime
	}
	return interruptionWarningStopped
}
