package outbox

import "time"

// FailureClass is how one delivery attempt that did not deliver is recorded
// (technical plan §5.1, §44.2). EvaluateFailure decides it; the worker writes
// what it decided and nothing else.
type FailureClass int

const (
	// ClassCounted keeps the attempt the claim counted: the row is
	// rescheduled on EvaluateBackoff's curve, or dead-lettered once it has
	// made MaxAttempts. Every failure is of this class unless a rule of
	// ClassDeferred's claims it.
	ClassCounted FailureClass = iota

	// ClassDeferred is the failure class that does not consume an attempt:
	// the attempt the claim counted is given back and the row is due again
	// at NextRetryAt, no closer to dead-letter than before the claim. It
	// is for a failure that says nothing about the delivery itself -- the
	// row did nothing wrong, and the time is all that was wrong.
	//
	// It is deliberately not tied to one cause. Today its cause is this
	// process's own shutdown (§5.1): a delivery the shutdown cut short, or
	// a claimed row the shutdown reached before its delivery started. A
	// rate limit whose deadline GitHub stated is the next (§44.2): it adds
	// its own input to Failure, its own rules below, and a NextRetryAt at
	// that deadline, and records through the same write
	// (postgres.OutboxStore.Defer), which takes the attempt back and sets
	// the time. Each cause brings its own bound, past which its failure is
	// ClassCounted again, so no cause can keep a row alive forever: a
	// shutdown's is Policy.MaxConsecutiveInterruptions, a count kept on the
	// row; a rate limit's is to be a ceiling of its own, measured from the
	// first deferral the row records.
	ClassDeferred
)

// Rule names the rule EvaluateFailure applied to one failed attempt -- the
// fact the worker logs about its decision, so a reader can tell a deferral
// from a failure, and why an interruption counted.
type Rule string

const (
	// RuleFailed applies when the attempt completed and failed on its own
	// merits, with no shutdown involved. ClassCounted; the run of
	// interruptions resets.
	RuleFailed Rule = "failed"

	// RuleShutdownBeforeStart applies when this process's shutdown had
	// begun before the row's delivery started, so nothing was sent.
	// ClassDeferred for every kind, repeatable or not, due at once, and the
	// run of interruptions stays as it was: this was no delivery cut short.
	RuleShutdownBeforeStart Rule = "shutdown_before_start"

	// RuleShutdownInterrupted applies when this process's shutdown cut the
	// delivery short, and none of the three rules below applies.
	// ClassDeferred, due again after Policy.InterruptedSettleDelay.
	RuleShutdownInterrupted Rule = "shutdown_interrupted"

	// RuleShutdownNotRepeatable applies when the shutdown cut the delivery
	// short, but the row's kind is not safe to deliver twice: a delivery
	// cut after the remote end accepted it would be repeated, and deferring
	// it would repeat it more often, not less. ClassCounted, as before this
	// rule.
	RuleShutdownNotRepeatable Rule = "shutdown_interrupted_not_repeatable"

	// RuleShutdownOutlivedDeliveryTimeout applies when the delivery ran for
	// at least its own delivery timeout, whatever stopped it. ClassCounted,
	// so a delivery that hangs until a liveness restart still reaches
	// dead-letter.
	RuleShutdownOutlivedDeliveryTimeout Rule = "shutdown_interrupted_outlived_delivery_timeout"

	// RuleShutdownPastBound applies when the shutdown cut the delivery short
	// more times in a row than Policy.MaxConsecutiveInterruptions allows.
	// ClassCounted.
	RuleShutdownPastBound Rule = "shutdown_interrupted_past_bound"
)

// Failure is one delivery attempt that did not deliver, as the worker
// observed it. Every field is a value the caller measured or looked up; this
// package reads no clock, no process state and no error.
type Failure struct {
	// AttemptCount is the row's attempt count after the claim that started
	// this attempt, this attempt included -- EvaluateBackoff's convention.
	AttemptCount int

	// ConsecutiveInterruptions is the row's run of shutdown interruptions
	// before this attempt (outbox.consecutive_interruptions). Below zero is
	// read as zero.
	ConsecutiveInterruptions int

	// ShutdownBegun reports that this process's own shutdown had begun when
	// the attempt ended. The caller reads it from the process's shutdown
	// state (platform.ShutdownState), set when the drain begins -- never
	// from a cancellation error, which the delivery's context, wrapping the
	// worker's, cannot attribute.
	ShutdownBegun bool

	// NotStarted reports that no delivery call was made: the row was
	// claimed, and the shutdown reached it before its turn. Read only when
	// ShutdownBegun is set.
	NotStarted bool

	// OutlivedDeliveryTimeout reports that the delivery ran for at least its
	// own delivery timeout (platform.Timeouts.OutboxDeliveryTimeout).
	OutlivedDeliveryTimeout bool

	// Repeatable reports that delivering the row's kind again after the
	// remote end accepted it leaves the same result as delivering it once,
	// so a deferral cannot multiply its effect. The zero value, false, is
	// the safe one: an interruption of a kind nobody classified counts.
	Repeatable bool
}

// Policy is EvaluateFailure's configuration, populated by the caller from
// platform.Timeouts.
type Policy struct {
	// Backoff is the counted class's schedule (EvaluateBackoff).
	Backoff BackoffConfig

	// MaxConsecutiveInterruptions is how many shutdown interruptions in a
	// row keep their attempt; the next one counts.
	// platform.Timeouts.OutboxMaxConsecutiveInterruptions.
	MaxConsecutiveInterruptions int

	// InterruptedSettleDelay is how long a delivery the shutdown cut short
	// waits before it is due again, when it keeps its attempt: the cut
	// request may already have reached the remote end, and a repeat must
	// not run before it lands. platform.Timeouts.
	// OutboxInterruptedSettleDelay.
	InterruptedSettleDelay time.Duration
}

// FailureDecision is EvaluateFailure's verdict for one failed attempt.
type FailureDecision struct {
	// Class is ClassCounted or ClassDeferred.
	Class FailureClass

	// Rule is the rule that decided Class.
	Rule Rule

	// BackoffDecision carries when the row is due again (NextRetryAt) and,
	// for ClassCounted only, whether this attempt dead-letters it
	// (DeadLetter): EvaluateBackoff's own verdict for a counted attempt,
	// and for a deferred one a NextRetryAt the deferral's cause decided and
	// never a dead-letter, since no attempt was spent.
	BackoffDecision

	// ConsecutiveInterruptions is the row's run of shutdown interruptions
	// to record with this outcome: zero after an attempt that completed,
	// one more after a delivery the shutdown cut short -- counted or not --
	// and unchanged for a row whose delivery never started.
	ConsecutiveInterruptions int
}

// EvaluateFailure decides how one failed delivery attempt is recorded, and
// is the one place that decision is made: a counted failure's schedule is
// still EvaluateBackoff's, called from here.
//
// The rules, in order:
//
//   - No shutdown: RuleFailed, counted. An attempt that completes resets the
//     run of interruptions.
//   - Shutdown before the delivery started: RuleShutdownBeforeStart,
//     deferred, due at once -- nothing was sent -- run unchanged.
//   - Shutdown cut the delivery short: the run grows by one, and the
//     attempt is counted if the kind is not repeatable, if the delivery
//     outlived its own delivery timeout, or if the run now exceeds
//     MaxConsecutiveInterruptions, in that order of precedence; otherwise
//     RuleShutdownInterrupted, deferred and due InterruptedSettleDelay
//     later, so the cut request, if the remote end accepted it, has
//     landed before another replica, or this one after its restart,
//     repeats it.
func EvaluateFailure(f Failure, p Policy, now time.Time) FailureDecision {
	run := f.ConsecutiveInterruptions
	if run < 0 {
		run = 0
	}

	counted := func(rule Rule, run int) FailureDecision {
		return FailureDecision{
			Class:                    ClassCounted,
			Rule:                     rule,
			BackoffDecision:          EvaluateBackoff(f.AttemptCount, p.Backoff, now),
			ConsecutiveInterruptions: run,
		}
	}
	deferred := func(rule Rule, run int, dueAfter time.Duration) FailureDecision {
		return FailureDecision{
			Class:                    ClassDeferred,
			Rule:                     rule,
			BackoffDecision:          BackoffDecision{NextRetryAt: now.Add(dueAfter)},
			ConsecutiveInterruptions: run,
		}
	}

	if !f.ShutdownBegun {
		return counted(RuleFailed, 0)
	}
	if f.NotStarted {
		return deferred(RuleShutdownBeforeStart, run, 0)
	}

	run++
	switch {
	case !f.Repeatable:
		return counted(RuleShutdownNotRepeatable, run)
	case f.OutlivedDeliveryTimeout:
		return counted(RuleShutdownOutlivedDeliveryTimeout, run)
	case run > p.MaxConsecutiveInterruptions:
		return counted(RuleShutdownPastBound, run)
	default:
		return deferred(RuleShutdownInterrupted, run, p.InterruptedSettleDelay)
	}
}
