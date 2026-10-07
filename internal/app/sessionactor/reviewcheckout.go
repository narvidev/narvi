// This file (reviewcheckout.go) holds a review turn until its sandbox
// holds the head it recorded (technical plan §21.1, §30.4). A turn of a
// pull request's review session that records a head
// (turns.review_head_sha) reviews that commit's tree: a warm sandbox holds
// whatever the previous turn left, a restored one holds the snapshot's,
// and a fresh one the ref's tip at boot, so the actor checks the commit
// out before the turn's prompt is sent.
//
// # The checkout is data on the pending turn
//
// No turn state or edge is added, as §3.3's prompt receipts add none. The
// turn stays pending -- its status reads queued -- while:
//
//   - the dispatch evaluation that would send it records, in its own
//     commit, a checkout command for the live gen (turns.checkout_*,
//     RecordTurnCheckoutRequest) and arms the dispatch timer at the bound;
//     after the commit the command is written to the sandbox's socket
//     (executeCheckout), and the handler returns -- it never waits;
//   - the agent fetches the pull request's ref from the base repository,
//     checks the commit out detached and forced, removes untracked files,
//     and answers with a checkout_result whose messageId is
//     'checkout_result:{command messageId}'. handleSandboxEvent stores it
//     through its generic path, as a binary that does not know the type
//     also does, and the stored row is the reply, read by that key
//     (GetTurnCheckoutState);
//   - the reply's own post-commit dispatch evaluation reads it, and
//     turn.DecideReviewCheckout says what follows: dispatch the turn,
//     recording the commit in the dispatching commit
//     (turns.checked_out_sha); send again -- after a reconnect, a ref that
//     lags, a failed fetch, a busy sandbox or a failed checkout, the last
//     three spaced by a doubling interval -- or wait; end an attempt whose
//     head moved; refuse the turn, naming why; or retire the gen.
//
// Which turns: one on a session that claims a pull request
// (github_pr_sessions), whose recorded head is a full commit id, and whose
// primary repo's url names the claim's repository -- the rule
// SESSION_CONFIG's ref is set by (pullRequestRef), so the ref a checkout
// names is the one the sandbox booted with. The GitHub ingress writes the
// base repository for every pull request, a fork's included; a session
// created before it did names the fork, and is moved onto the base only
// when no gen holds its old spec (reviewbaserepository.go) -- until then,
// on the gen that cloned the fork, it dispatches as before. Every other
// turn dispatches exactly as before.
//
// # What ends the wait
//
//   - The head moved. The pull request's ref holds another tip, or still
//     lacks the head past ReviewCheckoutRefLagWindow. A pending review
//     attempt a lane asks for again (turn.ContextCheckedAtDispatch: the
//     automatic re-review, the label, the button) ends context_moved
//     through technical plan §24.9's endContextMovedTurn, and its request
//     is asked again for the head the pull request has now. Any other turn
//     -- a mention-opened first review, a follow-up mention -- runs on the
//     head it recorded, checked out exactly; its verdict is anchored to it,
//     and every freshness reader shows it stale. One whose head is gone
//     from the ref is refused.
//   - An agent too old to check out: a gen whose latest ready did not
//     advertise capabilities.reviewCheckout is never sent the command. If
//     the sandbox has a snapshot -- whose restore may have brought that
//     agent back -- the gen is retired and the snapshot cleared, in one
//     transaction, and the next gen boots fresh; otherwise the turn is
//     refused, naming the remedy: rebuild the image.
//   - A worktree that fails every checkout: ReviewCheckoutFailuresBeforeRetire
//     failed replies on one gen retire it the same way, once per turn
//     (turns.checkout_retired_gen) -- a git killed mid-command leaves a
//     stale index.lock or a broken index, which the snapshot holds too.
//   - The bound, ReviewCheckoutTimeout from the first request on the gen:
//     no reply refuses the turn ("did not report"), and so does a checkout
//     that kept failing, naming the error the agent reported.
//
// A refused pending turn ends as one refused before it started
// (refusePersonalLinkOnly's shape); a turn already processing, re-sent to
// a new gen, is failed forward instead (failDispatchedTurn), never marked
// undelivered: an earlier gen did receive its prompt.
//
// # Composition with §24.9's context check
//
// The checkout runs first, on every turn it applies to, comparing the head
// alone, in the sandbox, at no cost on the code host. §24.9's check runs
// after the checkout is confirmed, in the evaluation that sends the
// prompt: its pre-read skips the code host while the checkout is
// outstanding (reviewCheckoutOutstanding), deciding that with this file's
// own rules over the same rows, so the two never disagree about which
// turn waits on a checkout.

package sessionactor

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/narvidev/narvi/contracts/gen/go/sandboxws"
	"github.com/narvidev/narvi/internal/adapters/outbound/postgres/sqlcgen"
	"github.com/narvidev/narvi/internal/app/workflowengine"
	"github.com/narvidev/narvi/internal/domain/reposource"
	"github.com/narvidev/narvi/internal/domain/reviewcheck"
	"github.com/narvidev/narvi/internal/domain/sandbox"
	"github.com/narvidev/narvi/internal/domain/turn"
)

// The outcome attribute of review_checkout_total (opsmetrics.go).
const (
	reviewCheckoutOutcomeSent            = "sent"
	reviewCheckoutOutcomeSendFailed      = "send_failed"
	reviewCheckoutOutcomeCheckedOut      = "checked_out"
	reviewCheckoutOutcomeContextMoved    = "context_moved"
	reviewCheckoutOutcomeUnsupported     = "unsupported"
	reviewCheckoutOutcomeNoReport        = "no_report"
	reviewCheckoutOutcomeHeadAbsent      = "head_absent"
	reviewCheckoutOutcomeError           = "error"
	reviewCheckoutOutcomeRetiredOldAgent = "retired_old_agent"
	reviewCheckoutOutcomeRetiredFailing  = "retired_failing"
)

// reviewCheckoutTarget is what a review turn's checkout names: the
// session's primary repo, by its SESSION_CONFIG name, the pull request's
// head ref in the base repository, and the commit the turn recorded.
type reviewCheckoutTarget struct {
	repoName string
	ref      string
	sha      string
	// repoFullName is the base repository, owner/name, the claim's.
	repoFullName string
}

// reviewCheckoutTargetFor returns the checkout a turn needs, and false when
// it needs none: the session claims no pull request, the turn recorded no
// head, or its primary repo's url does not name the claim's repository --
// pullRequestRef's own rule, so the ref is the one the sandbox booted
// with. A head that is not a full commit id is reported by badSHA, never
// sent: no code host records one, and the command's contract refuses it.
func reviewCheckoutTargetFor(claim *sqlcgen.GithubPrSession, sessionRepos []byte, headSHA *string) (target reviewCheckoutTarget, ok, badSHA bool, err error) {
	if claim == nil || headSHA == nil || *headSHA == "" {
		return reviewCheckoutTarget{}, false, false, nil
	}
	repos, err := reposFromJSON(sessionRepos)
	if err != nil {
		return reviewCheckoutTarget{}, false, false, err
	}
	if len(repos) == 0 {
		return reviewCheckoutTarget{}, false, false, nil
	}
	ref := pullRequestRef(claim, repos[0])
	if ref == nil {
		return reviewCheckoutTarget{}, false, false, nil
	}
	if reposource.ValidateCommitSHA(*headSHA) != nil {
		return reviewCheckoutTarget{}, false, true, nil
	}
	return reviewCheckoutTarget{repoName: repos[0].Name, ref: *ref, sha: *headSHA, repoFullName: claim.RepoFullName}, true, false, nil
}

// readyAdvertisesReviewCheckout reports whether a ready event advertises
// capabilities.reviewCheckout. A ready that fails its schema decode counts
// as not advertising it -- the safe direction, in which no checkout is
// sent -- as readyAdvertisesPromptReceipt does (promptreceipt.go). Its
// lifetimeRemainingSeconds plays no part (decodeReady, framekey.go).
func readyAdvertisesReviewCheckout(raw json.RawMessage) bool {
	evt, err := decodeReady(raw)
	if err != nil {
		return false
	}
	return evt.Capabilities != nil && evt.Capabilities.ReviewCheckout != nil && *evt.Capabilities.ReviewCheckout
}

// genAwaitingReady reports whether row's live gen has sent no ready yet:
// it went suspect while still spawning or connecting -- a restore or a
// respawn slow to connect, its connecting deadline past, or one whose
// provider call failed -- and the dispatch evaluation sees it suspect,
// which it dispatches to. Every other live state follows a ready: a ready
// moves a connecting gen to booting. review_checkout_gen still names an
// earlier gen then, so the gen's capability is unknown rather than absent.
func genAwaitingReady(row sqlcgen.Sandbox) bool {
	if sandbox.State(row.Status) != sandbox.StateSuspect || row.PreSuspectStatus == nil {
		return false
	}
	switch *row.PreSuspectStatus {
	case sqlcgen.SandboxStatusSpawning, sqlcgen.SandboxStatusConnecting:
		return true
	default:
		return false
	}
}

// reviewCheckoutCapable reports whether row's live gen advertised the
// review-checkout capability in its latest ready.
func reviewCheckoutCapable(row sqlcgen.Sandbox) bool {
	return row.ReviewCheckoutGen != nil && *row.ReviewCheckoutGen == row.Gen
}

// checkoutResultEntry is one repo of a stored checkout_result, decoded
// leniently: the outcome is an open enum, so it is read as text, and a
// value DecideReviewCheckout does not know is read as failed there.
type checkoutResultEntry struct {
	Name    string  `json:"name"`
	Outcome string  `json:"outcome"`
	HeadSha *string `json:"headSha"`
	RefSha  *string `json:"refSha"`
	Error   *string `json:"error"`
}

// decodeCheckoutReply returns the entry for repoName of a stored
// checkout_result, nil when raw is empty -- no reply stored. A reply that
// does not decode, or names no entry for repoName, is a reply that failed,
// so a sandbox that answers nonsense is retried and refused at the bound,
// never read as holding the head.
func decodeCheckoutReply(raw []byte, repoName string) *turn.CheckoutReply {
	if len(raw) == 0 {
		return nil
	}
	var result struct {
		Repos []checkoutResultEntry `json:"repos"`
	}
	if err := json.Unmarshal(raw, &result); err != nil {
		return &turn.CheckoutReply{Outcome: turn.CheckoutFailed, Error: "the sandbox's checkout_result does not decode"}
	}
	for _, entry := range result.Repos {
		if entry.Name != repoName {
			continue
		}
		return &turn.CheckoutReply{
			Outcome: turn.CheckoutOutcome(entry.Outcome),
			HeadSHA: stringOrEmpty(entry.HeadSha),
			RefSHA:  stringOrEmpty(entry.RefSha),
			Error:   stringOrEmpty(entry.Error),
		}
	}
	return &turn.CheckoutReply{Outcome: turn.CheckoutFailed, Error: fmt.Sprintf("the sandbox's checkout_result names no repo %q", repoName)}
}

// checkoutPlan is a checkout command a committed evaluation recorded, for
// executeCheckout to send.
type checkoutPlan struct {
	gen       int32
	messageID string
	target    reviewCheckoutTarget
	payload   json.RawMessage
	// resend is true when the turn had a request on this gen already.
	resend bool
}

// BuildCheckoutPayload marshals the sandbox-ws checkout command for one
// repo: the pull request's ref and the commit the turn recorded.
func BuildCheckoutPayload(sessionID string, gen int32, messageID, repoName, ref, sha string) (json.RawMessage, error) {
	return json.Marshal(sandboxws.Checkout{
		Type:      "checkout",
		MessageId: messageID,
		SessionId: sessionID,
		Gen:       int(gen),
		Repos:     []sandboxws.CheckoutReposElem{{Name: repoName, Ref: ref, Sha: sha}},
	})
}

// reviewCheckoutDecision is what applyReviewCheckout tells its caller to
// do with the turn it was about to send.
type reviewCheckoutDecision int

const (
	// reviewCheckoutNotNeeded: the turn needs no checkout; send it as
	// before.
	reviewCheckoutNotNeeded reviewCheckoutDecision = iota + 1
	// reviewCheckoutConfirmed: the sandbox holds the turn's head; send it,
	// recording the commit in the dispatching commit.
	reviewCheckoutConfirmed
	// reviewCheckoutHold: send nothing of the turn this round. plan, when
	// set, carries a checkout command to send or a retired gen to stop.
	reviewCheckoutHold
	// reviewCheckoutEnded: the pending turn ended in this transaction --
	// refused, or context_moved -- and the next is evaluated at once.
	reviewCheckoutEnded
	// reviewCheckoutFailForward: the turn, processing and re-sent to a new
	// gen, is failed forward after the commit; plan carries the failure.
	reviewCheckoutFailForward
)

// reviewCheckoutResult is applyReviewCheckout's answer.
type reviewCheckoutResult struct {
	decision reviewCheckoutDecision
	// sha is the commit checked out, for reviewCheckoutConfirmed.
	sha string
	// plan is the dispatch plan reviewCheckoutHold or
	// reviewCheckoutFailForward hands executeDispatch, nil for a wait.
	plan *dispatchPlan
	// counted is the review_checkout_total outcome this decision counts
	// once its transaction commits, "" for none.
	counted string
}

// applyReviewCheckout decides, inside the dispatch evaluation's
// transaction, whether target -- the turn about to be sent to sandboxRow's
// live gen, pending, or processing and re-sent after a gen change -- waits
// on its checkout, and acts on turn.DecideReviewCheckout's verdict. See
// this file's top comment.
func (a *Actor) applyReviewCheckout(ctx context.Context, tx pgx.Tx, sessionRow sqlcgen.Session, sandboxRow sqlcgen.Sandbox, turns []sqlcgen.Turn, target sqlcgen.Turn, now time.Time) (reviewCheckoutResult, error) {
	notNeeded := reviewCheckoutResult{decision: reviewCheckoutNotNeeded}
	if sessionRow.SpawnSource != sqlcgen.SessionSpawnSourceGithub || target.ReviewHeadSha == nil || *target.ReviewHeadSha == "" || a.stores.githubPRSession == nil {
		return notNeeded, nil
	}
	claim, err := a.stores.githubPRSession.WithTx(tx).GetBySessionID(ctx, a.sessionID)
	if errors.Is(err, pgx.ErrNoRows) {
		return notNeeded, nil
	}
	if err != nil {
		return reviewCheckoutResult{}, fmt.Errorf("sessionactor: review checkout: read the session's pull request claim: %w", err)
	}
	checkoutTarget, ok, badSHA, err := reviewCheckoutTargetFor(&claim, sessionRow.Repos, target.ReviewHeadSha)
	if err != nil {
		return reviewCheckoutResult{}, fmt.Errorf("sessionactor: review checkout: %w", err)
	}
	if badSHA {
		a.logger.Warn("sessionactor: review checkout: the turn's recorded head is not a full commit id, so it is not checked out; dispatched as before",
			"turn_id", target.ID.String(), "review_head_sha", *target.ReviewHeadSha)
	}
	if !ok {
		return notNeeded, nil
	}

	state, err := a.stores.turn.WithTx(tx).CheckoutState(ctx, target.ID)
	if err != nil {
		return reviewCheckoutResult{}, fmt.Errorf("sessionactor: review checkout: read the turn's checkout state: %w", err)
	}
	pending := turn.State(target.Status) == turn.StatePending
	facts := reviewCheckoutFacts(sandboxRow, state, checkoutTarget, pending && turn.ContextCheckedAtDispatch(target.IsReviewAttempt, target.RequestTrigger))
	verdict := turn.DecideReviewCheckout(facts, a.reviewCheckoutBounds())

	switch verdict.Action {
	case turn.CheckoutProceed:
		if verdict.RefSHA != "" && verdict.RefSHA != checkoutTarget.sha {
			a.logger.Info("sessionactor: review checkout: the pull request's head moved, and this turn runs on the head it recorded, checked out",
				"turn_id", target.ID.String(), "head_sha", checkoutTarget.sha, "ref_sha", verdict.RefSHA)
		}
		return reviewCheckoutResult{decision: reviewCheckoutConfirmed, sha: checkoutTarget.sha}, nil

	case turn.CheckoutSend:
		plan, err := a.recordCheckoutSend(ctx, tx, sessionRow, sandboxRow, target, checkoutTarget, verdict, facts.RequestedOnGen)
		if err != nil {
			return reviewCheckoutResult{}, err
		}
		return reviewCheckoutResult{decision: reviewCheckoutHold, plan: plan}, nil

	case turn.CheckoutWait:
		if err := a.stores.timer.WithTx(tx).ArmDispatchAfter(ctx, a.sessionID, verdict.NextLook); err != nil {
			return reviewCheckoutResult{}, fmt.Errorf("sessionactor: review checkout: arm the dispatch timer: %w", err)
		}
		a.logger.Debug("sessionactor: review checkout: the turn waits on its sandbox's checkout",
			"turn_id", target.ID.String(), "gen", sandboxRow.Gen, "outcome", string(verdict.Outcome), "next_look", verdict.NextLook.String())
		return reviewCheckoutResult{decision: reviewCheckoutHold}, nil

	case turn.CheckoutEndMoved:
		check := &reviewContextCheck{
			turnID:      target.ID,
			outcome:     reviewContextMoved,
			reason:      checkoutMovedReason(checkoutTarget, verdict),
			liveHeadSHA: verdict.RefSHA,
		}
		if err := a.endContextMovedTurn(ctx, tx, turns, target, check, now); err != nil {
			return reviewCheckoutResult{}, err
		}
		return reviewCheckoutResult{decision: reviewCheckoutEnded, counted: reviewCheckoutOutcomeContextMoved}, nil

	case turn.CheckoutRefuse:
		refusal := a.checkoutRefusal(checkoutTarget, verdict)
		if !pending {
			a.logger.Warn("sessionactor: review checkout: a turn re-sent to a new gen is failed: its sandbox did not check out the head it recorded",
				"turn_id", target.ID.String(), "gen", sandboxRow.Gen, "refusal", string(verdict.Refusal), "reason", refusal.reason)
			return reviewCheckoutResult{
				decision: reviewCheckoutFailForward,
				plan: &dispatchPlan{turnID: target.ID, sessionRow: sessionRow, checkoutFailure: &dispatchFailure{
					reason: refusal.reason, warning: refusal.warning, notAssessed: refusal.notAssessed, refused: true,
				}},
				counted: refusal.counted,
			}, nil
		}
		if err := a.refuseReviewCheckout(ctx, tx, sessionRow, turns, target, refusal, int(sandboxRow.Gen), now); err != nil {
			return reviewCheckoutResult{}, err
		}
		return reviewCheckoutResult{decision: reviewCheckoutEnded, counted: refusal.counted}, nil

	case turn.CheckoutRetireGen:
		retired, err := a.retireGenForCheckout(ctx, tx, sandboxRow, target, verdict)
		if err != nil {
			return reviewCheckoutResult{}, err
		}
		counted := reviewCheckoutOutcomeRetiredOldAgent
		if verdict.Retirement == turn.CheckoutRetiredFailing {
			counted = reviewCheckoutOutcomeRetiredFailing
		}
		return reviewCheckoutResult{
			decision: reviewCheckoutHold,
			plan:     &dispatchPlan{turnID: target.ID, sessionRow: sessionRow, checkoutRetire: retired},
			counted:  counted,
		}, nil

	default:
		return reviewCheckoutResult{}, fmt.Errorf("sessionactor: review checkout: unhandled verdict %s", verdict.Action)
	}
}

// reviewCheckoutBounds are the platform.Timeouts turn.DecideReviewCheckout
// measures against.
func (a *Actor) reviewCheckoutBounds() turn.CheckoutBounds {
	return turn.CheckoutBounds{
		Timeout:              a.timeouts.ReviewCheckoutTimeout,
		RefLagWindow:         a.timeouts.ReviewCheckoutRefLagWindow,
		RefetchInterval:      a.timeouts.ReviewCheckoutRefetchInterval,
		FailuresBeforeRetire: a.timeouts.ReviewCheckoutFailuresBeforeRetire,
	}
}

// reviewCheckoutFacts reads, from rows the evaluation already holds and the
// turn's checkout state, the facts turn.DecideReviewCheckout decides on.
func reviewCheckoutFacts(sandboxRow sqlcgen.Sandbox, state sqlcgen.GetTurnCheckoutStateRow, target reviewCheckoutTarget, movedEndsTurn bool) turn.CheckoutFacts {
	requested := state.CheckoutMessageID != nil && state.CheckoutGen != nil && *state.CheckoutGen == sandboxRow.Gen
	facts := turn.CheckoutFacts{
		WantSHA:        target.sha,
		AwaitingReady:  genAwaitingReady(sandboxRow),
		GenCapable:     reviewCheckoutCapable(sandboxRow),
		HasSnapshot:    sandboxRow.SnapshotID != nil && *sandboxRow.SnapshotID != "",
		RequestedOnGen: requested,
		RetiredAGen:    state.CheckoutRetiredGen != nil,
		MovedEndsTurn:  movedEndsTurn,
	}
	if requested {
		facts.Reply = decodeCheckoutReply(state.Reply, target.repoName)
		facts.ReconnectedSinceSend = state.CheckoutSentReadySeq != nil && sandboxRow.ReadySeq > *state.CheckoutSentReadySeq
		facts.SinceRequest = time.Duration(state.SinceRequestNanos)
		facts.SinceSend = time.Duration(state.SinceSendNanos)
		facts.Sends = int(state.CheckoutSends)
		facts.Failures = int(state.CheckoutFailures)
	}
	return facts
}

// recordCheckoutSend records, in tx, a checkout command for target's live
// gen -- a new messageId, the gen's ready_seq -- arms the dispatch timer at
// the verdict's next look, and returns the plan executeCheckout sends
// after the commit. With no commander nothing could be sent, so nothing is
// recorded, as tryPlanDispatch leaves a turn pending without one.
func (a *Actor) recordCheckoutSend(ctx context.Context, tx pgx.Tx, sessionRow sqlcgen.Session, sandboxRow sqlcgen.Sandbox, target sqlcgen.Turn, checkoutTarget reviewCheckoutTarget, verdict turn.CheckoutVerdict, resend bool) (*dispatchPlan, error) {
	if a.commander == nil {
		a.logger.Warn("sessionactor: review checkout would be sent but no SandboxCommander is configured; skipping")
		return nil, nil
	}
	messageID := uuid.NewString()
	payload, err := BuildCheckoutPayload(a.sessionID.String(), sandboxRow.Gen, messageID, checkoutTarget.repoName, checkoutTarget.ref, checkoutTarget.sha)
	if err != nil {
		return nil, fmt.Errorf("sessionactor: review checkout: build the command: %w", err)
	}
	written, err := a.stores.turn.WithTx(tx).RecordCheckoutRequest(ctx, target.ID, sandboxRow.Gen, messageID, sandboxRow.ReadySeq, verdict.AfterFailure)
	if err != nil {
		return nil, fmt.Errorf("sessionactor: review checkout: record the request: %w", err)
	}
	if written == 0 {
		// Unreachable while the evaluation holds the session's lock: the
		// turn was read open in this transaction.
		return nil, nil
	}
	if err := a.stores.timer.WithTx(tx).ArmDispatchAfter(ctx, a.sessionID, verdict.NextLook); err != nil {
		return nil, fmt.Errorf("sessionactor: review checkout: arm the dispatch timer: %w", err)
	}
	return &dispatchPlan{
		turnID:     target.ID,
		sessionRow: sessionRow,
		checkout: &checkoutPlan{
			gen: sandboxRow.Gen, messageID: messageID, target: checkoutTarget, payload: payload, resend: resend,
		},
	}, nil
}

// executeCheckout writes a checkout command a committed evaluation
// recorded (recordCheckoutSend) to the sandbox's socket, outside any
// transaction, and returns. It never waits for the reply: that arrives as
// a sandbox event, whose own post-commit evaluation reads it. A failed
// write is logged and counted and changes nothing: the request stays
// recorded, and the sandbox's reconnect -- a ready counted since the send
// -- or the bound decides what follows.
func (a *Actor) executeCheckout(ctx context.Context, plan *dispatchPlan) error {
	c := plan.checkout
	if err := a.commander.SendCommand(a.sessionID.String(), c.payload); err != nil {
		a.logger.Warn("sessionactor: review checkout: send the checkout command failed; the request stays recorded, and a reconnect or the bound decides what follows",
			"turn_id", plan.turnID.String(), "gen", c.gen, "message_id", c.messageID, "error", err)
		a.recordReviewCheckout(ctx, reviewCheckoutOutcomeSendFailed)
		return nil
	}
	a.logger.Info("sessionactor: review checkout: asked the sandbox to check out the head the turn recorded; the turn waits for its report",
		"turn_id", plan.turnID.String(), "gen", c.gen, "message_id", c.messageID, "repo", c.target.repoName,
		"ref", c.target.ref, "sha", c.target.sha, "resend", c.resend)
	a.recordReviewCheckout(ctx, reviewCheckoutOutcomeSent)
	return nil
}

// checkoutMovedReason is the reason of an attempt ended context_moved at
// its checkout: the head the pull request's ref holds now, or that the
// recorded one is no longer in it.
func checkoutMovedReason(target reviewCheckoutTarget, verdict turn.CheckoutVerdict) string {
	if verdict.Outcome == turn.CheckoutSHAAbsent {
		tip := verdict.RefSHA
		if tip == "" {
			tip = "unknown"
		}
		return fmt.Sprintf("the pull request's head in the base repository no longer holds %s (%s is at %s)", target.sha, target.ref, tip)
	}
	return fmt.Sprintf("the pull request's head in the base repository is %s, not %s", verdict.RefSHA, target.sha)
}

// reviewCheckoutRefusalText is a refusal's texts: the reason the turn's
// synthetic end carries, the session warning, the review check's
// not-assessed reason, and the review_checkout_total outcome.
type reviewCheckoutRefusalText struct {
	reason      string
	warning     string
	notAssessed reviewcheck.NotAssessedReason
	counted     string
}

// checkoutRefusal composes the texts of verdict's refusal.
func (a *Actor) checkoutRefusal(target reviewCheckoutTarget, verdict turn.CheckoutVerdict) reviewCheckoutRefusalText {
	var t reviewCheckoutRefusalText
	switch verdict.Refusal {
	case turn.CheckoutRefusedUnsupported:
		t.reason = "the sandbox's agent cannot check out the commit this review was asked about"
		t.warning = "This review was not run: the sandbox's agent cannot check out the commit this review was asked about. " +
			"Rebuild the sandbox image so it carries a current agent, then request the review again."
		t.notAssessed, t.counted = reviewcheck.NotAssessedReviewCheckoutUnsupported, reviewCheckoutOutcomeUnsupported
	case turn.CheckoutRefusedNoReport:
		t.reason = fmt.Sprintf("the sandbox did not report its checkout of %s within %s", target.sha, a.timeouts.ReviewCheckoutTimeout)
		t.warning = "This review was not run: " + t.reason + ". Request the review again once the sandbox answers."
		t.notAssessed, t.counted = reviewcheck.NotAssessedReviewCheckoutFailed, reviewCheckoutOutcomeNoReport
	case turn.CheckoutRefusedHeadAbsent:
		tip := verdict.RefSHA
		if tip == "" {
			tip = "unknown"
		}
		t.reason = fmt.Sprintf("the commit %s this turn was asked about is not in the pull request's %s, whose tip is %s", target.sha, target.ref, tip)
		t.warning = "This review was not run: " + t.reason + ". Ask again for the pull request's head as it is now."
		t.notAssessed, t.counted = reviewcheck.NotAssessedReviewCheckoutFailed, reviewCheckoutOutcomeHeadAbsent
	default:
		errText := verdict.Error
		if errText == "" {
			errText = "no error was named"
		}
		t.reason = fmt.Sprintf("the sandbox did not check out %s from %s of %s within %s: %s: %s", target.sha, target.ref, target.repoFullName, a.timeouts.ReviewCheckoutTimeout, verdict.Outcome, errText)
		t.warning = "This review was not run: " + t.reason + "."
		switch {
		case verdict.Outcome == turn.CheckoutFetchFailed && fetchRefusedAccess(verdict.Error):
			t.warning += fmt.Sprintf(" %s refused the read: the pull request's ref is read with the GitHub App's installation token, so "+
				"install the App on %s, with read access to its contents, then request the review again.", target.repoFullName, target.repoFullName)
		case verdict.Outcome == turn.CheckoutFetchFailed:
			t.warning += fmt.Sprintf(" The sandbox could not fetch the pull request's ref from %s for the reason above; "+
				"request the review again once it can.", target.repoFullName)
		}
		t.notAssessed, t.counted = reviewcheck.NotAssessedReviewCheckoutFailed, reviewCheckoutOutcomeError
	}
	return t
}

// fetchAccessRefusals are the phrases git prints when a remote refuses a
// read for want of access -- no credential, a credential it does not
// accept, or a repository it will not show -- as opposed to a host it
// cannot reach, a fetch that timed out or a ref that is not there.
var fetchAccessRefusals = []string{
	"authentication failed",
	"could not read username",
	"could not read password",
	"invalid username or password",
	"terminal prompts disabled",
	"repository not found",
	"permission denied",
	"access denied",
	"returned error: 401",
	"returned error: 403",
}

// fetchRefusedAccess reports whether errText, a fetch_failed reply's error,
// says the base repository refused the read for want of access: only then
// is installing the App the remedy.
func fetchRefusedAccess(errText string) bool {
	lower := strings.ToLower(errText)
	for _, phrase := range fetchAccessRefusals {
		if strings.Contains(lower, phrase) {
			return true
		}
	}
	return false
}

// refuseReviewCheckout ends target, still pending, as a turn refused
// before it started, in planDispatch's transaction: the machine's abandon
// edge (never_started), the workflow engine told through OnTurnRefused --
// read as a blocked outcome, it could queue the same step again -- a
// synthetic execution_complete from pending, marked "delivered": false
// since no prompt was sent, a session warning naming the cause, a review
// attempt's check closed not assessed with refusal.notAssessed, and the
// session's status re-derived. The dispatch timer is re-armed due at once,
// the trigger of the rest of the queue, which planDispatch evaluates again
// at once. refusePersonalLinkOnly's shape (credentialgate.go).
func (a *Actor) refuseReviewCheckout(ctx context.Context, tx pgx.Tx, sessionRow sqlcgen.Session, turns []sqlcgen.Turn, target sqlcgen.Turn, refusal reviewCheckoutRefusalText, gen int, now time.Time) error {
	from := turn.State(target.Status)
	to, err := turn.Transition(from, turn.TriggerAbandon)
	if err != nil {
		return fmt.Errorf("sessionactor: refuse turn %s at its checkout: %w", target.ID.String(), err)
	}
	if _, err := a.turnWrites(tx).UpdateStatus(ctx, sqlcgen.UpdateTurnStatusParams{
		ID:          target.ID,
		Status:      sqlcgen.TurnStatus(to),
		CompletedAt: pgtype.Timestamptz{Time: now, Valid: true},
	}); err != nil {
		return fmt.Errorf("sessionactor: update turn status: %w", err)
	}
	workflowengine.OnTurnRefused(ctx, a.workflowDeps(tx), sessionRow, target.ID, refusal.reason)
	if turn.RequiresSyntheticExecutionComplete(turn.TriggerAbandon) {
		payload := syntheticExecutionComplete(target.ID, from, refusal.reason)
		payload[syntheticUndeliveredKey] = false
		if err := a.appendEvent(ctx, tx, "execution_complete", payload); err != nil {
			return err
		}
	}
	if err := a.recordSessionWarning(ctx, tx, gen, refusal.warning); err != nil {
		return err
	}
	failureReason, _ := turn.DeriveFailureReason(from, turn.TriggerAbandon)
	if err := a.enqueueOutboxNotification(ctx, tx, sessionRow, turn.TriggerAbandon, failureReason, target, nil, refusal.notAssessed); err != nil {
		return err
	}
	if err := a.persistDerivedSessionStatus(ctx, tx, summariesWithOverride(turns, target.ID, to, failureReason)); err != nil {
		return err
	}
	a.logger.Warn("sessionactor: review checkout: the turn is refused: its sandbox did not check out the head it recorded",
		"turn_id", target.ID.String(), "gen", gen, "reason", refusal.reason)
	return a.armDispatchNow(ctx, tx)
}

// retireGenForCheckout retires sandboxRow's live gen and clears the
// sandbox's snapshot in tx, so the next gen boots fresh: one whose agent
// cannot check out a commit, which a restore of the snapshot may have
// brought back, or one whose checkouts of target kept failing on what its
// worktree -- and the snapshot taken after the last turn -- holds, which
// target records (turns.checkout_retired_gen) so it retires no other. The
// turn stays as it is, and the dispatch timer is re-armed due at once;
// after the commit the provider object is stopped
// (stopSandboxOfRetiredGen) and the next evaluation spawns the new gen.
// A fresh gen that is old too takes no snapshot, since no turn runs on it,
// so an old agent costs at most one respawn before its turn is refused.
func (a *Actor) retireGenForCheckout(ctx context.Context, tx pgx.Tx, sandboxRow sqlcgen.Sandbox, target sqlcgen.Turn, verdict turn.CheckoutVerdict) (*retiredGen, error) {
	if _, err := a.stores.sandbox.WithTx(tx).ClearSnapshot(ctx, a.sessionID); err != nil {
		return nil, fmt.Errorf("sessionactor: review checkout: clear the sandbox's snapshot: %w", err)
	}
	retired, err := a.retireLiveGen(ctx, tx, sandboxRow)
	if err != nil {
		return nil, err
	}
	if verdict.Retirement == turn.CheckoutRetiredFailing {
		if _, err := a.stores.turn.WithTx(tx).SetCheckoutRetiredGen(ctx, target.ID, sandboxRow.Gen); err != nil {
			return nil, fmt.Errorf("sessionactor: review checkout: record the gen the turn retired: %w", err)
		}
	}
	if err := a.armDispatchNow(ctx, tx); err != nil {
		return nil, err
	}
	snapshotID := ""
	if sandboxRow.SnapshotID != nil {
		snapshotID = *sandboxRow.SnapshotID
	}
	a.logger.Warn("sessionactor: review checkout: the sandbox gen is retired and its snapshot cleared, so the next gen boots fresh",
		"turn_id", target.ID.String(), "gen", sandboxRow.Gen, "retirement", string(verdict.Retirement),
		"snapshot_id", snapshotID, "outcome", string(verdict.Outcome), "error", verdict.Error, "provider_id", retired.providerID)
	return retired, nil
}

// reviewCheckoutOutstanding is §24.9's pre-read asking whether row's turn
// waits on its checkout, outside any transaction: the turn needs one
// (reviewCheckoutTargetFor), and turn.DecideReviewCheckout, over the same
// rows and with the same bounds applyReviewCheckout reads in the
// evaluation that follows, does not let it proceed -- it waits, sends,
// ends the attempt context_moved, retires the gen or refuses the turn, all
// before the context check is reached. The pre-read then reads nothing of
// the code host for it. A turn the decision lets proceed is read, so a
// turn the gate sends is never one the pre-read skipped, which would hold
// it for a read never made. A read that fails answers false -- the code
// host is read, which costs one read and never holds a turn. row is a
// pending pick the context check applies to, so a moved head ends it.
func (a *Actor) reviewCheckoutOutstanding(ctx context.Context, row sqlcgen.GetReviewAttemptToCheckRow) bool {
	if row.ReviewHeadSha == nil || *row.ReviewHeadSha == "" || a.stores.githubPRSession == nil {
		return false
	}
	claim, err := a.stores.githubPRSession.GetBySessionID(ctx, a.sessionID)
	if err != nil {
		return false
	}
	sessionRow, err := a.stores.session.Get(ctx, a.sessionID)
	if err != nil {
		return false
	}
	target, ok, _, err := reviewCheckoutTargetFor(&claim, sessionRow.Repos, row.ReviewHeadSha)
	if err != nil || !ok {
		return false
	}
	sandboxRow, err := a.stores.sandbox.Get(ctx, a.sessionID)
	if err != nil {
		return false
	}
	state, err := a.stores.turn.CheckoutState(ctx, row.ID)
	if err != nil {
		return false
	}
	facts := reviewCheckoutFacts(sandboxRow, state, target, turn.ContextCheckedAtDispatch(row.IsReviewAttempt, row.RequestTrigger))
	return turn.DecideReviewCheckout(facts, a.reviewCheckoutBounds()).Action != turn.CheckoutProceed
}
