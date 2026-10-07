package turn

import "time"

// Review checkout (technical plan §21.1, §30.4). A turn of a pull request's
// review session that records a head (turns.review_head_sha) reads the tree
// of that commit, checked out in its sandbox before its prompt is sent: the
// session actor sends the sandbox a checkout command naming the pull
// request's ref and the head, and dispatches the turn only once the stored
// checkout_result says the worktree holds it. A warm sandbox holds whatever
// the previous turn left, and a sandbox boots at the ref's tip, so without
// this a re-review reads an older head's tree, or a newer one.
//
// The checkout is data on the pending turn, never a transition: the turn
// stays pending -- or, re-sent to a new gen, processing -- while it waits,
// and nothing here adds an edge to the turn's transition table. The
// session actor reads the facts, in the dispatch evaluation's transaction
// and on the database's clock, and acts on the verdict
// (internal/app/sessionactor/reviewcheckout.go).

// CheckoutOutcome is what a sandbox's checkout_result says it did for the
// turn's repo: the five values of contracts/sandbox-ws/v1/events.schema.json's
// CheckoutResult outcome, an open enum. DecideReviewCheckout reads any
// other value as CheckoutFailed.
type CheckoutOutcome string

const (
	// CheckoutCheckedOut means the worktree holds the reply's head.
	CheckoutCheckedOut CheckoutOutcome = "checked_out"
	// CheckoutSHAAbsent means the ref was fetched, and the commit asked for is
	// not in the repository -- a ref that lags a push, or a head that is
	// gone. The worktree is untouched.
	CheckoutSHAAbsent CheckoutOutcome = "sha_absent"
	// CheckoutFetchFailed means the ref could not be fetched.
	CheckoutFetchFailed CheckoutOutcome = "fetch_failed"
	// CheckoutBusy means a turn or the boot was running in the sandbox, so
	// nothing was done.
	CheckoutBusy CheckoutOutcome = "busy"
	// CheckoutFailed means anything else, named in the reply's error.
	CheckoutFailed CheckoutOutcome = "failed"
)

// CheckoutReply is the stored checkout_result's entry for the turn's repo.
type CheckoutReply struct {
	// Outcome is the entry's outcome, as the agent wrote it.
	Outcome CheckoutOutcome
	// HeadSHA is the worktree's HEAD after a checked_out outcome; "" when
	// the agent sent none.
	HeadSHA string
	// RefSHA is the ref's tip as fetched; "" when it was never read.
	RefSHA string
	// Error is why the outcome is not checked_out, as the agent wrote it.
	Error string
}

// CheckoutFacts are what the session actor reads, in the dispatch
// evaluation's transaction, for the one turn it is about to send, whose
// checkout the gate applies to.
type CheckoutFacts struct {
	// WantSHA is the head the turn recorded: the commit to check out.
	WantSHA string
	// AwaitingReady is true when the live gen has sent no ready yet: the
	// sandbox went suspect while it was still spawning or connecting -- a
	// restore or a respawn slow to connect, or one a provider failed. Its
	// capability is unknown, not absent, so nothing is decided about it
	// until it connects or is replaced.
	AwaitingReady bool
	// GenCapable is true when the live gen's latest ready advertised
	// capabilities.reviewCheckout (sandboxes.review_checkout_gen = gen).
	GenCapable bool
	// HasSnapshot is true when the sandbox has a snapshot
	// (sandboxes.snapshot_id), which the next gen would restore.
	HasSnapshot bool
	// RequestedOnGen is true when the turn's latest checkout request went
	// to the live gen (turns.checkout_gen = gen).
	RequestedOnGen bool
	// Reply is the reply to that request, nil when none is stored. A reply
	// that does not decode, or names no entry for the turn's repo, is a
	// reply whose outcome is CheckoutFailed.
	Reply *CheckoutReply
	// ReconnectedSinceSend is true when the sandbox has recorded a ready
	// since the latest command was sent (sandboxes.ready_seq >
	// turns.checkout_sent_ready_seq): the command may have been lost with
	// its socket.
	ReconnectedSinceSend bool
	// SinceRequest is how long ago the turn's first checkout on the live
	// gen was asked for; SinceSend how long ago the latest was sent. Both
	// on the database's clock.
	SinceRequest, SinceSend time.Duration
	// Sends is how many commands were sent on the live gen, of any kind.
	// Failures is how many of the replies before the latest said failed.
	Sends, Failures int
	// RetiredAGen is true when this turn's failed checkouts already
	// retired a gen (turns.checkout_retired_gen): it retires no other.
	RetiredAGen bool
	// MovedEndsTurn is true when a head that moved past WantSHA ends the
	// turn context_moved rather than running it on WantSHA: a pending
	// review attempt a lane asks for again (ContextCheckedAtDispatch).
	// False for every other turn, and for a turn already processing,
	// re-sent to a new gen, whose run had started on WantSHA.
	MovedEndsTurn bool
}

// CheckoutBounds are the platform.Timeouts the decision measures against.
type CheckoutBounds struct {
	// Timeout is ReviewCheckoutTimeout, from the first request on the gen.
	Timeout time.Duration
	// RefLagWindow is ReviewCheckoutRefLagWindow, from the first request.
	RefLagWindow time.Duration
	// RefetchInterval is ReviewCheckoutRefetchInterval.
	RefetchInterval time.Duration
	// FailuresBeforeRetire is ReviewCheckoutFailuresBeforeRetire.
	FailuresBeforeRetire int
}

// CheckoutAction is what the session actor does with the turn.
type CheckoutAction int

const (
	// CheckoutProceed means the sandbox holds WantSHA: the turn is
	// dispatched, and the commit recorded (turns.checked_out_sha).
	CheckoutProceed CheckoutAction = iota + 1
	// CheckoutSend means a checkout command is sent, with a new messageId,
	// and the turn looked at again NextLook from now. The turn stays as it
	// is.
	CheckoutSend
	// CheckoutWait means nothing is sent, and the turn looked at again
	// NextLook from now.
	CheckoutWait
	// CheckoutEndMoved means the pull request's head moved past WantSHA:
	// the pending attempt ends context_moved, as technical plan §24.9's
	// check ends one, and is asked for again. The verdict's RefSHA is the
	// ref's tip.
	CheckoutEndMoved
	// CheckoutRefuse means the turn ends, refused for the verdict's
	// Refusal.
	CheckoutRefuse
	// CheckoutRetireGen means the live gen is retired and the sandbox's
	// snapshot cleared, for the verdict's Retirement; the turn stays as it
	// is, and the next gen boots fresh.
	CheckoutRetireGen
)

// String names the action as the session actor logs it.
func (a CheckoutAction) String() string {
	switch a {
	case CheckoutProceed:
		return "proceed"
	case CheckoutSend:
		return "send"
	case CheckoutWait:
		return "wait"
	case CheckoutEndMoved:
		return "end_moved"
	case CheckoutRefuse:
		return "refuse"
	case CheckoutRetireGen:
		return "retire_gen"
	default:
		return "unknown"
	}
}

// CheckoutRefusal is why a turn is refused.
type CheckoutRefusal string

const (
	// CheckoutRefusedUnsupported means the gen's agent cannot check out a
	// commit (it never advertised capabilities.reviewCheckout), and it has
	// no snapshot whose restore a fresh gen would replace.
	CheckoutRefusedUnsupported CheckoutRefusal = "unsupported"
	// CheckoutRefusedNoReport means no reply came within the bound.
	CheckoutRefusedNoReport CheckoutRefusal = "no_report"
	// CheckoutRefusedHeadAbsent means WantSHA is still not in the pull
	// request's ref past the lag window, on a turn no lane asks for again.
	CheckoutRefusedHeadAbsent CheckoutRefusal = "head_absent"
	// CheckoutRefusedError means the checkout kept failing -- a fetch failure,
	// a busy sandbox or a failed checkout -- until the bound.
	CheckoutRefusedError CheckoutRefusal = "error"
)

// CheckoutRetirement is why a gen is retired.
type CheckoutRetirement string

const (
	// CheckoutRetiredOldAgent means the gen's agent cannot check out a commit,
	// and the sandbox has a snapshot, which may have brought that agent
	// back; a fresh gen boots a current one.
	CheckoutRetiredOldAgent CheckoutRetirement = "old_agent"
	// CheckoutRetiredFailing means the turn's checkouts on the gen failed
	// FailuresBeforeRetire times, on what its worktree holds.
	CheckoutRetiredFailing CheckoutRetirement = "failing"
)

// CheckoutVerdict is DecideReviewCheckout's answer.
type CheckoutVerdict struct {
	Action CheckoutAction
	// NextLook is when the session actor looks at the turn again, from
	// now, for CheckoutSend and CheckoutWait: the bound, a re-fetch of a
	// ref that lags, or a failed checkout's next try -- always positive,
	// never past the bound.
	NextLook time.Duration
	// AfterFailure, for CheckoutSend, is true when the reply the send
	// answers said failed: the turn's count of failures grows by one.
	AfterFailure bool
	// Refusal is set for CheckoutRefuse, Retirement for CheckoutRetireGen.
	Refusal    CheckoutRefusal
	Retirement CheckoutRetirement
	// Outcome is the reply's outcome as the decision read it: one of the
	// five, a value it does not know read as CheckoutFailed, and a
	// checked_out reply whose head is not WantSHA as well. "" with no
	// reply.
	Outcome CheckoutOutcome
	// HeadSHA, RefSHA and Error are the reply's, for the session actor's
	// logs and texts.
	HeadSHA, RefSHA, Error string
}

// DecideReviewCheckout decides what to do about a review turn's checkout
// (technical plan §21.1's table, in order):
//
//   - a gen that has sent no ready yet is waited for: whether it can check
//     out is not known until it connects, and a slow restore is no old
//     agent;
//   - a gen that cannot check out is retired, its snapshot cleared, when
//     the sandbox has a snapshot -- its restore may have brought back an
//     older agent, and a fresh gen boots a current one -- and otherwise
//     the turn is refused, naming the remedy;
//   - no request on the live gen: send one; a new gen starts a new bound;
//   - no reply: past the bound, retire a gen that answered failed before
//     this send, as below, and otherwise refuse; after a reconnect, send
//     again, since the command may have been lost with its socket;
//     otherwise wait for the bound;
//   - checked_out at WantSHA: proceed, unless the ref's tip is not
//     WantSHA and the turn is a pending attempt a lane asks for again.
//     Then, within the lag window, the ref may still lag a push whose head
//     is already in the clone -- a fresh boot clones every branch -- so it
//     is fetched again every RefetchInterval, as for sha_absent; past the
//     window the head moved, and the attempt ends context_moved. Any other
//     turn runs on WantSHA, the head it recorded, at once;
//   - sha_absent within the lag window: fetch again every
//     RefetchInterval; past it, the head is gone, which ends an attempt a
//     lane asks for again context_moved and refuses any other turn;
//   - fetch_failed, busy, failed -- and a checked_out at another head, or
//     an outcome the decision does not know, read as failed: the
//     FailuresBeforeRetire'th failed reply on the gen retires it, once per
//     turn; at the bound, a gen that answered failed at least once is
//     retired the same way rather than the turn refused, and any other
//     error refuses the turn, naming it; otherwise send again once the
//     wait has passed since the latest send: RefetchInterval doubled once
//     per earlier failed reply after a failed one, so a failing worktree
//     is retired FailedRetireBackoff after its first failure whatever came
//     before, and doubled once per earlier send after busy or
//     fetch_failed.
func DecideReviewCheckout(f CheckoutFacts, b CheckoutBounds) CheckoutVerdict {
	if f.AwaitingReady {
		return CheckoutVerdict{Action: CheckoutWait, NextLook: b.RefetchInterval}
	}
	if !f.GenCapable {
		if f.HasSnapshot {
			return CheckoutVerdict{Action: CheckoutRetireGen, Retirement: CheckoutRetiredOldAgent}
		}
		return CheckoutVerdict{Action: CheckoutRefuse, Refusal: CheckoutRefusedUnsupported}
	}
	if !f.RequestedOnGen {
		return CheckoutVerdict{Action: CheckoutSend, NextLook: b.Timeout}
	}
	remaining := b.Timeout - f.SinceRequest
	if f.Reply == nil {
		switch {
		case remaining <= 0 && f.Failures > 0 && !f.RetiredAGen:
			// A gen that answered failed before this send is retired at the
			// bound, as it is when the latest reply is in: a checkout that
			// fails slowly leaves its re-send unanswered at the bound.
			return CheckoutVerdict{Action: CheckoutRetireGen, Retirement: CheckoutRetiredFailing}
		case remaining <= 0:
			return CheckoutVerdict{Action: CheckoutRefuse, Refusal: CheckoutRefusedNoReport}
		case f.ReconnectedSinceSend:
			return CheckoutVerdict{Action: CheckoutSend, NextLook: remaining}
		default:
			return CheckoutVerdict{Action: CheckoutWait, NextLook: remaining}
		}
	}

	v := CheckoutVerdict{Outcome: f.Reply.Outcome, HeadSHA: f.Reply.HeadSHA, RefSHA: f.Reply.RefSHA, Error: f.Reply.Error}
	switch v.Outcome {
	case CheckoutCheckedOut:
		if v.HeadSHA != f.WantSHA {
			v.Outcome = CheckoutFailed
		}
	case CheckoutSHAAbsent, CheckoutFetchFailed, CheckoutBusy, CheckoutFailed:
	default:
		v.Outcome = CheckoutFailed
	}

	switch v.Outcome {
	case CheckoutCheckedOut:
		if v.RefSHA == "" || v.RefSHA == f.WantSHA || !f.MovedEndsTurn {
			v.Action = CheckoutProceed
			return v
		}
		if refetchLaggingRef(&v, f, b, remaining) {
			return v
		}
		v.Action = CheckoutEndMoved
		return v
	case CheckoutSHAAbsent:
		if refetchLaggingRef(&v, f, b, remaining) {
			return v
		}
		if f.MovedEndsTurn {
			v.Action = CheckoutEndMoved
			return v
		}
		v.Action, v.Refusal = CheckoutRefuse, CheckoutRefusedHeadAbsent
		return v
	}

	failed := v.Outcome == CheckoutFailed
	failures := f.Failures
	if failed {
		failures++
	}
	if failed && !f.RetiredAGen && failures >= b.FailuresBeforeRetire {
		v.Action, v.Retirement = CheckoutRetireGen, CheckoutRetiredFailing
		return v
	}
	if remaining <= 0 {
		if failures > 0 && !f.RetiredAGen {
			v.Action, v.Retirement = CheckoutRetireGen, CheckoutRetiredFailing
			return v
		}
		v.Action, v.Refusal = CheckoutRefuse, CheckoutRefusedError
		return v
	}
	backoff := checkoutBackoff(b.RefetchInterval, f.Sends)
	if failed {
		backoff = checkoutBackoff(b.RefetchInterval, failures)
	}
	if f.SinceSend < backoff {
		v.Action, v.NextLook = CheckoutWait, min(backoff-f.SinceSend, remaining)
		return v
	}
	v.Action, v.NextLook, v.AfterFailure = CheckoutSend, remaining, failed
	return v
}

// refetchLaggingRef sets v to fetch the ref again, or to wait for that,
// while the ref may still lag the push that made WantSHA: within the lag
// window and the bound, a send once RefetchInterval has passed since the
// latest, a wait until then. It reports whether it did; past the window
// it leaves v alone.
func refetchLaggingRef(v *CheckoutVerdict, f CheckoutFacts, b CheckoutBounds, remaining time.Duration) bool {
	if f.SinceRequest >= b.RefLagWindow || remaining <= 0 {
		return false
	}
	if f.SinceSend >= b.RefetchInterval {
		v.Action, v.NextLook = CheckoutSend, remaining
		return true
	}
	v.Action, v.NextLook = CheckoutWait, min(b.RefetchInterval-f.SinceSend, remaining)
	return true
}

// checkoutBackoff is the wait before the next send after the nth reply
// that called for one: interval, doubled n-1 times -- saturating, never
// overflowing.
func checkoutBackoff(interval time.Duration, n int) time.Duration {
	backoff := interval
	for i := 1; i < n; i++ {
		if backoff > maxCheckoutBackoff/2 {
			return maxCheckoutBackoff
		}
		backoff *= 2
	}
	return backoff
}

// maxCheckoutBackoff is the longest time.Duration.
const maxCheckoutBackoff = time.Duration(1<<63 - 1)
