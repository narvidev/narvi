// This file (owedreviewrequest.go) implements the consumer of technical
// plan §24.9's owed human review requests: the owed_review_request timer's
// handler.
//
// # Where a request becomes owed
//
// A person's review request through the configured GitHub label or the
// web Re-run review button (turns.request_trigger 'label', 'button')
// queues its review attempt at once, behind whatever turn of the session is
// open. (A mention never does: on a session that exists it is no review
// attempt, and on a new one it is the session's first turn, which waits
// behind nothing.) When that attempt is dispatched after waiting, the
// context check (reviewcontextcheck.go) compares the head, base and
// ancestor chain it recorded with the pull request's live ones; one that
// moved does not start. It ends context_moved, and its request is owed to
// its requester (oweReviewRequest): a row of owed_review_requests, in the
// dispatching transaction, with the owed_review_request timer armed due at
// once. Both are rows, so a restart between the move and the re-run loses
// nothing: whichever replica's pump claims the timer runs this.
//
// # What the consumer does
//
// It serves the session's oldest owed request, in three phases, the way
// the automatic re-review does (reviewretrigger.go's top comment): no
// network read ever holds a transaction open.
//
//  1. One transaction reads the request (readOwedReviewRequest), after
//     dropping every request a person's standing stop predates
//     (dropOwedReviewRequestsForStop). A request past
//     ReviewContextMoveMaxConsecutive moves in a row is dropped there, and
//     its requester told once (dropOwedReviewRequest).
//  2. With no transaction open, it asks whether the requester may still
//     have it run (Actor.reviewRequestAuthorizer, a port the control plane
//     wires to the HTTP layer's own checks, which this package cannot
//     import) and, when they may, reads the pull request again
//     (fetchAutoRetriggerReviewContext) and composes the prompt for the
//     head it has now from the request's own text (composeReviewTurn, the
//     composition the automatic lane shares).
//  3. One transaction deletes the request and inserts its re-run turn on the
//     person's path (insertOwedReviewRequestTurn): the workflow step the
//     person's own request resolves to shapes it -- its prompt template,
//     model and effort -- as createTurnLocked's does, and never the
//     automatic lane's opt-in, hold, budget or "already reviewed"
//     comparison, which a person's request is exempt from. Or, for a
//     requester no longer authorized -- or no longer known, their account
//     deleted -- it drops it and tells them once. The timer is re-armed due
//     at once while more is owed, deleted otherwise.
//
// A pull request that cannot be read, or an authorization that cannot be
// evaluated, keeps the request: the timer backs off on the dispatch
// timer's schedule (DispatchRetryBackoff to DispatchRetryBackoffMax) and
// the session reads scheduled meanwhile. A failure is never read as a
// verdict.
//
// # The hold, and the workflow attempt
//
// While a request is owed the automatic lane holds (ReviewRetriggerHeld's
// owed term): the person's re-run comes first. The re-run turn then holds
// the lane as an open turn; a drop releases it, waking a held debounce in
// the drop's transaction (wakeHeldReviewRetrigger). A moved attempt the
// workflow engine tracked keeps its step attempt live while the request is
// owed: the re-run turn takes it over (handOverOwedRequestWorkflow), and a
// drop ends its run cancelled, the way a person's stop does
// (workflowengine.OnTurnWithdrawn): a drop is a policy decision, not the
// step's outcome, so no edge is followed and no second notice posted.

package sessionactor

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/narvidev/narvi/internal/adapters/outbound/githubapi"
	"github.com/narvidev/narvi/internal/adapters/outbound/postgres/sqlcgen"
	"github.com/narvidev/narvi/internal/app/auditlog"
	"github.com/narvidev/narvi/internal/app/ports"
	"github.com/narvidev/narvi/internal/app/workflowengine"
	"github.com/narvidev/narvi/internal/domain/knowledge"
	"github.com/narvidev/narvi/internal/domain/reposource"
	"github.com/narvidev/narvi/internal/domain/reviewpost"
	"github.com/narvidev/narvi/internal/domain/reviewverdict"
	"github.com/narvidev/narvi/internal/domain/turn"
	"github.com/narvidev/narvi/internal/platform"
)

// owedReviewRequestFallbackText is the base text of a re-run whose request
// recorded none: every human lane records its own (turns.request_text), so
// it stands only for a row written without one.
const owedReviewRequestFallbackText = "Review requested on this pull request."

// owedRequestDrop is why an owed request is dropped rather than re-run.
type owedRequestDrop int

const (
	// owedRequestDropUnauthorized: the requester may no longer have it run.
	owedRequestDropUnauthorized owedRequestDrop = iota + 1
	// owedRequestDropBound: the request met a moved context more times in
	// a row than ReviewContextMoveMaxConsecutive allows.
	owedRequestDropBound
)

func (d owedRequestDrop) String() string {
	switch d {
	case owedRequestDropUnauthorized:
		return "unauthorized"
	case owedRequestDropBound:
		return "context_moved_bound"
	default:
		return fmt.Sprintf("owedRequestDrop(%d)", int(d))
	}
}

// owedRequest is the request phase 1 read, with its pull request.
type owedRequest struct {
	row          sqlcgen.OwedReviewRequest
	repoFullName string
	prNumber     int32
}

// handleOwedReviewRequestTimer implements the TimerOwedReviewRequest named
// timer (technical plan §24.9). See this file's own top comment for the
// three phases.
func (a *Actor) handleOwedReviewRequestTimer(ctx context.Context) error {
	owed, err := a.readOwedReviewRequest(ctx)
	if err != nil || owed == nil {
		return err
	}

	// The requester's authorization first: a request that is dropped costs
	// no read of the code host, and composing its prompt never runs the
	// false-positive fetch's hit count for a turn that will not exist.
	allowed, err := a.authorizeOwedReviewRequest(ctx, owed.row, owed.repoFullName)
	if err != nil {
		a.logger.Warn("sessionactor: owed_review_request: the requester's authorization could not be evaluated; the request is kept and asked again later",
			"owed_id", owed.row.ID.String(), "error", err)
		return a.backOffOwedReviewRequest(ctx, owed.row)
	}
	var composed composedReviewTurn
	if allowed {
		reviewCtx := a.fetchAutoRetriggerReviewContext(ctx, owed.repoFullName, owed.prNumber)
		if reviewCtx.HeadSHA == "" {
			a.logger.Warn("sessionactor: owed_review_request: could not read the pull request's live head; the request is kept and asked again later",
				"owed_id", owed.row.ID.String(), "repo_full_name", owed.repoFullName, "pr_number", owed.prNumber)
			return a.backOffOwedReviewRequest(ctx, owed.row)
		}
		baseText := owedReviewRequestFallbackText
		if owed.row.RequestText != nil && *owed.row.RequestText != "" {
			baseText = *owed.row.RequestText
		}
		composed = a.composeReviewTurn(ctx, owed.repoFullName, owed.prNumber, reviewCtx, baseText)
	}

	var inserted bool
	err = a.transact(ctx, func(ctx context.Context, tx pgx.Tx) error {
		inserted = false
		served, err := a.stores.owedReviewRequest.WithTx(tx).Delete(ctx, owed.row.ID)
		if errors.Is(err, pgx.ErrNoRows) {
			// A person's stop dropped it while this read the pull request.
			a.logger.Info("sessionactor: owed_review_request: the request is no longer owed", "owed_id", owed.row.ID.String())
			return a.rearmOrDeleteOwedReviewRequestTimer(ctx, tx)
		}
		if err != nil {
			return fmt.Errorf("sessionactor: delete the owed review request: %w", err)
		}
		sessionRow, err := a.stores.session.WithTx(tx).Get(ctx, a.sessionID)
		if err != nil {
			return fmt.Errorf("sessionactor: get session: %w", err)
		}
		if stopPredates(sessionRow, served) {
			// A stop requested while this read the pull request: the
			// request is dropped as the stop timer would drop it.
			a.withdrawOwedRequestWorkflow(ctx, tx, served.MovedTurnID)
			if err := a.wakeHeldReviewRetrigger(ctx, tx, "a stop dropped an owed review request"); err != nil {
				return err
			}
			a.logger.Info("sessionactor: owed_review_request: dropped by a person's stop", "owed_id", served.ID.String())
			return a.rearmOrDeleteOwedReviewRequestTimer(ctx, tx)
		}
		if !allowed {
			if err := a.dropOwedReviewRequest(ctx, tx, served, owed.repoFullName, owed.prNumber, owedRequestDropUnauthorized); err != nil {
				return err
			}
			return a.rearmOrDeleteOwedReviewRequestTimer(ctx, tx)
		}
		created, err := a.insertOwedReviewRequestTurn(ctx, tx, sessionRow, served, composed, owed.repoFullName, owed.prNumber)
		if err != nil {
			return err
		}
		a.logger.Info("sessionactor: owed_review_request: a person's review request re-run for the head the pull request has now",
			"owed_id", served.ID.String(), "turn_id", created.ID.String(), "request_trigger", served.Trigger,
			"head_sha", composed.reviewCtx.HeadSHA, "moves", served.ContextMoves)
		inserted = true
		return a.rearmOrDeleteOwedReviewRequestTimer(ctx, tx)
	})
	if err != nil {
		return err
	}
	if inserted {
		if dispatchErr := a.handleEnsureDispatched(ctx); dispatchErr != nil {
			a.logger.Warn("sessionactor: ensure-dispatched after an owed review request's re-run failed", "error", dispatchErr)
		}
	}
	return nil
}

// readOwedReviewRequest is the consumer's first phase, one transaction:
// the requests a person's standing stop predates dropped
// (dropOwedReviewRequestsForStop), then the session's oldest owed request
// with its pull request. nil, with the timer deleted, when nothing is
// owed. A request whose session claims no pull request has nothing to be
// re-run against and is dropped silently; one past
// ReviewContextMoveMaxConsecutive moves in a row is dropped and its
// requester told once. Either way nil is returned, and the timer re-armed
// due at once when more is owed, so the next is served at the next pump
// tick.
func (a *Actor) readOwedReviewRequest(ctx context.Context) (*owedRequest, error) {
	var owed *owedRequest
	err := a.transact(ctx, func(ctx context.Context, tx pgx.Tx) error {
		owed = nil
		sessionRow, err := a.stores.session.WithTx(tx).Get(ctx, a.sessionID)
		if err != nil {
			return fmt.Errorf("sessionactor: get session: %w", err)
		}
		if err := a.dropOwedReviewRequestsForStop(ctx, tx, sessionRow); err != nil {
			return err
		}
		requests := a.stores.owedReviewRequest.WithTx(tx)
		row, err := requests.Oldest(ctx, a.sessionID)
		if errors.Is(err, pgx.ErrNoRows) {
			return a.deleteTimer(ctx, tx, TimerOwedReviewRequest)
		}
		if err != nil {
			return fmt.Errorf("sessionactor: read the oldest owed review request: %w", err)
		}
		prSession, err := a.stores.githubPRSession.WithTx(tx).GetBySessionID(ctx, a.sessionID)
		if errors.Is(err, pgx.ErrNoRows) {
			if _, err := requests.Delete(ctx, row.ID); err != nil {
				return fmt.Errorf("sessionactor: delete the owed review request: %w", err)
			}
			a.withdrawOwedRequestWorkflow(ctx, tx, row.MovedTurnID)
			if err := a.wakeHeldReviewRetrigger(ctx, tx, "an owed review request with no pull request was dropped"); err != nil {
				return err
			}
			a.logger.Warn("sessionactor: owed_review_request: the session claims no pull request; the request has nothing to be re-run against and is dropped",
				"owed_id", row.ID.String())
			return a.rearmOrDeleteOwedReviewRequestTimer(ctx, tx)
		}
		if err != nil {
			return fmt.Errorf("sessionactor: get github pr session: %w", err)
		}
		if int(row.ContextMoves) > a.timeouts.ReviewContextMoveMaxConsecutive {
			if _, err := requests.Delete(ctx, row.ID); err != nil {
				return fmt.Errorf("sessionactor: delete the owed review request: %w", err)
			}
			if err := a.dropOwedReviewRequest(ctx, tx, row, prSession.RepoFullName, prSession.PrNumber, owedRequestDropBound); err != nil {
				return err
			}
			return a.rearmOrDeleteOwedReviewRequestTimer(ctx, tx)
		}
		owed = &owedRequest{row: row, repoFullName: prSession.RepoFullName, prNumber: prSession.PrNumber}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return owed, nil
}

// stopPredates reports whether the session's standing stop request was
// made at or after owed was owed: dropOwedReviewRequestsForStop's rule.
func stopPredates(sessionRow sqlcgen.Session, owed sqlcgen.OwedReviewRequest) bool {
	return sessionRow.StopRequestedAt.Valid && !owed.CreatedAt.Time.After(sessionRow.StopRequestedAt.Time)
}

// authorizeOwedReviewRequest asks the registry's ReviewRequestAuthorizer
// whether owed's requester may still have it run on repoFullName's pull
// request. A request whose requester is no longer known -- their account
// deleted, before the request was owed or since -- is unauthorized without
// asking: no one is left to ask for. With no authorizer wired, no answer
// can be had either. Either way the request is dropped and its requester
// told, never re-run unchecked.
func (a *Actor) authorizeOwedReviewRequest(ctx context.Context, owed sqlcgen.OwedReviewRequest, repoFullName string) (bool, error) {
	if !owed.RequestedBy.Valid {
		a.logger.Info("sessionactor: owed_review_request: the requester's account no longer exists; the request is unauthorized", "owed_id", owed.ID.String())
		return false, nil
	}
	if a.reviewRequestAuthorizer == nil {
		a.logger.Warn("sessionactor: owed_review_request: no review request authorizer is configured; the request cannot be authorized again", "owed_id", owed.ID.String())
		return false, nil
	}
	return a.reviewRequestAuthorizer.AuthorizeReviewRequest(ctx, ports.ReviewRequest{
		SessionID:    a.sessionID.String(),
		RequestedBy:  owed.RequestedBy.String(),
		Trigger:      owed.Trigger,
		RepoFullName: repoFullName,
	})
}

// backOffOwedReviewRequest keeps owed, whose pull request could not be
// read or whose authorization could not be evaluated, and backs the timer
// off in a transaction of its own: fires_at to the database's now plus the
// request's age, held between DispatchRetryBackoff and
// DispatchRetryBackoffMax (TimerStore.BackOffOwedReviewRequest), so a
// lasting failure is retried at a doubling delay, never at the claim
// cadence, and the actor can idle out between two tries.
func (a *Actor) backOffOwedReviewRequest(ctx context.Context, owed sqlcgen.OwedReviewRequest) error {
	return a.transact(ctx, func(ctx context.Context, tx pgx.Tx) error {
		if _, err := a.stores.timer.WithTx(tx).BackOffOwedReviewRequest(ctx, a.sessionID, owed.CreatedAt, a.timeouts.DispatchRetryBackoff, a.timeouts.DispatchRetryBackoffMax); err != nil {
			return fmt.Errorf("sessionactor: back off the owed review request timer: %w", err)
		}
		return nil
	})
}

// rearmOrDeleteOwedReviewRequestTimer is the consumer's last write in
// every transaction that served a request: the owed_review_request timer
// re-armed due at once while the session is still owed one, deleted
// otherwise -- the re-arm-or-delete contract of every named timer.
func (a *Actor) rearmOrDeleteOwedReviewRequestTimer(ctx context.Context, tx pgx.Tx) error {
	left, err := a.stores.owedReviewRequest.WithTx(tx).Exists(ctx, a.sessionID)
	if err != nil {
		return fmt.Errorf("sessionactor: read whether a review request is still owed: %w", err)
	}
	if !left {
		return a.deleteTimer(ctx, tx, TimerOwedReviewRequest)
	}
	if err := a.stores.timer.WithTx(tx).ArmOwedReviewRequest(ctx, a.sessionID); err != nil {
		return fmt.Errorf("sessionactor: arm the owed review request timer: %w", err)
	}
	return nil
}

// insertOwedReviewRequestTurn inserts owed's re-run in tx, on the person's
// path: the workflow step resolution createTurnLocked applies (the step's
// prompt template, model and effort over the composed prompt and the
// triage's model and effort), the same store-level insert every turn takes
// (TurnStore.CreateAndArmDispatch, which arms the dispatch timer), the
// step attempt -- the moved turn's, handed over, or a new run's -- and the
// audit row createTurnLocked writes, attributed to the requester; and
// nothing of the automatic lane's: no opt-in, hold, budget or "already
// reviewed" check, which a person's request is exempt from, and no budget
// slot spent. The turn carries the request on -- its trigger, requester,
// text and count of moves in a row, so a move it meets in turn is counted
// from there -- and the head, context, depth and prompt composed for the
// head the pull request has now.
func (a *Actor) insertOwedReviewRequestTurn(ctx context.Context, tx pgx.Tx, sessionRow sqlcgen.Session, owed sqlcgen.OwedReviewRequest, composed composedReviewTurn, repoFullName string, prNumber int32) (sqlcgen.Turn, error) {
	reviewCtx := composed.reviewCtx
	verdictContextJSON, err := json.Marshal(reviewverdict.Context{
		BaseRef:       reviewCtx.BaseRef,
		BaseSHA:       reviewCtx.BaseSHA,
		AncestorChain: reviewCtx.AncestorChain,
		PolicyVersion: reviewCtx.PolicyVersion,
	})
	if err != nil {
		a.logger.Warn("sessionactor: owed_review_request: marshal review verdict context failed, the re-run will carry review_head_sha but no review_verdict_context",
			"error", err, "repo_full_name", repoFullName, "pr_number", prNumber)
		verdictContextJSON = nil
	}
	var reviewDepth *string
	if composed.reviewDepth != "" {
		reviewDepth = &composed.reviewDepth
	}
	knowledgeMode := knowledge.ModeA
	var correlationID *string
	if id, ok := platform.CorrelationIDFromContext(ctx); ok && id != "" {
		correlationID = &id
	}
	// The workflow step the person's request resolves to, as
	// createTurnLocked resolves it (workflowengine.ResolveStepForNewTurn):
	// its prompt template, model and effort shape the re-run. While the
	// moved attempt's own step attempt is live, its run is running and that
	// same step resolves; with none running, a new run starts and tracks
	// the re-run, as it would a person's fresh request.
	workflows := a.stores.workflow.WithTx(tx)
	resolution := workflowengine.ResolveStepForNewTurn(ctx, workflows, sessionRow, composed.prompt, composed.modelID, composed.effort)
	trigger := owed.Trigger
	headSHA := reviewCtx.HeadSHA
	prompt := resolution.Prompt
	moves := owed.ContextMoves
	created, err := a.stores.turn.WithTx(tx).CreateAndArmDispatch(ctx, sqlcgen.CreateTurnParams{
		SessionID:               a.sessionID,
		Status:                  sqlcgen.TurnStatusPending,
		Prompt:                  &prompt,
		ModelID:                 resolution.ModelID,
		Effort:                  resolution.Effort,
		PlanMode:                false,
		ReviewHeadSha:           &headSHA,
		ReviewDepth:             reviewDepth,
		ReviewDepthDecision:     composed.reviewDepthDecisionJSON,
		ReviewKnowledgeMode:     &knowledgeMode,
		ReviewKnowledgeDecision: composed.knowledgeDecisionJSON,
		ReviewVerdictContext:    verdictContextJSON,
		IsReviewAttempt:         owed.IsReviewAttempt,
		CorrelationID:           correlationID,
		RequestTrigger:          &trigger,
		RequestedBy:             owed.RequestedBy,
		RequestText:             owed.RequestText,
		ContextMoves:            &moves,
	})
	if err != nil {
		return sqlcgen.Turn{}, fmt.Errorf("sessionactor: insert the owed review request's re-run: %w", err)
	}
	if resolution.Tracked {
		workflowengine.AttachTurn(ctx, workflows, resolution, created.ID)
	} else {
		a.handOverOwedRequestWorkflow(ctx, tx, owed.MovedTurnID, created.ID)
	}
	if err := auditlog.Record(ctx, a.stores.auditLog.WithTx(tx), owed.RequestedBy, "turn.create", "turn", created.ID.String(), map[string]any{
		"session_id":      a.sessionID.String(),
		"trigger":         "owed_review_request",
		"request_trigger": owed.Trigger,
		"moved_turn_id":   owed.MovedTurnID.String(),
		"head_sha":        headSHA,
		"repo":            repoFullName,
		"pr_number":       prNumber,
	}); err != nil {
		return sqlcgen.Turn{}, fmt.Errorf("sessionactor: record turn.create audit log: %w", err)
	}
	return created, nil
}

// owedRequestLane names a request lane in the drop notice.
func owedRequestLane(trigger string) string {
	switch trigger {
	case turn.RequestTriggerButton:
		return "the Re-run review button"
	case turn.RequestTriggerLabel:
		return "the re-review label"
	default:
		return "a person"
	}
}

// owedRequestDropMessage is the one sentence a dropped request's
// requester is told, on the pull request and as a session warning.
func owedRequestDropMessage(trigger string, why owedRequestDrop, moves int32) string {
	lane := owedRequestLane(trigger)
	if why == owedRequestDropBound {
		return fmt.Sprintf("A review requested through %s was not run: the pull request changed while it waited, %d times in a row, so it was not run again for the new head.", lane, moves)
	}
	return fmt.Sprintf("A review requested through %s was not run: the pull request changed while it waited, and the person who asked can no longer request reviews here, so it was not run again for the new head.", lane)
}

// dropOwedReviewRequest drops owed, already deleted in tx, and tells its
// requester once (technical plan §24.9): one notice on the pull request
// through the verdict outbox -- the sanctioned way a review session posts
// anything to a pull request (§5.2), like the budget notice
// (enqueueAutoRetriggerBudgetExhaustedNotice), with no risk level so no
// label moves -- and one session warning. The moved turn's workflow run,
// when one tracked it, ends cancelled with no edge followed and no notice
// of its own (withdrawOwedRequestWorkflow), and the hold's owed term is
// released (wakeHeldReviewRetrigger), all in tx: the row's delete and the
// notice commit together, so a drop is told exactly once.
func (a *Actor) dropOwedReviewRequest(ctx context.Context, tx pgx.Tx, owed sqlcgen.OwedReviewRequest, repoFullName string, prNumber int32, why owedRequestDrop) error {
	message := owedRequestDropMessage(owed.Trigger, why, owed.ContextMoves)
	if owner, repo, ok := reposource.SplitFullName(repoFullName); ok {
		payload, err := json.Marshal(githubapi.VerdictPayload{
			Owner:    owner,
			Repo:     repo,
			PRNumber: int(prNumber),
			Event:    string(reviewpost.FormalReviewEventComment),
			Body:     message + "\n\n" + reviewpost.RerunGuidance(a.githubBotHandle),
		})
		if err != nil {
			return fmt.Errorf("sessionactor: marshal the dropped review request's notice: %w", err)
		}
		if _, err := a.stores.outbox.WithTx(tx).Create(ctx, sqlcgen.CreateOutboxEntryParams{
			SessionID: a.sessionID,
			Kind:      string(ports.NotificationKindGitHubVerdict),
			Payload:   payload,
		}); err != nil {
			return fmt.Errorf("sessionactor: enqueue the dropped review request's notice: %w", err)
		}
	} else {
		a.logger.Warn("sessionactor: owed_review_request: repo_full_name not in owner/repo shape, no notice posted on the pull request",
			"repo_full_name", repoFullName)
	}
	gen := 0
	if sandboxRow, err := a.stores.sandbox.WithTx(tx).Get(ctx, a.sessionID); err == nil {
		gen = int(sandboxRow.Gen)
	} else if !errors.Is(err, pgx.ErrNoRows) {
		return fmt.Errorf("sessionactor: get sandbox: %w", err)
	}
	if err := a.recordSessionWarning(ctx, tx, gen, message); err != nil {
		return err
	}
	a.withdrawOwedRequestWorkflow(ctx, tx, owed.MovedTurnID)
	if err := a.wakeHeldReviewRetrigger(ctx, tx, "an owed review request was dropped"); err != nil {
		return err
	}
	a.logger.Warn("sessionactor: owed_review_request: a person's review request dropped and its requester told",
		"owed_id", owed.ID.String(), "moved_turn_id", owed.MovedTurnID.String(), "request_trigger", owed.Trigger,
		"reason", why.String(), "moves", owed.ContextMoves, "max", a.timeouts.ReviewContextMoveMaxConsecutive)
	return nil
}

// handOverOwedRequestWorkflow gives the workflow attempt the moved turn
// held, when the workflow engine tracked it, to its re-run: the attempt
// stayed live while the request was owed (endContextMovedTurn runs no
// workflow hook), and its run goes on with the turn that runs the request.
// Bookkeeping, fail open like the engine's own (workflowengine's doc.go):
// a failure is logged and never undoes the re-run.
func (a *Actor) handOverOwedRequestWorkflow(ctx context.Context, tx pgx.Tx, movedTurnID, rerunTurnID pgtype.UUID) {
	workflows := a.stores.workflow.WithTx(tx)
	stepRun, err := workflows.GetLiveStepRunByTurnID(ctx, movedTurnID)
	if errors.Is(err, pgx.ErrNoRows) {
		return
	}
	if err != nil {
		a.logger.Warn("sessionactor: owed_review_request: read the moved turn's workflow attempt failed; the re-run is untracked", "moved_turn_id", movedTurnID.String(), "error", err)
		return
	}
	if err := workflows.AttachTurn(ctx, stepRun.ID, rerunTurnID); err != nil {
		a.logger.Warn("sessionactor: owed_review_request: hand the workflow attempt to the re-run failed", "step_run_id", stepRun.ID.String(), "error", err)
	}
}

// withdrawOwedRequestWorkflow ends the workflow run the moved turn's step
// attempt belongs to, when the engine tracked one, now that its request is
// dropped: cancelled, the way a person's stop ends a run
// (workflowengine.OnTurnWithdrawn). A drop is a policy decision -- the
// requester may no longer ask, the pull request kept moving, a stop came
// first -- never the step's outcome: no edge is followed, so no attempt of
// the step is queued again, and no escalation notice joins the one the
// drop posted. A no-op for an untracked turn.
func (a *Actor) withdrawOwedRequestWorkflow(ctx context.Context, tx pgx.Tx, movedTurnID pgtype.UUID) {
	workflowengine.OnTurnWithdrawn(ctx, a.stores.workflow.WithTx(tx), movedTurnID)
}
