// This file (reviewcontextcheck.go) implements technical plan §24.9's
// second rule: a review attempt that waited behind another turn checks, as
// it is dispatched, that the head, base and ancestor chain it recorded are
// still its pull request's. A review runs with the diff and context
// captured when it was inserted, so one queued behind a turn while the
// pull request moved would review code that no longer exists.
//
// # Two halves, like every dispatch
//
// The comparison needs the code host: the pull request's live head and
// base ref (GetOpenPR), the base branch's live tip and, when it moved, the
// ancestry check of reviewfreshness.ReadLive. A network read never holds a
// transaction open (dispatch.go's top comment), so handleEnsureDispatched
// reads it first, outside any transaction (preReadReviewContext), for the
// turn the evaluation is about to pick, and planDispatch applies the answer
// inside its own transaction (applyReviewContextCheck), only to the turn it
// picks, and only when the answer is for that turn: a check for another
// turn, or none, dispatches nothing this round and re-arms the dispatch
// timer due at once, so the next evaluation reads the pick it makes then.
//
// # What it compares, and what each answer does
//
// The recorded side is the turn's own review_head_sha and
// review_verdict_context, compared through autoapproval.CheckFreshness --
// the one comparison the merge path, a session's result and the decision
// inbox share (§21.1b) -- with the policy version set equal on both sides,
// so only the head, the base (a fast-forward of an unchanged base ref
// confirmed by the code host is no move) and the ancestor chain can move.
//
//   - fresh: the attempt starts, and the pull request's count of
//     automatic attempts in a row that met a moved context starts again;
//   - moved: the attempt does not start (endContextMovedTurn). It ends
//     through the machine's abandon edge with the end reason
//     context_moved, which the session's status derivation and every
//     reader of attempts skip: it notifies nobody, publishes no check, and
//     fails nothing. In the same transaction an automatic request goes
//     back to the lane -- the pull request's pending head set again if no
//     push left one, the debounce due at once if none is armed (its
//     insert consumed it, and a moved base brings no push) -- and the
//     count grows by one; past ReviewContextMoveMaxConsecutive the request
//     is dropped instead, and the session's status shows the drop. When a
//     younger automatic attempt already queued recorded the live head, it
//     stands for the request, and nothing is asked again. A person's
//     request (the label, the button) is owed to its requester
//     instead (oweReviewRequest): a row of owed_review_requests with its
//     count of moves in a row, and the owed_review_request timer due at
//     once, whose consumer re-runs it for the head the pull request has
//     now (owedreviewrequest.go);
//   - unconfirmed: a fact could not be read, or is unknown on either side.
//     The attempt starts, as every attempt did before this rule, and its
//     turn records that its context was unconfirmed at start; the
//     verdict's own freshness check still stands behind it.
//
// Only a review attempt a lane that records its trigger asked for is
// checked -- the automatic re-review, or a person's request
// (turn.ContextCheckedAtDispatch) -- and only one that waited behind
// another turn: an attempt dispatched at once costs no read of the code
// host. A person's request queues routinely behind the review it follows,
// so its check is the common case; an automatic one queues only rarely,
// since the hold keeps the lane from inserting behind an open turn.

package sessionactor

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/narvidev/narvi/internal/adapters/outbound/postgres/sqlcgen"
	"github.com/narvidev/narvi/internal/app/ports"
	"github.com/narvidev/narvi/internal/app/reviewfreshness"
	"github.com/narvidev/narvi/internal/domain/autoapproval"
	"github.com/narvidev/narvi/internal/domain/reposource"
	"github.com/narvidev/narvi/internal/domain/reviewverdict"
	"github.com/narvidev/narvi/internal/domain/turn"
)

// ReviewLiveReader is what technical plan §24.9's context check reads a
// pull request's live facts with: the pull request as it stands
// (ports.SourceControl's GetOpenPR, its head, base ref and ancestor
// chain), and the two reads reviewfreshness.ReadLive makes of the base
// branch and the ancestor link. ports.SourceControl satisfies it; the
// control plane hands in the same decorated instance every other
// freshness read goes through.
type ReviewLiveReader interface {
	GetOpenPR(ctx context.Context, owner, repo string, number int, token string) (pr ports.OpenPR, found bool, err error)
	reviewfreshness.LiveReader
}

var _ ReviewLiveReader = ports.SourceControl(nil)

// reviewContextOutcome is what the context check found for one turn.
type reviewContextOutcome int

const (
	// reviewContextUnchecked: the turn is one the check applies to, but it
	// waited behind no other turn, so it is dispatched as it would be at
	// once, unchecked.
	reviewContextUnchecked reviewContextOutcome = iota + 1
	// reviewContextUnread: the turn needs a check, and the pre-read made
	// none -- the session's sandbox could not take the turn then. A
	// dispatch that finds the sandbox live after all treats it as no
	// check.
	reviewContextUnread
	// reviewContextFresh: the recorded head, base and ancestor chain are
	// the pull request's, read live.
	reviewContextFresh
	// reviewContextMoved: one of them is not.
	reviewContextMoved
	// reviewContextUnconfirmed: the comparison could not be made.
	reviewContextUnconfirmed
)

func (o reviewContextOutcome) String() string {
	switch o {
	case reviewContextUnchecked:
		return "unchecked"
	case reviewContextUnread:
		return "unread"
	case reviewContextFresh:
		return "fresh"
	case reviewContextMoved:
		return "moved"
	case reviewContextUnconfirmed:
		return "unconfirmed"
	default:
		return fmt.Sprintf("reviewContextOutcome(%d)", int(o))
	}
}

// reviewContextCheck is the pre-read's answer for one turn: the session's
// next turn to dispatch as the pre-read saw it.
type reviewContextCheck struct {
	turnID  pgtype.UUID
	outcome reviewContextOutcome
	// reason says why the context moved or could not be confirmed: the
	// comparison's own autoapproval.Reason, or what could not be read.
	reason string
	// liveHeadSHA is the pull request's head as the pre-read read it --
	// the head a moved attempt's re-request names. Empty when it was not
	// read.
	liveHeadSHA string
}

// preReadReviewContext is the first half of the context check, run by
// handleEnsureDispatched before each dispatch evaluation and outside any
// transaction: on a pull request's review session it reads the session's
// next turn to dispatch (TurnStore.ReviewAttemptToCheck) and, when the
// check applies to it, it
// waited behind another turn and the session's sandbox can take it now,
// compares its recorded context with the pull request's live one. nil when
// there is no turn to dispatch, the turn is not one the check applies to,
// or the read of the turn failed -- planDispatch then checks nothing, and
// holds back only a turn the check applies to (applyReviewContextCheck).
func (a *Actor) preReadReviewContext(ctx context.Context) *reviewContextCheck {
	if a.spawnSource != sqlcgen.SessionSpawnSourceGithub {
		return nil
	}
	row, err := a.stores.turn.ReviewAttemptToCheck(ctx, a.sessionID)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil
	}
	if err != nil {
		a.logger.Warn("sessionactor: review context check: read the next turn to dispatch failed; checking nothing this round", "error", err)
		return nil
	}
	if !a.contextCheckApplies(row.IsReviewAttempt, row.RequestTrigger) {
		return nil
	}
	check := &reviewContextCheck{turnID: row.ID}
	switch {
	case !row.Queued:
		check.outcome = reviewContextUnchecked
		return check
	case !row.SandboxLive:
		check.outcome = reviewContextUnread
		return check
	}
	check.outcome, check.reason, check.liveHeadSHA = a.compareReviewContext(ctx, row)
	a.logger.Info("sessionactor: review context check: a queued review attempt's context read before dispatch",
		"turn_id", row.ID.String(), "request_trigger", stringOrEmpty(row.RequestTrigger), "outcome", check.outcome.String(), "reason", check.reason, "live_head_sha", check.liveHeadSHA)
	return check
}

// contextCheckApplies reports whether a turn is one the context check
// applies to (turn.ContextCheckedAtDispatch), on a pull request's review
// session: the GitHub lane's sessions are the only ones a review lane asks
// for turns in, so no other session's dispatch reads anything for it. The
// pre-read and the dispatch ask the same question, so a turn the pre-read
// skips is never one the dispatch holds back for want of a read.
func (a *Actor) contextCheckApplies(isReviewAttempt bool, requestTrigger *string) bool {
	return a.spawnSource == sqlcgen.SessionSpawnSourceGithub && turn.ContextCheckedAtDispatch(isReviewAttempt, requestTrigger)
}

// The reasons compareReviewContext gives when it cannot read what it
// needs. Every one is an unconfirmed answer, never an error.
const (
	reviewContextReasonNoReader       = "no code host connection is configured to read the pull request's live state"
	reviewContextReasonNoToken        = "GitHub outbound is off, so the pull request cannot be read as the bot"
	reviewContextReasonNoPullRequest  = "the review session claims no pull request"
	reviewContextReasonNoHead         = "the attempt recorded no head"
	reviewContextReasonContextUnread  = "the attempt's recorded context could not be read"
	reviewContextReasonPRUnread       = "the pull request's live state could not be read from the code host"
	reviewContextReasonPRNotOpen      = "the pull request is no longer open"
	reviewContextReasonRepoMalformed  = "the review session's repository is not in owner/repo shape"
	reviewContextReasonClaimUnread    = "the review session's pull request claim could not be read"
	reviewContextReasonContextMissing = "the attempt recorded no context"
)

// compareReviewContext compares row's recorded head and context with the
// pull request's live facts, in the order a session's result runs the
// same comparison (reviewfreshness.Assess): the pull request read live; the
// merge path's probe, which finds a moved head before any further read;
// ReadLive's reads of the base branch and the ancestor link; then the
// comparison over the live facts. The recorded policy version is replaced
// by the current one, so a policy change is never a move: only the head,
// the base and the ancestor chain can move. It returns the outcome --
// fresh, moved or unconfirmed -- with its reason, and the live head when
// the pull request was read.
func (a *Actor) compareReviewContext(ctx context.Context, row sqlcgen.GetReviewAttemptToCheckRow) (reviewContextOutcome, string, string) {
	if a.reviewLiveReader == nil {
		return reviewContextUnconfirmed, reviewContextReasonNoReader, ""
	}
	if a.githubOutbound == nil {
		return reviewContextUnconfirmed, reviewContextReasonNoToken, ""
	}
	if row.ReviewHeadSha == nil || *row.ReviewHeadSha == "" {
		return reviewContextUnconfirmed, reviewContextReasonNoHead, ""
	}
	if len(row.ReviewVerdictContext) == 0 {
		return reviewContextUnconfirmed, reviewContextReasonContextMissing, ""
	}
	var recorded reviewverdict.Context
	if err := json.Unmarshal(row.ReviewVerdictContext, &recorded); err != nil {
		a.logger.Warn("sessionactor: review context check: the attempt's recorded context does not decode", "turn_id", row.ID.String(), "error", err)
		return reviewContextUnconfirmed, reviewContextReasonContextUnread, ""
	}
	// Only the head, the base and the ancestor chain may move (technical
	// plan §24.9): a verdict produced under an older policy is the
	// verdict's freshness check's concern, not a reason not to run.
	recorded.PolicyVersion = autoapproval.CurrentPolicyVersion

	prSession, err := a.stores.githubPRSession.GetBySessionID(ctx, a.sessionID)
	if errors.Is(err, pgx.ErrNoRows) {
		return reviewContextUnconfirmed, reviewContextReasonNoPullRequest, ""
	}
	if err != nil {
		a.logger.Warn("sessionactor: review context check: read the session's pull request claim failed", "error", err)
		return reviewContextUnconfirmed, reviewContextReasonClaimUnread, ""
	}
	owner, repo, ok := reposource.SplitFullName(prSession.RepoFullName)
	if !ok {
		return reviewContextUnconfirmed, reviewContextReasonRepoMalformed, ""
	}
	token := a.githubOutbound.BotToken()

	readCtx, cancel := context.WithTimeout(ctx, a.timeouts.GitHubGetOpenPRTimeout)
	target, found, err := a.reviewLiveReader.GetOpenPR(readCtx, owner, repo, int(prSession.PrNumber), token)
	cancel()
	if err != nil {
		a.logger.Warn("sessionactor: review context check: live pull request read failed", "error", err, "repo_full_name", prSession.RepoFullName, "pr_number", prSession.PrNumber)
		return reviewContextUnconfirmed, reviewContextReasonPRUnread, ""
	}
	// GetOpenPR is a composite read: a deadline that fired partway answers
	// no error with fields left blank, never a fact (reviewfreshness.Assess
	// guards the same way).
	if errors.Is(readCtx.Err(), context.DeadlineExceeded) {
		return reviewContextUnconfirmed, reviewContextReasonPRUnread, ""
	}
	if !found {
		return reviewContextUnconfirmed, reviewContextReasonPRNotOpen, ""
	}

	record := reviewverdict.Record{HeadSHA: *row.ReviewHeadSha, Context: recorded}
	if reason := autoapproval.CheckFreshness(reviewfreshness.ProbeInput(record, target)); reason != autoapproval.ReasonNone {
		outcome := reviewContextOutcomeFor(reason)
		return outcome, string(reason), target.HeadSHA
	}
	live, failure := reviewfreshness.ReadLive(ctx, a.timeouts, a.reviewLiveReader, token, target, recorded)
	if failure != nil {
		a.logger.Warn("sessionactor: review context check: live freshness read failed", "step", string(failure.Step), "error", failure.Err, "repo_full_name", prSession.RepoFullName, "pr_number", prSession.PrNumber)
		return reviewContextUnconfirmed, failure.Describe(), target.HeadSHA
	}
	reason := autoapproval.CheckFreshness(reviewfreshness.FreshnessInput(record, live))
	return reviewContextOutcomeFor(reason), string(reason), target.HeadSHA
}

// reviewContextOutcomeFor maps the comparison's answer to the check's: a
// fact that differs is a move, a fact that cannot be established is
// unconfirmed (autoapproval.ClassifyFreshness, which never reads an unknown
// as current).
func reviewContextOutcomeFor(reason autoapproval.Reason) reviewContextOutcome {
	switch autoapproval.ClassifyFreshness(reason) {
	case autoapproval.FreshnessCurrent:
		return reviewContextFresh
	case autoapproval.FreshnessStale:
		return reviewContextMoved
	default:
		return reviewContextUnconfirmed
	}
}

// reviewContextDecision is what applyReviewContextCheck tells planDispatch
// to do with the turn it picked.
type reviewContextDecision int

const (
	// reviewContextDispatch: dispatch the pick as planned.
	reviewContextDispatch reviewContextDecision = iota + 1
	// reviewContextHold: dispatch nothing this round; the dispatch timer
	// was re-armed due at once, and the next evaluation reads the pick
	// again.
	reviewContextHold
	// reviewContextEnded: the pick ended context_moved; evaluate the next
	// one.
	reviewContextEnded
)

// applyReviewContextCheck is the second half of the context check, inside
// planDispatch's transaction, for target, the turn it is about to dispatch
// to a live sandbox. A turn the check does not apply to is dispatched as
// it always was. For one it applies to, check must be the pre-read's
// answer for that very turn: with none (no pre-read, one for another turn,
// or one that made no read), the pick is held back -- the dispatch timer
// re-armed due at once, nothing dispatched -- and the next evaluation's
// pre-read reads it. Otherwise:
//
//   - unchecked (it waited behind no turn), fresh or unconfirmed: it is
//     dispatched, an unconfirmed one first stamped context_unconfirmed_at,
//     and, for an automatic attempt, the pull request's count of automatic
//     attempts in a row that met a moved context starts again -- one of
//     them is starting. A person's request keeps its count on its own turn
//     (turns.context_moves), so a turn of it that starts ends the request
//     and its count with it;
//   - moved: endContextMovedTurn ends it, and planDispatch evaluates again.
func (a *Actor) applyReviewContextCheck(ctx context.Context, tx pgx.Tx, turns []sqlcgen.Turn, target sqlcgen.Turn, check *reviewContextCheck, now time.Time) (reviewContextDecision, error) {
	if !a.contextCheckApplies(target.IsReviewAttempt, target.RequestTrigger) {
		return reviewContextDispatch, nil
	}
	if check == nil || check.turnID != target.ID || check.outcome == reviewContextUnread {
		a.logger.Info("sessionactor: review context check: the automatic review attempt to dispatch was not read before this evaluation; holding it for the next one",
			"turn_id", target.ID.String())
		if err := a.armDispatchNow(ctx, tx); err != nil {
			return 0, err
		}
		return reviewContextHold, nil
	}
	switch check.outcome {
	case reviewContextMoved:
		if err := a.endContextMovedTurn(ctx, tx, turns, target, check, now); err != nil {
			return 0, err
		}
		return reviewContextEnded, nil
	case reviewContextUnconfirmed:
		if _, err := a.stores.turn.WithTx(tx).SetContextUnconfirmed(ctx, target.ID); err != nil {
			return 0, fmt.Errorf("sessionactor: record the review attempt's unconfirmed context: %w", err)
		}
		a.logger.Warn("sessionactor: review context check: a queued review attempt starts with its context unconfirmed",
			"turn_id", target.ID.String(), "request_trigger", stringOrEmpty(target.RequestTrigger), "reason", check.reason)
	case reviewContextUnchecked, reviewContextFresh:
	default:
		return 0, fmt.Errorf("sessionactor: unhandled review context outcome %s", check.outcome)
	}
	if turn.IsHumanRequestTrigger(target.RequestTrigger) {
		return reviewContextDispatch, nil
	}
	if _, err := a.stores.githubPRSession.WithTx(tx).ResetAutoRetriggerContextMoves(ctx, a.sessionID); err != nil {
		return 0, fmt.Errorf("sessionactor: reset the automatic re-review's moved-context count: %w", err)
	}
	return reviewContextDispatch, nil
}

// contextMovedReasonPrefix opens the reason of a context_moved turn's
// synthetic execution_complete; the comparison's own reason follows it.
const contextMovedReasonPrefix = "review context moved before the review started: "

// endContextMovedTurn ends target, a pending review attempt whose
// recorded context the pull request has moved past, without running it
// (technical plan §24.9), in planDispatch's transaction:
//
//   - the machine's abandon edge, pending to failed, written through the
//     recorder with the end reason context_moved, so a held re-review
//     wakes like on any turn's end, and the session's status derivation
//     and every reader of attempts skip it;
//   - a synthetic execution_complete from pending, marked "delivered":
//     false -- no prompt was sent, so no agent of it can be running -- and
//     no channel notice, no check, no session warning, no workflow hook:
//     it notifies nobody and fails nothing;
//   - the session's status re-derived without it;
//   - a person's request (the label, the button) owed to its
//     requester (oweReviewRequest): a row of owed_review_requests, its
//     count of moves in a row grown by one, and the owed_review_request
//     timer due at once, whose consumer re-runs the request for the head
//     the pull request has now or drops it past the bound;
//   - an automatic request back to the lane: the pull request's pending
//     head set to the live head unless a push left one, the count of
//     moves in a row grown by one, and the debounce armed due at once if
//     the session has none (a held one is woken by this very end) -- unless
//     a younger automatic attempt already queued recorded the live head,
//     which then stands for the request (requeueAutoRetrigger). A move
//     that takes the count past ReviewContextMoveMaxConsecutive drops the
//     request instead: the pending head cleared and the drop recorded for
//     the session's status, the debounce deleted. The budget slot the
//     attempt spent is not given back: §24.6's count only grows, and the
//     bound caps what moves can cost;
//   - the dispatch timer re-armed due at once, the trigger of the rest of
//     the session's queue, which planDispatch evaluates again at once.
func (a *Actor) endContextMovedTurn(ctx context.Context, tx pgx.Tx, turns []sqlcgen.Turn, target sqlcgen.Turn, check *reviewContextCheck, now time.Time) error {
	from := turn.State(target.Status)
	to, err := turn.Transition(from, turn.TriggerAbandon)
	if err != nil {
		return fmt.Errorf("sessionactor: end review attempt %s whose context moved: %w", target.ID.String(), err)
	}
	endReason := turn.EndReasonContextMoved
	if _, err := a.turnWrites(tx).UpdateStatus(ctx, sqlcgen.UpdateTurnStatusParams{
		ID:          target.ID,
		Status:      sqlcgen.TurnStatus(to),
		CompletedAt: pgtype.Timestamptz{Time: now, Valid: true},
		EndReason:   &endReason,
	}); err != nil {
		return fmt.Errorf("sessionactor: update turn status: %w", err)
	}
	if turn.RequiresSyntheticExecutionComplete(turn.TriggerAbandon) {
		payload := syntheticExecutionComplete(target.ID, from, contextMovedReasonPrefix+check.reason)
		payload[syntheticUndeliveredKey] = false
		if err := a.appendEvent(ctx, tx, "execution_complete", payload); err != nil {
			return err
		}
	}
	failureReason, _ := turn.DeriveFailureReason(from, turn.TriggerAbandon)
	summaries := summariesWithOverride(turns, target.ID, to, failureReason)
	for i, t := range turns {
		if t.ID == target.ID {
			summaries[i].Ignored = true
		}
	}
	if err := a.persistDerivedSessionStatus(ctx, tx, summaries); err != nil {
		return err
	}
	a.logger.Info("sessionactor: review context check: a queued review attempt's context moved; it ends without running and its request is asked again",
		"turn_id", target.ID.String(), "request_trigger", stringOrEmpty(target.RequestTrigger), "reason", check.reason, "live_head_sha", check.liveHeadSHA)

	if turn.IsHumanRequestTrigger(target.RequestTrigger) {
		if err := a.oweReviewRequest(ctx, tx, target); err != nil {
			return err
		}
	} else if err := a.requeueAutoRetrigger(ctx, tx, turns, target, check); err != nil {
		return err
	}
	return a.armDispatchNow(ctx, tx)
}

// armDispatchNow arms the session's dispatch timer due at the database's
// now, in tx -- the statement the transaction that creates a turn arms it
// with (TimerStore.ArmDispatch), so every arm of this timer reads one
// clock, and a pump that compares fires_at with the database's now finds it
// due at once whatever this replica's clock says.
func (a *Actor) armDispatchNow(ctx context.Context, tx pgx.Tx) error {
	if err := a.stores.timer.WithTx(tx).ArmDispatch(ctx, a.sessionID); err != nil {
		return fmt.Errorf("sessionactor: arm the dispatch timer: %w", err)
	}
	return nil
}

// requeueAutoRetrigger is endContextMovedTurn's re-request of the
// automatic re-review (technical plan §24.9), in the same transaction:
// RequeueAutoRetrigger, then the debounce armed due at once if the session
// has none -- or, past ReviewContextMoveMaxConsecutive, the drop. A review
// session with no pull request claim has no lane to go back to, and the
// turn's end alone stands.
//
// Nothing is re-requested when a younger automatic attempt already queued
// in the session recorded the head the pull request has now
// (liveHeadCoveredByYoungerAttempt): the evaluation that follows picks it
// next and checks it like this one, so it is the review of that head. A
// re-request beside it would leave a pending head and a debounce that,
// once it runs, review the same head again -- the heads comparison only
// sees a posted live verdict, which a shadow repository or an attempt left
// not assessed never leaves -- and spend another slot of the budget. The
// move is not counted either: the request is not asked again, and the
// younger attempt's own check counts or resets it.
func (a *Actor) requeueAutoRetrigger(ctx context.Context, tx pgx.Tx, turns []sqlcgen.Turn, target sqlcgen.Turn, check *reviewContextCheck) error {
	if liveHeadCoveredByYoungerAttempt(turns, target, check.liveHeadSHA) {
		a.logger.Info("sessionactor: review context check: a younger automatic review attempt already queued is of the live head; nothing asked again",
			"turn_id", target.ID.String(), "live_head_sha", check.liveHeadSHA)
		return nil
	}
	prSessions := a.stores.githubPRSession.WithTx(tx)
	prSession, err := prSessions.GetBySessionID(ctx, a.sessionID)
	if errors.Is(err, pgx.ErrNoRows) {
		a.logger.Warn("sessionactor: review context check: the session claims no pull request; nothing to ask again", "turn_id", target.ID.String())
		return nil
	}
	if err != nil {
		return fmt.Errorf("sessionactor: get github pr session: %w", err)
	}
	head := check.liveHeadSHA
	if head == "" && target.ReviewHeadSha != nil {
		head = *target.ReviewHeadSha
	}
	requeued, err := prSessions.RequeueAutoRetrigger(ctx, prSession.RepoFullName, prSession.PrNumber, head)
	if errors.Is(err, pgx.ErrNoRows) {
		a.logger.Warn("sessionactor: review context check: the pull request claim has no session; nothing to ask again", "turn_id", target.ID.String())
		return nil
	}
	if err != nil {
		return fmt.Errorf("sessionactor: requeue the automatic re-review: %w", err)
	}
	pending := head
	if requeued.PendingRetriggerHeadSha != nil {
		pending = *requeued.PendingRetriggerHeadSha
	}
	if int(requeued.AutoRetriggerContextMoves) > a.timeouts.ReviewContextMoveMaxConsecutive {
		if _, err := prSessions.DropAutoRetrigger(ctx, prSession.RepoFullName, prSession.PrNumber, pending); err != nil {
			return fmt.Errorf("sessionactor: drop the automatic re-review: %w", err)
		}
		a.logger.Warn("sessionactor: review context check: the automatic re-review gave up: its attempts met a moved context too many times in a row",
			"repo_full_name", prSession.RepoFullName, "pr_number", prSession.PrNumber, "head_sha", pending,
			"moves", requeued.AutoRetriggerContextMoves, "max", a.timeouts.ReviewContextMoveMaxConsecutive)
		return a.deleteTimer(ctx, tx, TimerReviewRetriggerDebounce)
	}
	if _, err := a.stores.timer.WithTx(tx).RequeueReviewRetriggerDebounce(ctx, a.sessionID); err != nil {
		return fmt.Errorf("sessionactor: re-arm the re-review debounce: %w", err)
	}
	return nil
}

// liveHeadCoveredByYoungerAttempt reports whether turns -- the session's,
// as planDispatch read them -- hold a pending review attempt the automatic
// lane asked for, created after target, that recorded liveHead: a review
// of the head the pull request has now is already queued behind target,
// and is checked in its turn. An empty liveHead covers nothing.
func liveHeadCoveredByYoungerAttempt(turns []sqlcgen.Turn, target sqlcgen.Turn, liveHead string) bool {
	if liveHead == "" {
		return false
	}
	for _, t := range turns {
		if t.ID == target.ID || turn.State(t.Status) != turn.StatePending ||
			!t.IsReviewAttempt || t.RequestTrigger == nil || *t.RequestTrigger != turn.RequestTriggerAuto ||
			t.ReviewHeadSha == nil || *t.ReviewHeadSha != liveHead {
			continue
		}
		if t.CreatedAt.Time.After(target.CreatedAt.Time) ||
			(t.CreatedAt.Time.Equal(target.CreatedAt.Time) && bytesAfter(t.ID.Bytes, target.ID.Bytes)) {
			return true
		}
	}
	return false
}

// bytesAfter orders two ids the way Postgres orders uuids, byte by byte:
// the tie-break ListTurnsForSession and GetReviewAttemptToCheck read.
func bytesAfter(a, b [16]byte) bool {
	for i := range a {
		if a[i] != b[i] {
			return a[i] > b[i]
		}
	}
	return false
}

// oweReviewRequest is endContextMovedTurn's re-request of a person's
// review request (technical plan §24.9), in the same transaction: the
// request target stood for -- who asked, through which lane, with what
// text -- becomes a row of owed_review_requests, its count of moves in a
// row the turn's own (none for a turn no re-run carried) plus this one,
// and the session's owed_review_request
// timer is armed due at once on the database's clock. The timer's consumer
// (handleOwedReviewRequestTimer) re-runs it for the head the pull request
// has now, on the person's path, or drops it past
// ReviewContextMoveMaxConsecutive and tells its requester; meanwhile the
// row holds the automatic lane (ReviewRetriggerHeld) and the timer keeps
// the session reading scheduled. Both are rows, so a restart between the
// move and the re-run loses nothing.
func (a *Actor) oweReviewRequest(ctx context.Context, tx pgx.Tx, target sqlcgen.Turn) error {
	moves := int32(1)
	if target.ContextMoves != nil {
		moves += *target.ContextMoves
	}
	owed, err := a.stores.owedReviewRequest.WithTx(tx).Insert(ctx, sqlcgen.InsertOwedReviewRequestParams{
		SessionID:       a.sessionID,
		RequestedBy:     target.RequestedBy,
		Trigger:         *target.RequestTrigger,
		RequestText:     target.RequestText,
		IsReviewAttempt: target.IsReviewAttempt,
		ContextMoves:    moves,
		MovedTurnID:     target.ID,
	})
	if err != nil {
		return fmt.Errorf("sessionactor: owe the review request: %w", err)
	}
	if err := a.stores.timer.WithTx(tx).ArmOwedReviewRequest(ctx, a.sessionID); err != nil {
		return fmt.Errorf("sessionactor: arm the owed review request timer: %w", err)
	}
	a.logger.Info("sessionactor: review context check: a person's review request is owed: it is re-run for the head the pull request has now",
		"turn_id", target.ID.String(), "owed_id", owed.ID.String(), "request_trigger", owed.Trigger, "moves", owed.ContextMoves)
	return nil
}
