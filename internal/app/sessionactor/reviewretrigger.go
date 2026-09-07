// This file (reviewretrigger.go) implements §24's ("review: automatic
// re-review on new commits", §24) own SECOND, automatic trigger for a
// review session's own turns -- alongside, never replacing, §8.2's
// existing manual label/button re-trigger (internal/adapters/inbound/
// httpapi/reviewretrigger.go), which this file's own logic never touches.
//
// # Where this fits in the pipeline
//
// internal/adapters/inbound/github/pullrequestsynchronize.go (§24.1) is
// this feature's own ingress: it re-arms the TimerReviewRetriggerDebounce
// named timer (command.go) DIRECTLY, bypassing this Actor's mailbox
// entirely (command.go's own Command sum type has no member for an
// inbound "new commits pushed" signal), in the SAME transaction as its
// own upsert of github_pr_sessions.pending_retrigger_head_sha (migrations/
// 000075). Only the timer's later FIRING reaches this Actor, exactly like
// every other named timer (§2) -- handleReviewRetriggerDebounceTimer below
// is that fire handler, dispatched from timerfired.go's own
// handleTimerFired switch.
//
// # Why this needs a network call outside any transaction
//
// Deciding whether to enqueue a turn (§24.3 steps 1-3) needs only
// Postgres reads; ACTUALLY enqueueing one (step 4) needs a fresh,
// live-fetched diff/head-sha (internal/app/reviewcontext.Fetch, the SAME
// "diff provably anchored to a live GetPullRequest call" guarantee every
// other review-trigger path already gets own
// fix) -- a real outbound GitHub API call. This mirrors dispatch.go's own
// established "plan inside a transact, real network call OUTSIDE any
// transaction, result written back in a fresh transact" shape
// (planDispatch/executeSpawn/executeDispatch) rather than holding a
// Postgres transaction open across a network round trip: this handler
// reads its own decision inside one a.transact call
// (readReviewRetriggerState), fetches the diff (if the decision calls for
// it) with no transaction open at all, then writes the outcome back
// inside a SECOND, fresh a.transact call.
//
// # Why the writes are each their own guarded UPDATE
//
// github_pr_sessions.pending_retrigger_head_sha has exactly two writers:
// this Actor (clearing it) and the synchronize webhook handler (setting
// it, from OUTSIDE the actor, on every new push). A new push can land in
// the gap between this handler's own read (readReviewRetriggerState) and
// its own write (the second a.transact call below) -- clearing the
// column unconditionally in that race would silently drop the NEWER
// push's own still-pending re-review need. Every clear here is therefore
// a compare-and-swap (postgres.GitHubPRSessionStore.
// ClearPendingRetriggerHeadSHA, CLAUDE.md/§11's own "guarded UPDATE ...
// WHERE for cross-writer transitions" idiom): pgx.ErrNoRows means a
// newer synchronize event already won the race.
//
// Rereview fix (finding 2): session_timers has UNIQUE(session_id, name),
// so there is exactly ONE review_retrigger_debounce row for this session
// -- never a second, separate row this handler "owns" apart from
// whatever the newer synchronize event re-armed. On a guard miss,
// finishReviewRetrigger's every guarded-clear call site therefore SKIPS
// its own subsequent deleteTimer call entirely, rather than deleting
// (unconditionally, as an earlier version of this file incorrectly did)
// what is, in fact, the SAME row the newer event just re-armed --
// timerfired.go's own re-arm-or-delete contract is satisfied either way:
// on a guard miss, the newer event's own already-committed re-arm IS
// this firing's own "re-arm" half of that contract, so this handler
// simply leaves it alone instead of performing its own now-stale
// "delete" half on top of it.

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
	"github.com/narvidev/narvi/internal/app/reviewcontext"
	appreviewtriage "github.com/narvidev/narvi/internal/app/reviewtriage"
	"github.com/narvidev/narvi/internal/domain/autoapproval"
	"github.com/narvidev/narvi/internal/domain/knowledge"
	"github.com/narvidev/narvi/internal/domain/reposource"
	"github.com/narvidev/narvi/internal/domain/review"
	"github.com/narvidev/narvi/internal/domain/reviewpost"
	domainreviewtriage "github.com/narvidev/narvi/internal/domain/reviewtriage"
)

// reviewAutoRetriggerBudget is §24.6's own per-PR budget on the AUTOMATIC
// re-review path only -- a human's manual label/button re-trigger
// (internal/adapters/inbound/httpapi/reviewretrigger.go) is never subject
// to it and always works, regardless of this count (§24.6: "two
// independent tracks by design"). A plain package constant, not a
// platform.Timeouts field or a repo_settings column: this is a COUNT, not
// a time.Duration (mailboxBufferSize's own identical "does not belong in
// platform.Timeouts" precedent, actor.go), and §24.6 itself only asks for
// a documented, reasoned default, not a per-repo override surface. Not
// given an explicit figure in the plan; chosen as 10 -- the plan's own
// proposed figure (§24.6: "propose 10 per PR").
const reviewAutoRetriggerBudget = 10

// autoRetriggerPromptText is this feature's own fixed, deterministically-
// synthesized turn prompt -- a webhook-driven re-review carries no human
// text at all, so any wording here is rendered server-side, never
// model-generated, mirroring httpapi's own manualRetriggerPromptText
// constant exactly in kind (a plain, fixed string) -- worded distinctly
// only so a maintainer reading turns.prompt later can tell which of this
// session's re-trigger paths actually produced a given turn.
const autoRetriggerPromptText = "Automatic re-review triggered: new commits were pushed to this pull request."

// autoRetriggerBudgetExhaustedBodyFormat renders §24.6's own one-time
// budget-exhausted notice -- a fixed, deterministic template (never
// model-generated), embedding reviewpost.RerunGuidance's own identical
// server-side re-run phrasing every OTHER posted verdict already carries
// (§5.2), so this notice is recognizable by the SAME deterministic
// mention-detection fallback path.
const autoRetriggerBudgetExhaustedBodyFormat = "Automatic re-review has reached its budget for this pull request (%d automatic re-reviews). Further pushes will not trigger another automatic re-review.\n\n%s"

// reviewRetriggerDecision is readReviewRetriggerState's own pure output --
// carries exactly what handleReviewRetriggerDebounceTimer needs to decide
// what to do next, without holding a transaction open across the network
// call §24.3 step 4 may require (see this file's own top comment).
type reviewRetriggerDecision struct {
	// action is one of the reviewRetriggerAction* constants below.
	action reviewRetriggerAction

	repoFullName            string
	prNumber                int32
	pendingHeadSHA          string
	autoRetriggerCount      int32
	budgetNoticeAlreadySent bool
	latestVerdictRiskLevel  string

	// latestVerdictReviewPath (§26.3) is the latest posted
	// verdict's own review_path column -- §24's own re-review floor
	// input ("once deep, a PR stays deep, even if the delta itself
	// would independently route light"). Empty when no verdict has ever
	// been posted for this PR, or when the latest one predates §26.3 /
	// never resolved a depth -- both degrade identically to "nothing to
	// floor against", mirroring latestVerdictRiskLevel's own identical
	// "no prior verdict" zero-value convention immediately above.
	latestVerdictReviewPath string

	// The four fields below are §26.3's own computed OUTPUT (§26.3),
	// set by handleReviewRetriggerDebounceTimer between phase 2 (fetch)
	// and phase 3 (finish/insert) -- never set by readReviewRetriggerState
	// itself, which only ever reads latestVerdictReviewPath above as an
	// INPUT. finalReviewDepth is decision.Depth AFTER §24's Floor has
	// been applied against latestVerdictReviewPath -- see
	// insertAutoRetriggerTurn's own doc comment for how these four ride
	// onto the inserted turn's own row.
	finalReviewDepth        string
	reviewDepthDecisionJSON []byte
	triageModelID           *string
	triageEffort            *string

	// knowledgeDecisionJSON (§31.6) is composeAutoRetriggerPrompt's own
	// marshaled knowledge.InjectedRecord -- mirrors reviewDepthDecisionJSON's
	// own identical "computed between phase 2 and phase 3" shape, one
	// field over. Set only when composeAutoRetriggerPrompt actually runs
	// (the Enqueue branch); nil otherwise, exactly like
	// reviewDepthDecisionJSON.
	knowledgeDecisionJSON []byte
}

type reviewRetriggerAction int

const (
	// reviewRetriggerActionDeleteOnly covers every case with nothing left
	// to clear: no github_pr_sessions row at all, no pending head sha, or
	// the per-repo opt-in is off/unreadable (§24.3 step 1's own fail-
	// closed branch, which the plan does not ask to also clear
	// pending_retrigger_head_sha -- see readReviewRetriggerState's own
	// doc comment).
	reviewRetriggerActionDeleteOnly reviewRetriggerAction = iota
	// reviewRetriggerActionHeadsMatch is §24.3 step 3: the pending head
	// sha already equals the latest posted verdict's own head sha --
	// clear pending_retrigger_head_sha, delete the timer, done.
	reviewRetriggerActionHeadsMatch
	// reviewRetriggerActionFetchFailed is handleReviewRetriggerDebounceTimer's
	// own downgrade of reviewRetriggerActionEnqueue when the live
	// reviewcontext.Fetch call could not resolve a head sha (§24's own
	// fail-closed "never guess-and-dispatch" direction) -- clear
	// pending_retrigger_head_sha, delete the timer, spend no budget.
	// Deliberately a SEPARATE action from reviewRetriggerActionDeleteOnly
	// below (which does NOT clear pending_retrigger_head_sha): a
	// transient fetch failure must still let a LATER firing retry against
	// a clean slate, unlike the opt-in-off case, which intentionally
	// leaves the stale pending value for whenever the repo opts back in.
	reviewRetriggerActionFetchFailed
	// reviewRetriggerActionBudgetExhausted is §24.6's own ceiling: clear
	// pending_retrigger_head_sha, delete the timer, and (the FIRST time
	// only) post the one-time budget-exhausted notice.
	reviewRetriggerActionBudgetExhausted
	// reviewRetriggerActionEnqueue is §24.3 step 4's "otherwise" branch:
	// fetch a fresh diff/head sha and enqueue a real review turn.
	reviewRetriggerActionEnqueue
)

// handleReviewRetriggerDebounceTimer implements the
// TimerReviewRetriggerDebounce named timer (§24.2/§24.3). See this file's
// own top comment for the two-phase (read-decide, then act) shape and why
// every write here is a guarded UPDATE.
func (a *Actor) handleReviewRetriggerDebounceTimer(ctx context.Context) error {
	decision, err := a.readReviewRetriggerState(ctx)
	if err != nil {
		return err
	}
	if decision == nil {
		// readReviewRetriggerState already fully handled (and logged) a
		// terminal case -- e.g. a stale/misrouted timer with no
		// github_pr_sessions row at all -- and left nothing further to do.
		return nil
	}

	var reviewCtx review.PreFetchedContext
	var prompt string
	if decision.action == reviewRetriggerActionEnqueue {
		reviewCtx = a.fetchAutoRetriggerReviewContext(ctx, decision.repoFullName, decision.prNumber)
		if reviewCtx.HeadSHA == "" {
			// §24's own fail-closed direction: an error path in this
			// feature must never guess-and-dispatch. Without a live,
			// provably-fresh head sha to anchor turns.review_head_sha to,
			// enqueueing a turn here would produce a review that can
			// never honestly be recorded in review_verdicts (§21's
			// NOT NULL head_sha column) -- downgrade to a plain no-op
			// this cycle: clear pending_retrigger_head_sha (a fresh
			// synchronize event will try again), delete the timer, spend
			// no budget.
			a.logger.Warn("sessionactor: review_retrigger_debounce: could not resolve a live head sha for this PR, declining to enqueue an automatic re-review turn this cycle",
				"repo_full_name", decision.repoFullName, "pr_number", decision.prNumber)
			decision.action = reviewRetriggerActionFetchFailed
		} else {
			// (§26.3): depth re-evaluated on the delta (this
			// PR's own CURRENT diff, reviewCtx above), THEN floored at
			// the PR's own previous depth ("once deep, a PR stays deep,
			// even if the delta itself would independently route
			// light") -- UNLESS this fresh decision is itself an explicit
			// always_light admin override (D9, below) -- decision.
			// latestVerdictReviewPath is the SAME
			// GetLatest read readReviewRetriggerState already performed
			// (phase 1), never a second, redundant review_verdicts
			// query. ComputeDecision itself performs its OWN further
			// GetLatest read (for the "prior high verdict" signal,
			// distinct from the floor) -- a second, small, harmless
			// query outside any transaction, accepted for reusing the
			// SAME shared entry point every other trigger path calls
			// rather than a bespoke variant just for this one caller.
			// D1 (adversarial-review fix): ComputeDecision's own third
			// return value (priorReviewDepth, compute.go) is deliberately
			// IGNORED here -- this lane already has its OWN, independently
			// obtained prior depth (decision.latestVerdictReviewPath,
			// read by readReviewRetriggerState's own phase-1 GetLatest,
			// above) to floor against, so using ComputeDecision's copy of
			// the identical fact here would be redundant, never a
			// correctness difference (both reads name the SAME latest
			// review_verdicts row for this repoFullName/prNumber).
			//
			// Adversarial-review fix (§26.4/§26.7): computed
			// BEFORE composeAutoRetriggerPrompt now, not after -- this
			// lane previously rendered the prompt FIRST and only computed
			// the floored depth afterward, so reviewCtx.DeepPath (never
			// assigned at all) stayed permanently false regardless of what
			// turns.review_depth eventually recorded: an auto-re-review
			// turn floored to deep by §24's own history rule persisted
			// review_depth="deep" while its OWN prompt kept telling the
			// agent every deep-path-only field was merely "REQUESTED, not
			// required" and never mentioned counterReview at all -- exactly
			// the D2-class contradiction the OTHER two trigger lanes
			// (internal/adapters/inbound/httpapi/reviewretrigger.go,
			// internal/adapters/inbound/github/handler.go) were already
			// fixed against, that this lane alone had never received. Previously
			// this only mis-set the prompt's own wording; now it
			// makes it a guaranteed 400 (reviewpost.ValidateVerdictInput's
			// own ErrInvalidCounterReview/ErrEmptyDigestArchDecisions) on
			// every such verdict, since the agent was never told
			// counterReview/the three digest fields were required at all.
			triageDeps := appreviewtriage.Deps{RepoSettings: a.stores.repoSettings, ReviewVerdicts: a.stores.reviewVerdict, Artifacts: a.stores.artifact, Sessions: a.stores.session}
			triageDecision, triageConfig, _ := appreviewtriage.ComputeDecision(ctx, triageDeps, decision.repoFullName, decision.prNumber, reviewCtx)
			triageProvenance := appreviewtriage.ResolveProvenance(ctx, triageDeps, decision.repoFullName, decision.prNumber)
			// D9 (adversarial-review fix): skip the floor entirely when
			// the FRESH decision's own Reason is ReasonAlwaysLightConfig
			// -- an explicit admin cost-control override (reviewDepth.
			// mode=always_light) outranks this history-based "always add
			// rigor" floor -- see domainreviewtriage.Floor's own doc
			// comment (depth.go) for the full "why" this precedence exists
			// and why it was never explicitly decided before this fix.
			// Without this guard, a PR that had EVER gone deep once would
			// stay deep on every subsequent auto-triggered push forever,
			// even after an admin flipped this repo to always_light --
			// and the persisted decision record would self-contradict
			// (mode "always_light" alongside depth "deep").
			flooredDepth := triageDecision.Depth
			if triageDecision.Reason != domainreviewtriage.ReasonAlwaysLightConfig {
				flooredDepth = domainreviewtriage.Floor(triageDecision.Depth, domainreviewtriage.ReviewDepth(decision.latestVerdictReviewPath))
			}
			decision.finalReviewDepth = string(flooredDepth)
			decision.triageModelID, decision.triageEffort = domainreviewtriage.ModelAndEffort(flooredDepth, a.reviewModelDeep)
			// D4 (nice-to-have adversarial-review fix): see internal/
			// adapters/inbound/httpapi/reviewretrigger.go's own identical
			// log line for the full "why" -- an operator otherwise has no
			// signal that a deep-routed turn's model-tier override is
			// silently inert.
			if flooredDepth == domainreviewtriage.DepthDeep && a.reviewModelDeep == "" {
				a.logger.Info("sessionactor: automatic re-review routed deep but no deep-tier model configured (NARVI_REVIEW_MODEL_DEEP unset), dispatching with the default model at forced high effort", "repo_full_name", decision.repoFullName, "pr_number", decision.prNumber)
			}
			// (§31.6): computed once, reused both for the write-time
			// stamp immediately below AND for the prior-decisions fetch
			// composeAutoRetriggerPrompt performs -- see internal/
			// adapters/inbound/github/handler.go's own identical addition
			// for the full "why this carrier, why computed once" reasoning.
			archTagStrings := autoapproval.TagStrings(autoapproval.ClassifyChangedPaths(reviewCtx.ChangedPaths))
			archRoots := autoapproval.ClassifyChangedRoots(reviewCtx.ChangedPaths)
			if recordJSON, marshalErr := json.Marshal(domainreviewtriage.NewDecisionRecord(triageDecision, triageConfig, flooredDepth, triageProvenance, decision.triageModelID, decision.triageEffort, reviewCtx.ChangedFilesCount, reviewCtx.Diff == "", reviewCtx.DiffTruncated, archTagStrings, archRoots)); marshalErr != nil {
				a.logger.Warn("sessionactor: marshal review-depth decision record failed, turn will carry review_depth but no review_depth_decision", "error", marshalErr, "repo_full_name", decision.repoFullName, "pr_number", decision.prNumber)
			} else {
				decision.reviewDepthDecisionJSON = recordJSON
			}

			// reviewCtx is a plain value here (never a pointer) -- both of
			// these MUST be set before composeAutoRetriggerPrompt below,
			// the one and only place this lane calls review.RenderTurnPrompt.
			reviewCtx.DeepPath = flooredDepth == domainreviewtriage.DepthDeep
			reviewCtx.ReviewCostBudgetUSD = triageConfig.CostBudget.ForDepth(flooredDepth)
			// B5 fix: threads reviewtriage.CostBudgetSafetyMargin through as
			// a whole percentage -- review.PreFetchedContext.
			// CostBudgetSafetyMarginPercent's own doc comment for why this
			// package (which already imports reviewtriage) is the one that
			// must set it, never review itself (doc.go's own "zero external
			// imports" convention).
			reviewCtx.CostBudgetSafetyMarginPercent = int(domainreviewtriage.CostBudgetSafetyMargin * 100)

			// Rereview fix (finding 1): compose §22.3's own false-positive
			// advisory block and §22.1's own already-answered-facts block
			// HERE, in this phase-2 window with no transaction open --
			// see composeAutoRetriggerPrompt's own doc comment for why
			// FetchFalsePositivePatterns' own IncrementHitCount side
			// effect must never run inside a transaction that might still
			// roll back -- before calling review.RenderTurnPrompt, mirroring
			// httpapi.RetriggerReview's own manual-button lane and
			// internal/adapters/inbound/github/handler.go's own mention/
			// label lane byte-for-byte in ordering. Moved to AFTER the
			// depth/cost-budget computation above (§26.4, this
			// block's own doc comment) -- reviewCtx.DeepPath/
			// ReviewCostBudgetUSD must already reflect the FLOORED depth
			// this turn is about to persist before its own prompt renders.
			prompt, decision.knowledgeDecisionJSON = a.composeAutoRetriggerPrompt(ctx, decision.repoFullName, decision.prNumber, reviewCtx, archTagStrings, archRoots)
		}
	}

	enqueued, err := a.finishReviewRetrigger(ctx, decision, reviewCtx, prompt)
	if err != nil {
		return err
	}

	if enqueued {
		if dispatchErr := a.handleEnsureDispatched(ctx); dispatchErr != nil {
			a.logger.Warn("sessionactor: ensure-dispatched after automatic re-review enqueue failed", "error", dispatchErr)
		}
	}
	return nil
}

// readReviewRetriggerState is handleReviewRetriggerDebounceTimer's own
// read-and-decide phase (§24.3 steps 1-3) -- a single a.transact call, no
// writes, no network calls. Returns (nil, nil) for every terminal case it
// fully handles and commits itself (no github_pr_sessions row at all, or
// no pending head sha to act on) -- deleting the timer inline rather than
// returning a decision the caller would have to special-case identically.
func (a *Actor) readReviewRetriggerState(ctx context.Context) (*reviewRetriggerDecision, error) {
	var decision *reviewRetriggerDecision

	err := a.transact(ctx, func(ctx context.Context, tx pgx.Tx) error {
		prSession, err := a.stores.githubPRSession.WithTx(tx).GetBySessionID(ctx, a.sessionID)
		if err != nil {
			if errors.Is(err, pgx.ErrNoRows) {
				// A stale or misrouted timer -- there is no PR identity
				// to act on at all. timerfired.go's own re-arm-or-delete
				// contract still applies: delete it.
				a.logger.Warn("sessionactor: review_retrigger_debounce fired for a session with no github_pr_sessions row, deleting timer")
				return a.deleteTimer(ctx, tx, TimerReviewRetriggerDebounce)
			}
			return fmt.Errorf("sessionactor: get github pr session: %w", err)
		}

		var pendingHeadSHA string
		if prSession.PendingRetriggerHeadSha != nil {
			pendingHeadSHA = *prSession.PendingRetriggerHeadSha
		}
		if pendingHeadSHA == "" {
			// Nothing pending to debounce (already cleared by an earlier
			// firing, or a leftover timer from before this column
			// existed) -- nothing to clear, just delete.
			return a.deleteTimer(ctx, tx, TimerReviewRetriggerDebounce)
		}

		// §24.3 step 1: re-read the per-repo opt-in, fail closed on any
		// read error OR a missing row -- mirrors appreviewverdict.
		// AutoMergeEnabled's own identical fail-closed shape exactly
		// (internal/app/reviewverdict/config.go).
		enabled := false
		if settings, settingsErr := a.stores.repoSettings.WithTx(tx).Get(ctx, prSession.RepoFullName); settingsErr != nil {
			if !errors.Is(settingsErr, pgx.ErrNoRows) {
				a.logger.Warn("sessionactor: review_retrigger_debounce: read repo settings failed, treating auto-retrigger-review as OFF",
					"error", settingsErr, "repo_full_name", prSession.RepoFullName)
			}
		} else {
			enabled = settings.AutoRetriggerReviewEnabled
		}
		if !enabled {
			// §24.3 step 1's own wording is exact: "the timer is simply
			// dropped" -- pending_retrigger_head_sha is deliberately left
			// untouched (a later opt-in flip, or simply the next real
			// push's own fresh synchronize event, re-arms a working timer
			// again; leaving the stale value costs nothing since it is
			// only ever read, never trusted blindly, the next time a
			// timer for this PR fires).
			a.logger.Info("sessionactor: review_retrigger_debounce: auto-retrigger-review is off (or unreadable) for this repo, dropping timer without re-review",
				"repo_full_name", prSession.RepoFullName, "pr_number", prSession.PrNumber)
			return a.deleteTimer(ctx, tx, TimerReviewRetriggerDebounce)
		}

		// §24.3 step 2: the latest posted verdict for this PR. TWO reads,
		// because this one row feeds three decisions and they do not all
		// have the same audience.
		//
		// GetLatestNonShadow for the customer-consequential pair (§30.8):
		// a shadow-era "already reviewed" fact must never suppress a REAL
		// re-review once this repo goes live, and a shadow-era risk level
		// must never be quoted in the real, customer-visible
		// budget-exhausted notice below.
		//
		// But the THIRD consumer is §24's "once deep, a PR stays deep"
		// floor, and that one is internal: it picks a model tier and an
		// effort for a review this platform is about to run on its own
		// machines. Filtering it would quietly make a shadow evaluation
		// behave DIFFERENTLY from the live system it exists to predict --
		// a PR that went deep in shadow would drop back to light on its
		// next shadow push, and the evaluation would under-report exactly
		// the escalation an operator is trying to observe. So the floor
		// reads the unfiltered latest.
		//
		// The split is the point: suppression is about what reaches a
		// customer, and evaluation fidelity is about everything else.
		var verdictHeadSHA, verdictRiskLevel, verdictReviewPath string
		if latest, verdictErr := a.stores.reviewVerdict.WithTx(tx).GetLatestNonShadow(ctx, prSession.RepoFullName, prSession.PrNumber); verdictErr != nil {
			if !errors.Is(verdictErr, pgx.ErrNoRows) {
				return fmt.Errorf("sessionactor: get latest review verdict: %w", verdictErr)
			}
			// No verdict ever posted for this PR -- verdictHeadSHA stays
			// "", which can never equal a real pendingHeadSHA, so the
			// comparison below correctly falls through to "otherwise".
		} else {
			verdictHeadSHA = latest.HeadSha
			verdictRiskLevel = latest.RiskLevel
			// (§26.3): review_path is nullable (a pre-existing
			// row, or a verdict whose own turn never resolved a depth)
			// -- degrades to "", the SAME "nothing to floor against"
			// reading as no prior verdict at all.
			if latest.ReviewPath != nil {
				verdictReviewPath = *latest.ReviewPath
			}
		}
		// The floor's own read, unfiltered -- see the two-reads note
		// above. A shadow-era depth is a fact about how this platform
		// reviews, not an effect on anyone's repository.
		if anyLatest, anyErr := a.stores.reviewVerdict.WithTx(tx).GetLatest(ctx, prSession.RepoFullName, prSession.PrNumber); anyErr != nil {
			if !errors.Is(anyErr, pgx.ErrNoRows) {
				return fmt.Errorf("sessionactor: get latest review verdict for the depth floor: %w", anyErr)
			}
		} else if anyLatest.ReviewPath != nil && verdictReviewPath == "" {
			verdictReviewPath = *anyLatest.ReviewPath
		}

		base := reviewRetriggerDecision{
			repoFullName:            prSession.RepoFullName,
			prNumber:                prSession.PrNumber,
			pendingHeadSHA:          pendingHeadSHA,
			autoRetriggerCount:      prSession.AutoRetriggerCount,
			budgetNoticeAlreadySent: prSession.AutoRetriggerBudgetNoticeSentAt.Valid,
			latestVerdictRiskLevel:  verdictRiskLevel,
			latestVerdictReviewPath: verdictReviewPath,
		}

		switch {
		case pendingHeadSHA == verdictHeadSHA:
			// §24.3 step 3: a race already reviewed this exact sha (a
			// manual re-trigger, or an earlier automatic one) -- nothing
			// to do.
			base.action = reviewRetriggerActionHeadsMatch
		case prSession.AutoRetriggerCount >= reviewAutoRetriggerBudget:
			base.action = reviewRetriggerActionBudgetExhausted
		default:
			base.action = reviewRetriggerActionEnqueue
		}
		decision = &base
		return nil
	})
	if err != nil {
		return nil, err
	}
	return decision, nil
}

// fetchAutoRetriggerReviewContext live-fetches this PR's own current
// diff/stack, pinned to a freshly-resolved head sha (internal/app/
// reviewcontext.Fetch -- the SAME assembly point every other review-
// trigger path already uses), OUTSIDE any Postgres transaction (this
// file's own top comment). A nil a.reviewDiffFetcher (not configured for
// this deployment/test) degrades identically to a live fetch failure --
// review.PreFetchedContext's own honest zero value, HeadSHA == "".
func (a *Actor) fetchAutoRetriggerReviewContext(ctx context.Context, repoFullName string, prNumber int32) review.PreFetchedContext {
	if a.reviewDiffFetcher == nil {
		a.logger.Warn("sessionactor: review_retrigger_debounce: no review diff fetcher configured, cannot resolve a live head sha",
			"repo_full_name", repoFullName, "pr_number", prNumber)
		return review.PreFetchedContext{}
	}
	owner, repo, ok := reposource.SplitFullName(repoFullName)
	if !ok {
		a.logger.Warn("sessionactor: review_retrigger_debounce: repo_full_name not in owner/repo shape",
			"repo_full_name", repoFullName)
		return review.PreFetchedContext{}
	}
	return reviewcontext.Fetch(ctx, a.logger, a.reviewDiffFetcher, a.timeouts, owner, repo, prNumber, a.githubBotToken, nil)
}

// finishReviewRetrigger is handleReviewRetriggerDebounceTimer's own act
// phase -- a single, fresh a.transact call applying decision (enriched
// with reviewCtx/prompt when decision.action ==
// reviewRetriggerActionEnqueue, both already fully resolved by phase 2,
// with no transaction open -- see composeAutoRetriggerPrompt's own doc
// comment) -- see this file's own top comment for why every write here is
// a guarded UPDATE, and why this is a SEPARATE transaction from
// readReviewRetriggerState's. Returns whether a turn was actually
// enqueued (the caller's own signal to call handleEnsureDispatched
// afterward).
func (a *Actor) finishReviewRetrigger(ctx context.Context, decision *reviewRetriggerDecision, reviewCtx review.PreFetchedContext, prompt string) (bool, error) {
	enqueued := false

	err := a.transact(ctx, func(ctx context.Context, tx pgx.Tx) error {
		switch decision.action {
		case reviewRetriggerActionDeleteOnly:
			return a.deleteTimer(ctx, tx, TimerReviewRetriggerDebounce)

		case reviewRetriggerActionHeadsMatch, reviewRetriggerActionFetchFailed:
			guardMissed, err := a.clearPendingRetriggerHeadSHAGuarded(ctx, tx, decision)
			if err != nil {
				return err
			}
			if guardMissed {
				// Rereview fix (finding 2): the newer event that won this
				// race already re-armed the SAME session_timers row (see
				// this file's own top comment) -- deleting it here would
				// strand that newer event's own pending head sha with no
				// timer left to ever act on it again. Skip the delete;
				// the newer event's own re-arm already satisfies
				// timerfired.go's re-arm-or-delete contract for this
				// firing too.
				return nil
			}
			return a.deleteTimer(ctx, tx, TimerReviewRetriggerDebounce)

		case reviewRetriggerActionBudgetExhausted:
			if !decision.budgetNoticeAlreadySent {
				if _, markErr := a.stores.githubPRSession.WithTx(tx).MarkAutoRetriggerBudgetNoticeSent(ctx, decision.repoFullName, decision.prNumber); markErr != nil {
					if !errors.Is(markErr, pgx.ErrNoRows) {
						return fmt.Errorf("sessionactor: mark auto-retrigger budget notice sent: %w", markErr)
					}
					// ErrNoRows: already marked by an earlier firing --
					// this actor processes one command at a time for
					// this session (§2), so in practice this branch is
					// unreachable, but the guard is honored defensively
					// like every other guarded write in this file.
				} else if err := a.enqueueAutoRetriggerBudgetExhaustedNotice(ctx, tx, decision); err != nil {
					return err
				}
			}
			guardMissed, err := a.clearPendingRetriggerHeadSHAGuarded(ctx, tx, decision)
			if err != nil {
				return err
			}
			if guardMissed {
				// See the identical comment on the HeadsMatch/FetchFailed
				// branch above -- same race, same fix, same reasoning.
				return nil
			}
			return a.deleteTimer(ctx, tx, TimerReviewRetriggerDebounce)

		case reviewRetriggerActionEnqueue:
			// Rereview fix (finding 4): mirror createTurnLocked's own
			// awaiting-plan gate (internal/adapters/inbound/httpapi/
			// turn.go) -- an ordinary (planMode == false) turn, which
			// every automatic re-review turn always is, must never
			// dispatch while this session has a plan sitting in
			// plan.StatusAwaitingApproval. See
			// reviewSessionHasAwaitingApprovalPlan's own doc comment for
			// why this is a real, reachable precondition here, not dead
			// code.
			awaitingPlan, err := a.reviewSessionHasAwaitingApprovalPlan(ctx, tx)
			if err != nil {
				return err
			}
			if awaitingPlan {
				a.logger.Info("sessionactor: review_retrigger_debounce: declining to enqueue an automatic re-review turn -- a plan is awaiting approval on this session",
					"repo_full_name", decision.repoFullName, "pr_number", decision.prNumber)
			} else {
				if err := a.insertAutoRetriggerTurn(ctx, tx, decision, prompt, reviewCtx.HeadSHA); err != nil {
					return err
				}
				if _, err := a.stores.githubPRSession.WithTx(tx).IncrementAutoRetriggerCount(ctx, decision.repoFullName, decision.prNumber); err != nil {
					return fmt.Errorf("sessionactor: increment auto retrigger count: %w", err)
				}
				enqueued = true
			}
			guardMissed, err := a.clearPendingRetriggerHeadSHAGuarded(ctx, tx, decision)
			if err != nil {
				return err
			}
			if guardMissed {
				// See the identical comment on the HeadsMatch/FetchFailed
				// branch above -- same race, same fix, same reasoning.
				// enqueued (if true) is unaffected: a turn genuinely was
				// (or wasn't) just inserted above, independent of which
				// push's own pending_retrigger_head_sha/timer this
				// firing's own clear/delete happens to end up touching.
				return nil
			}
			return a.deleteTimer(ctx, tx, TimerReviewRetriggerDebounce)

		default:
			return fmt.Errorf("sessionactor: unhandled review-retrigger action %d", decision.action)
		}
	})
	if err != nil {
		return false, err
	}
	return enqueued, nil
}

// clearPendingRetriggerHeadSHAGuarded is the one shared "clear, tolerating
// a race" call every reviewRetriggerAction branch above starts its own
// tail with -- see this file's own top comment for the compare-and-swap
// this performs. Returns guardMissed == true when pgx.ErrNoRows means a
// newer synchronize event already won the race (an expected, harmless
// outcome, never an error) -- see every call site above for why a true
// result means the caller must skip its own subsequent deleteTimer call
// (rereview fix, finding 2).
func (a *Actor) clearPendingRetriggerHeadSHAGuarded(ctx context.Context, tx pgx.Tx, decision *reviewRetriggerDecision) (guardMissed bool, err error) {
	if _, err := a.stores.githubPRSession.WithTx(tx).ClearPendingRetriggerHeadSHA(ctx, decision.repoFullName, decision.prNumber, decision.pendingHeadSHA); err != nil {
		if !errors.Is(err, pgx.ErrNoRows) {
			return false, fmt.Errorf("sessionactor: clear pending retrigger head sha: %w", err)
		}
		a.logger.Info("sessionactor: review_retrigger_debounce: pending_retrigger_head_sha already moved on (a newer push raced in) -- leaving it, and the SAME session_timers row that push's own re-arm just updated, alone for that push's own firing to handle",
			"repo_full_name", decision.repoFullName, "pr_number", decision.prNumber)
		return true, nil
	}
	return false, nil
}

// reviewSessionHasAwaitingApprovalPlan reports whether this actor's own
// session currently has a plan row sitting in plan.StatusAwaitingApproval
// -- createTurnLocked's own identical gate (internal/adapters/inbound/
// httpapi/turn.go), reused here (via the SAME PlanStore.
// ListSummariesForSession query that gate itself uses) because an
// automatic re-review turn is exactly the kind of "ordinary (planMode ==
// false) turn" that gate exists to hold back.
//
// This precondition IS reachable for a review session (rereview fix,
// finding 4, correcting this file's own earlier false claim to the
// contrary): internal/adapters/inbound/httpapi/turn.go's CreateTurn
// forwards client-supplied req.PlanMode into CreateTurnCore with no
// session-kind restriction, so a maintainer/admin CAN submit a
// planMode=true turn on a GitHub review session via the ordinary REST
// API, and planrecord.go's own recordPlanIfNeeded WILL then write an
// awaiting_approval plans row for that session -- httpapi's own manual
// re-trigger path (reviewretrigger.go) deliberately keeps this exact
// gate for review sessions today, and this automatic path now matches it.
func (a *Actor) reviewSessionHasAwaitingApprovalPlan(ctx context.Context, tx pgx.Tx) (bool, error) {
	summaries, err := a.stores.plan.WithTx(tx).ListSummariesForSession(ctx, a.sessionID)
	if err != nil {
		return false, fmt.Errorf("sessionactor: list plan summaries for review-retrigger awaiting-plan gate: %w", err)
	}
	for _, s := range summaries {
		if s.Status == sqlcgen.PlanStatusAwaitingApproval {
			return true, nil
		}
	}
	return false, nil
}

// composeAutoRetriggerPrompt builds the Enqueue branch's own final review-
// turn prompt text -- called by handleReviewRetriggerDebounceTimer's own
// phase 2, with NO transaction open (rereview fix, finding 1). Prepends
// §22.3's own learned false-positive advisory block, then §22.1's own
// already-answered-facts block, to autoRetriggerPromptText, exactly like
// httpapi.RetriggerReview's own manual-button lane and internal/adapters/
// inbound/github/handler.go's own mention/label lane already do -- before
// this fix, the automatic lane was the ONLY review-turn producer in this
// codebase that skipped both, self-defeating against §24.6's own per-PR
// budget (the budget exists to break an automated-fix-to-automated-review
// loop; telling the agent what was already rebutted/taught-as-false-
// positive is the strongest suppressant of that exact loop). Only THEN
// calls review.RenderTurnPrompt with reviewCtx, mirroring both of those
// callers' own identical ordering (prepend context blocks, then render
// diff/stack/verdict-tool-instructions last).
//
// Deliberately called here, not inside insertAutoRetriggerTurn: this
// function calls reviewcontext.FetchFalsePositivePatterns, whose own
// IncrementHitCount is a real, best-effort Postgres WRITE side effect
// (§22.4's usage-signal bookkeeping) that must never run inside a
// transaction that might still roll back -- insertAutoRetriggerTurn runs
// inside finishReviewRetrigger's own transact call, but this function
// runs before that transaction ever opens, exactly like
// fetchAutoRetriggerReviewContext's own live network fetch already does
// for the same reason (this file's own top comment).
// archTags/archRoots (§31.6) are the SAME server-derived gate key the
// caller already computed (once) for the write-time stamp -- passed in
// rather than recomputed here, mirroring reviewCtx itself. Returns the
// composed prompt AND the marshaled knowledge.InjectedRecord (nil when
// a.stores.reviewVerdict is nil, or on a marshal failure) for the caller
// to persist onto decision.knowledgeDecisionJSON, exactly like
// reviewDepthDecisionJSON's own identical "computed here, stored on
// decision" shape.
func (a *Actor) composeAutoRetriggerPrompt(ctx context.Context, repoFullName string, prNumber int32, reviewCtx review.PreFetchedContext, archTags, archRoots []string) (string, []byte) {
	prompt := autoRetriggerPromptText
	advisory := reviewcontext.FetchFalsePositivePatterns(ctx, a.logger, a.stores.falsePositivePattern, repoFullName)
	if advisory != "" {
		prompt = advisory + prompt
	}
	// (§31.6 item 1): placed between the advisory and already-answered
	// blocks above/below, mirroring the OTHER two review-turn producers'
	// own identical relative ordering (handler.go, httpapi/
	// reviewretrigger.go) -- still entirely before RenderTurnPrompt.
	// a.stores.reviewVerdict already satisfies reviewcontext.
	// ArchDecisionsFetcher directly (reviewverdictarchdecisions.go); no
	// separate fetcher field needed on this Actor.
	var knowledgeDecisionJSON []byte
	var archBlock string
	if a.stores.reviewVerdict != nil {
		var injected knowledge.InjectedRecord
		archBlock, injected = reviewcontext.FetchPriorArchDecisions(ctx, a.logger, a.stores.reviewVerdict, a.knowledgeRanker, a.timeouts, knowledge.Query{
			RepoFullName: repoFullName,
			Tags:         archTags,
			Roots:        archRoots,
			ChangedPaths: reviewCtx.ChangedPaths,
			Title:        reviewCtx.Title,
		})
		if archBlock != "" {
			prompt = archBlock + prompt
		}
		if recJSON, marshalErr := json.Marshal(injected); marshalErr != nil {
			a.logger.Warn("sessionactor: marshal knowledge injected-ids record failed, turn will carry review_knowledge_mode but no review_knowledge_decision", "error", marshalErr, "repo_full_name", repoFullName, "pr_number", prNumber)
		} else {
			knowledgeDecisionJSON = recJSON
		}
	}
	// (§31.2's own "instrumented trigger"): the token gauge, mirrors
	// handler.go's own identical call site.
	reviewcontext.RecordKnowledgeBlockTokens(ctx, advisory, archBlock)
	if alreadyAnswered := reviewcontext.FetchAlreadyAnswered(ctx, a.logger, a.stores.reviewFinding, repoFullName, prNumber, reviewCtx.ChangedPaths, reviewCtx.DiffTruncated); alreadyAnswered != "" {
		prompt = alreadyAnswered + prompt
	}
	return review.RenderTurnPrompt(prompt, reviewCtx), knowledgeDecisionJSON
}

// insertAutoRetriggerTurn is §24.3 step 4's own turn creation -- CANNOT
// call httpapi.CreateTurnForBot (§8.2's manual path): internal/app/
// sessionactor cannot import internal/adapters/inbound/httpapi (httpapi
// already imports sessionactor throughout its bot/create/turn/plan files;
// the reverse would be a compile-time import cycle), and createTurnLocked
// (the function CreateTurnForBot wraps) is unexported besides. This
// inserts the turn directly via a.stores.turn.Create -- the SAME
// store-level primitive createTurnLocked itself calls
// (internal/adapters/inbound/httpapi/turn.go) -- mirroring §8.2's
// manual path at the storage layer rather than calling through it.
//
// prompt is ALREADY the fully-composed, fully-rendered turn text
// (composeAutoRetriggerPrompt, called by this handler's own phase 2,
// BEFORE this transaction ever opened) -- this function itself never
// calls review.RenderTurnPrompt or either reviewcontext Fetch* function,
// it only persists what phase 2 already built. headSHA is reviewCtx.
// HeadSHA, threaded through as its own parameter for the same reason.
//
// # Which of createTurnLocked's own extra logic is duplicated here, and why
//
// The audit-log write IS duplicated (below): recordPlanIfNeeded's own
// "plan.superseded" write (planrecord.go) already establishes the
// precedent that a system-triggered state change inside this package logs
// an audit_log row with an explicitly invalid actor_user_id
// (pgtype.UUID{}) -- an UNATTENDED action is, if anything, MORE valuable
// to have an audit trail for than a human-clicked one, not less.
//
// The awaiting-plan gate IS now also duplicated (rereview fix, finding
// 4) -- one level up, in this function's own caller
// (finishReviewRetrigger's reviewRetriggerActionEnqueue branch, via
// reviewSessionHasAwaitingApprovalPlan), not here. This file used to
// claim the gate was "DELIBERATELY NOT duplicated" because "no review
// session can EVER have an awaiting_approval plan row to gate against in
// the first place" -- that claim was FALSE: internal/adapters/inbound/
// httpapi/turn.go's CreateTurn forwards client-supplied req.PlanMode into
// CreateTurnCore with no session-kind restriction, so a maintainer/admin
// CAN submit a planMode=true turn on a GitHub review session via the
// ordinary REST API, and planrecord.go's own recordPlanIfNeeded WILL then
// write an awaiting_approval plans row for that session -- exactly the
// precondition reviewSessionHasAwaitingApprovalPlan's own doc comment
// names. httpapi's own manual re-trigger path (reviewretrigger.go)
// already kept this exact gate for review sessions; this automatic path
// now matches it, closing what was a real, if narrow, divergence between
// the two.
//
// workflowengine (§25.6) wiring is DELIBERATELY NOT duplicated:
// createTurnLocked calls workflowengine.ResolveStepForNewTurn/AttachTurn
// for every turn it creates, so that turn picks up its lane's configured
// workflow prompt/model/effort and is tracked by a workflow run --
// insertAutoRetriggerTurn does neither. An automatic re-review turn
// degrades safely without this (an untracked turn is already the
// expected, safely-handled common case everywhere else workflowengine
// reads turns from), and wiring it in is not a low-risk addition:
// ResolveStepForNewTurn resolves a LANE from sessionRow.IntentDecision
// (a signal with no meaning for a review-only session) and can rewrite
// prompt/model/effort via a step's own PromptTemplate, which risks
// silently reshaping the carefully delimited §22.1/§22.3/verdict-tool-
// instructions text composeAutoRetriggerPrompt just built, in ways no
// test in this codebase exercises today. Left as a documented, deliberate
// omission (rereview finding 9) rather than a speculative rewrite of this
// file's own prompt-composition contract.
// reviewDepth/reviewDepthDecision/modelID/effort (§26.3) are
// decision's own finalReviewDepth/reviewDepthDecisionJSON/triageModelID/
// triageEffort fields, already computed and FLOORED (§24) by
// handleReviewRetriggerDebounceTimer before this function's own caller
// (finishReviewRetrigger) ever runs -- this function does no further
// triage computation of its own, it only persists what was already
// decided, mirroring headSHA's own identical "already resolved
// upstream, just persisted here" shape.
func (a *Actor) insertAutoRetriggerTurn(ctx context.Context, tx pgx.Tx, decision *reviewRetriggerDecision, prompt string, headSHA string) error {
	var reviewDepth *string
	if decision.finalReviewDepth != "" {
		reviewDepth = &decision.finalReviewDepth
	}
	// knowledgeMode (§31.2) is unconditionally knowledge.ModeA today --
	// see that constant's own doc comment for why (the admin-facing
	// mode switch does not exist yet).
	knowledgeMode := knowledge.ModeA
	created, err := a.stores.turn.WithTx(tx).Create(ctx, sqlcgen.CreateTurnParams{
		SessionID:               a.sessionID,
		Status:                  sqlcgen.TurnStatusPending,
		Prompt:                  &prompt,
		ModelID:                 decision.triageModelID,
		Effort:                  decision.triageEffort,
		PlanMode:                false,
		ReviewHeadSha:           &headSHA,
		ReviewDepth:             reviewDepth,
		ReviewDepthDecision:     decision.reviewDepthDecisionJSON,
		ReviewKnowledgeMode:     &knowledgeMode,
		ReviewKnowledgeDecision: decision.knowledgeDecisionJSON,
	})
	if err != nil {
		return fmt.Errorf("sessionactor: insert automatic re-review turn: %w", err)
	}

	if err := auditlog.Record(ctx, a.stores.auditLog.WithTx(tx), pgtype.UUID{}, "turn.create", "turn", created.ID.String(), map[string]any{
		"session_id": a.sessionID.String(),
		"trigger":    "review_auto_retrigger",
		"head_sha":   headSHA,
		"repo":       decision.repoFullName,
		"pr_number":  decision.prNumber,
	}); err != nil {
		return fmt.Errorf("sessionactor: record turn.create audit log: %w", err)
	}
	return nil
}

// enqueueAutoRetriggerBudgetExhaustedNotice is §24.6's own one-time
// notice -- posted through the SAME verdict-posting mechanism every
// OTHER review-session write to a PR already goes through (§8.2's
// raw-comment blocking: this is the sanctioned way review-session content
// ever reaches a PR at all), never a raw comment. This is NOT a real
// review.Verdict -- no risk assessment happened -- so, deliberately
// unlike httpapi.PostReviewVerdict, this does NOT insert a review_verdicts
// row (that table's own doc comment, migrations/000067: "every column
// below is forwarded verbatim from a value already computed server-side"
// -- there is no honestly-computed RiskLevel/Shippable to forward here).
//
// RiskLevel in the payload is decision.latestVerdictRiskLevel -- this
// PR's own last REAL verdict's risk, not a fabricated new assessment.
// When a real verdict WAS previously posted for this PR, this makes
// VerdictNotifier's own label sync (ComputeLabelSync) resolve to a no-op
// (the PR's current label already reflects that same risk from the real
// verdict that posted it). When NO verdict has ever been posted for this
// PR (a real, reachable state: the budget can exhaust from automatic
// re-reviews alone, with every one of them declining before ever posting
// a verdict -- e.g. every firing hit reviewRetriggerActionFetchFailed),
// latestVerdictRiskLevel is "" -- rereview fix (finding 6): VerdictNotifier.
// Deliver now treats an empty RiskLevel as an explicit, intentional
// "skip label sync entirely" signal, rather than letting
// reviewpost.RiskLabel's own fail-conservative default (an empty/
// unrecognized RiskLevel renders as review:high-risk) stamp a "high risk"
// label on a PR this notice never actually assessed.
func (a *Actor) enqueueAutoRetriggerBudgetExhaustedNotice(ctx context.Context, tx pgx.Tx, decision *reviewRetriggerDecision) error {
	owner, repo, ok := reposource.SplitFullName(decision.repoFullName)
	if !ok {
		a.logger.Warn("sessionactor: review_retrigger_debounce: repo_full_name not in owner/repo shape, skipping budget-exhausted notice",
			"repo_full_name", decision.repoFullName)
		return nil
	}

	body := fmt.Sprintf(autoRetriggerBudgetExhaustedBodyFormat, reviewAutoRetriggerBudget, reviewpost.RerunGuidance(a.githubBotHandle))

	payload, err := json.Marshal(githubapi.VerdictPayload{
		Owner:     owner,
		Repo:      repo,
		PRNumber:  int(decision.prNumber),
		Event:     string(reviewpost.FormalReviewEventComment),
		Body:      body,
		RiskLevel: decision.latestVerdictRiskLevel,
	})
	if err != nil {
		return fmt.Errorf("sessionactor: marshal auto-retrigger budget-exhausted notice payload: %w", err)
	}

	if _, err := a.stores.outbox.WithTx(tx).Create(ctx, sqlcgen.CreateOutboxEntryParams{
		SessionID: a.sessionID,
		Kind:      string(ports.NotificationKindGitHubVerdict),
		Payload:   payload,
	}); err != nil {
		return fmt.Errorf("sessionactor: insert github_verdict outbox entry for budget-exhausted notice: %w", err)
	}
	return nil
}
