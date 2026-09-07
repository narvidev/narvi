// This file (reviewretrigger.go) implements §8.2's ("review sessions",
// §8.2) own manual re-trigger-via-BUTTON surface: POST
// /api/sessions/:id/review/retrigger (§12.2 item 2's own "re-run action"
// on the Code review view). This is the THIRD of §8.2's three manual
// re-trigger surfaces -- see internal/adapters/inbound/github's own doc.go
// for the other two (an @mention comment, and the new label lane) and for
// why all three are equally legitimate, deliberate human commands (§5.1).
//
// Unlike the other two, this handler never creates a brand-new review
// session -- it targets an ALREADY-KNOWN session_id (the URL itself), so
// there is no "first mention on this PR" ambiguity for
// github_pr_sessions' own atomic claim to resolve; this handler never
// touches that claim row at all. What it DOES reuse from §8.2/46's
// own existing machinery: CreateTurnCore (turn.go) with AlwaysQueue --
// the SAME policy coalesce.go's own REUSE branch uses via
// CreateTurnForBot (bot.go) -- so a manual re-review click behaves exactly
// like another @mention on the same PR: it always succeeds, queuing
// alongside whatever else is already Pending/Dispatched/Processing on this
// session, rather than the ordinary REST CreateTurn endpoint's own
// RejectIfOpen 409. A human clicking "re-run review" on a PR that already
// has other review turns in flight should queue behind them, not be told
// to try again later -- the two surfaces (@mention, button) are the same
// kind of command wearing a different UI.

package httpapi

import (
	"encoding/json"
	"errors"
	"net/http"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/narvidev/narvi/contracts/gen/go/restdtos"
	"github.com/narvidev/narvi/internal/adapters/outbound/postgres"
	"github.com/narvidev/narvi/internal/app/reviewcontext"
	appreviewtriage "github.com/narvidev/narvi/internal/app/reviewtriage"
	"github.com/narvidev/narvi/internal/app/sessionactor"
	"github.com/narvidev/narvi/internal/domain/authz"
	"github.com/narvidev/narvi/internal/domain/autoapproval"
	"github.com/narvidev/narvi/internal/domain/reposource"
	"github.com/narvidev/narvi/internal/domain/review"
	domainreviewtriage "github.com/narvidev/narvi/internal/domain/reviewtriage"
	"github.com/narvidev/narvi/internal/platform"
)

// manualRetriggerPromptText is this endpoint's own fixed, deterministically-
// synthesized turn prompt -- a REST button click carries no request body
// at all (this endpoint deliberately never accepts one: any re-run
// phrasing is rendered server-side, never client-supplied, mirroring §5.2's
// "any re-run phrasing a posted verdict recommends ... is rendered
// server-side from the verdict's own typed fields, never generated ... by
// a model" -- the same discipline applied here to a human's own explicit
// button click, which needs no model-generated wording at all). Mirrors
// internal/adapters/inbound/github's own labelRetriggerPromptText constant
// exactly in kind (a plain, fixed string, never model-generated) -- worded
// distinctly only so a maintainer reading turns.prompt later can tell
// which of §8.2's three manual surfaces actually triggered a given
// turn.
const manualRetriggerPromptText = "Manual re-review requested via the web review button."

// RetriggerReview backs POST /api/sessions/{sessionID}/review/retrigger.
// 404 if sessionID doesn't exist; 400 if it exists but was never created
// via a GitHub PR mention (github_pr_sessions has no reverse row for it --
// this action is meaningless for a session with no PR to review); 403 if
// the authenticated caller fails the authz.ActionRetriggerReview check
// (§13.3 row 5: "edit review verdicts; re-trigger reviews; auto-approval
// eligibility config" -- admin/maintainer only, no member own/joined
// carve-out, unlike CreateTurn's own ActionPromptSession gate); otherwise
// 201 with the newly-queued turn, exactly like CreateTurn's own response
// shape.
//
// Audit fix: this endpoint previously authorized against
// authz.ActionPromptSession (the SAME own/joined-aware check CreateTurn's
// REST endpoint applies), which let a plain member re-trigger a review on
// any session they created or joined -- a privilege escalation against
// §13.3's own RBAC matrix, which reserves "re-trigger reviews" for
// admin/maintainer with no member carve-out at all (unlike row 2's
// ordinary prompt/create carve-out). ActionRetriggerReview has no
// allowIfOwned entry (domain/authz/authorize.go), so ownership/
// participation is never consulted here any more -- ownedOrJoined is no
// longer computed at all (it would be silently ignored by Authorize for
// this Action regardless, per Resource's own doc comment on fields an
// Action doesn't consult), avoiding a wasted Postgres participants read on
// every call.
//
// No intentSvc parameter (F1, §23 follow-up fix, review Finding 1):
// this endpoint used to thread the platform's real *intentclassifier.
// Service straight through to CreateTurnCore below, which meant a plan
// sitting in StatusAwaitingApproval on this session made createTurnLocked's
// own plan_followup block (turn.go) classify manualRetriggerPromptText
// (plus review.RenderTurnPrompt's own folded-in diff/stack/verdict-tool
// text once diffFetcher is wired) as if it were a human's own amend-vs-
// answer reply -- but a manual re-review button click is a deterministic
// system command, never a reply to the plan at all; there is no human
// text here for that classifier to legitimately read. CreateTurnCore is
// now always called with a literal nil intentSvc below, which -- per that
// function's own nil-safe contract (turn.go's own doc comment: "a nil
// intentSvc ... skips classification entirely and falls back to the
// pre-existing 'always decline' awaiting-plan gate behavior") -- degrades
// this endpoint to the SAME safe, deterministic "decline while a plan is
// awaiting approval" outcome every pre-existing caller already got, with no
// outbound LLM call spent classifying text that was never a reply to
// begin with (the fail-safe direction §23's own review batch requires:
// "when in doubt, skip classification rather than guess").
// reviewTriageDeps/reviewModelDeep (§26.3) mirror internal/
// adapters/inbound/github's own identical SessionCoalescer.ReviewTriage/
// ReviewModelDeep fields -- see that struct's own doc comment.
func RetriggerReview(pool *pgxpool.Pool, sessions *postgres.SessionStore, turns *postgres.TurnStore, plans *postgres.PlanStore, auditLog *postgres.AuditLogStore, registry *sessionactor.Registry, prSessions *postgres.GitHubPRSessionStore, diffFetcher reviewcontext.Fetcher, reviewFindings reviewcontext.FindingsFetcher, falsePositivePatterns reviewcontext.FalsePositivePatternsFetcher, botToken string, timeouts platform.Timeouts, reviewTriageDeps appreviewtriage.Deps, reviewModelDeep string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		sessionID, ok := parseSessionID(w, r)
		if !ok {
			return
		}
		ctx := platform.WithSessionID(r.Context(), sessionID.String())
		r = r.WithContext(ctx)
		logger := platform.Logger(ctx)

		// Mirrors CreateTurn's own "404 before 403" sequencing (turn.go):
		// fetch the session first so a caller never learns "you can't
		// retrigger this" about a session that doesn't exist at all, THEN
		// render the authz verdict -- but the verdict itself is
		// authz.ActionRetriggerReview, admin/maintainer only (§13.3 row 5),
		// never CreateTurn's own own/joined-aware ActionPromptSession.
		actorUserID, ok := authenticatedUserID(w, r)
		if !ok {
			return
		}

		if _, err := sessions.Get(ctx, sessionID); err != nil {
			if errors.Is(err, pgx.ErrNoRows) {
				writeError(w, http.StatusNotFound, "session not found")
				return
			}
			logger.Error("httpapi: get session for authorization failed", "error", err)
			writeError(w, http.StatusInternalServerError, "internal error")
			return
		}

		if !authorize(w, r, authz.ActionRetriggerReview, authz.Resource{}) {
			return
		}

		// This action is meaningful ONLY for a session that IS a GitHub PR
		// review session -- the reverse (session_id -> repo/PR) lookup
		// §5.1 ("outbox delivery") already added GitHubPRSessionStore for.
		// pgx.ErrNoRows here means sessionID was never created via a GitHub
		// PR mention/label -- a plain web/Slack/Linear session has no PR to
		// re-review, so this is a genuine 400, not a transient failure.
		prSession, err := prSessions.GetBySessionID(ctx, sessionID)
		if err != nil {
			if errors.Is(err, pgx.ErrNoRows) {
				writeError(w, http.StatusBadRequest, "this session has no associated GitHub pull request to review")
				return
			}
			logger.Error("httpapi: look up github_pr_sessions by session id failed", "error", err)
			writeError(w, http.StatusInternalServerError, "internal error")
			return
		}

		prompt := manualRetriggerPromptText
		// (§22.3): prepend this repo's own currently-active
		// learned false-positive patterns BEFORE the already-answered
		// facts below -- "injected into every review pass, first pass and
		// re-review alike": a manual re-trigger is exactly a re-review
		// pass. Independent of diffFetcher (a review turn can still carry
		// the advisory block even with no diff fetcher wired).
		if falsePositivePatterns != nil {
			if advisory := reviewcontext.FetchFalsePositivePatterns(ctx, logger, falsePositivePatterns, prSession.RepoFullName); advisory != "" {
				prompt = advisory + prompt
			}
		}
		// (§22.1)/(§22.1.2 retirement, this Step): prepend
		// this PR's own already-answered facts (open+rebutted
		// review_findings) BEFORE calling RenderTurnPrompt -- prepended
		// to, never replacing, the prose fallback above. This call is
		// DELIBERATELY placed AFTER prCtx is resolved below (moved by
		// this Step; it used to sit here, immediately after the
		// false-positive-patterns block above) so it can pass
		// prCtx.ChangedPaths through to FetchAlreadyAnswered for §22.1.2's
		// own out-of-diff retirement check -- see that function's own doc
		// comment for why reordering it after the diff fetch changes
		// nothing about correctness (neither read depends on the other)
		// while still producing byte-for-byte the SAME final prompt text
		// order as before this Step: the "prompt = X + prompt" prepend
		// idiom used throughout this handler means the LAST block
		// prepended ends up FIRST in the rendered prompt, so moving this
		// call later in EXECUTION order, while keeping it the LAST
		// prepend before RenderTurnPrompt, preserves its own existing
		// FIRST position in the text exactly.
		// reviewHeadSHA is
		// captured here and threaded into CreateTurnCore below via
		// CreateTurnOptions.ReviewHeadSHA -- persisted onto THIS turn's
		// own row (turns.review_head_sha, set once at creation), never
		// written to a shared, mutable per-(repo,PR) column a LATER,
		// unrelated turn's own context-fetch could overwrite (the
		// previous github_pr_sessions.pending_head_sha design this fix
		// replaces -- see migrations/000072_turns_review_head_sha.up.sql's
		// own doc comment for the full "why").
		// prCtx (§26.3) is hoisted to this outer scope -- the
		// WHOLE struct is needed further down to compute this manual
		// re-trigger's own light/deep triage decision, mirroring
		// internal/adapters/inbound/github's own identical hoist
		// (handler.go). review.PreFetchedContext{} (every field its own
		// honest zero value) is exactly what a nil diffFetcher, or a
		// repo_full_name that fails to split, degrades to --
		// internal/app/reviewtriage.ComputeDecision's own fail-open
		// posture already treats an all-zero Signals as "route light".
		// havePrCtx (D2's own fix) tracks whether prCtx below was actually
		// populated by a real Fetch call -- exactly the SAME condition
		// that used to gate the (now-moved) RenderTurnPrompt call inline,
		// preserved here so a nil diffFetcher or a repo_full_name that
		// fails to split keeps degrading identically to before this fix:
		// no RenderTurnPrompt call at all, prompt stays the plain fixed
		// text with no diff/verdict-tool-instructions block appended.
		var reviewHeadSHA *string
		var prCtx review.PreFetchedContext
		havePrCtx := false
		if diffFetcher != nil {
			if owner, repo, ok := reposource.SplitFullName(prSession.RepoFullName); ok {
				prCtx = reviewcontext.Fetch(ctx, logger, diffFetcher, timeouts, owner, repo, prSession.PrNumber, botToken, nil)
				havePrCtx = true
				if prCtx.HeadSHA != "" {
					reviewHeadSHA = &prCtx.HeadSHA
				}
			} else {
				logger.Warn("httpapi: could not split repo_full_name into owner/repo, skipping pre-fetched review context",
					"repo_full_name", prSession.RepoFullName, "pr_number", prSession.PrNumber)
			}
		}

		// (§26.3): the depth decision, computed from prCtx above.
		// Adversarial-review fix D2 ("deep-path digest requirement
		// contradicts the prompt the agent actually receives"): this MUST
		// run, and be floored (D1, immediately below), BEFORE
		// review.RenderTurnPrompt is called -- prCtx.DeepPath (set right
		// below) is the ONE fact that makes RenderTurnPrompt's own
		// verdictToolInstructions tell the agent the truth about whether
		// digest.archDecisions/stackRisks/unverifiedLimits are REQUIRED
		// for THIS turn. Getting this ordering wrong (as this handler used
		// to: rendering the prompt first, computing/flooring the depth
		// only afterward) is exactly what let an agent be told "requested,
		// not required" on a turn the server was about to persist -- and
		// later validate -- as deep.
		//
		// D1 (adversarial-review fix, "re-review depth floor applied at
		// only 1 of 3 lanes"): this handler used to feed the FRESH,
		// unfloored triageDecision.Depth straight through, never applying
		// §24's re-review floor (domainreviewtriage.Floor) at all -- a
		// single unfloored light re-review through this lane would
		// silently erase the floor's own guarantee for every OTHER lane
		// reading review_verdicts.review_path back (readReviewRetriggerState,
		// internal/app/sessionactor/reviewretrigger.go). ComputeDecision's
		// own third return value (priorReviewDepth) is the SAME
		// review_verdicts.GetLatest read that function already performs
		// for its "prior high verdict" signal -- no second query.
		//
		// This handler always targets an ALREADY-EXISTING session/PR (this
		// file's own top doc comment: "this handler never creates a
		// brand-new review session"), so Floor is applied unconditionally
		// here -- unlike coalesce.go's own WINNER branch, there is no
		// "brand-new session, nothing to floor against" case to skip it
		// for.
		//
		// D9 (adversarial-review fix): the SAME
		// ReasonAlwaysLightConfig guard internal/app/sessionactor/
		// reviewretrigger.go now applies -- an explicit admin
		// reviewDepth.mode=always_light override must outrank this
		// history-based floor here too, for the identical reason (see
		// domainreviewtriage.Floor's own doc comment, depth.go).
		triageDecision, triageConfig, priorReviewDepth := appreviewtriage.ComputeDecision(ctx, reviewTriageDeps, prSession.RepoFullName, prSession.PrNumber, prCtx)
		triageProvenance := appreviewtriage.ResolveProvenance(ctx, reviewTriageDeps, prSession.RepoFullName, prSession.PrNumber)
		flooredDepth := triageDecision.Depth
		if triageDecision.Reason != domainreviewtriage.ReasonAlwaysLightConfig {
			flooredDepth = domainreviewtriage.Floor(triageDecision.Depth, priorReviewDepth)
		}
		prCtx.DeepPath = flooredDepth == domainreviewtriage.DepthDeep
		// ReviewCostBudgetUSD (§26.7): the SAME triageConfig
		// ComputeDecision already resolved, above -- no second repo_settings
		// read. Read AFTER flooredDepth is known so a re-review that got
		// floored deep by §24's own "once deep, stays deep" rule correctly
		// gets the deep-path ceiling, never the fresh (possibly light)
		// triageDecision.Depth's own ceiling.
		prCtx.ReviewCostBudgetUSD = triageConfig.CostBudget.ForDepth(flooredDepth)
		// B5 fix: threads reviewtriage.CostBudgetSafetyMargin through as a
		// whole percentage -- review.PreFetchedContext.
		// CostBudgetSafetyMarginPercent's own doc comment for why this
		// package (which already imports reviewtriage) is the one that
		// must set it, never review itself (doc.go's own "zero external
		// imports" convention).
		prCtx.CostBudgetSafetyMarginPercent = int(domainreviewtriage.CostBudgetSafetyMargin * 100)
		// (§22.1)/(§22.1.2 retirement): see this
		// function's own earlier comment (where this block used to sit,
		// right after the false-positive-patterns block) for why it moved
		// here -- prCtx.ChangedPaths is only known now, after the diff
		// fetch above, and §22.1.2's retirement check needs it. This is
		// still the LAST block prepended before RenderTurnPrompt below,
		// so the final prompt text order is unchanged from before this
		// Step.
		if reviewFindings != nil {
			if alreadyAnswered := reviewcontext.FetchAlreadyAnswered(ctx, logger, reviewFindings, prSession.RepoFullName, prSession.PrNumber, prCtx.ChangedPaths, prCtx.DiffTruncated); alreadyAnswered != "" {
				prompt = alreadyAnswered + prompt
			}
		}
		if havePrCtx {
			prompt = review.RenderTurnPrompt(prompt, prCtx)
		}
		reviewDepthStr := string(flooredDepth)
		triageModelID, triageEffort := domainreviewtriage.ModelAndEffort(flooredDepth, reviewModelDeep)
		// D4 (nice-to-have adversarial-review fix): a deep-routed turn
		// whose deep-tier model override is inert (platform.Config.
		// ReviewModelDeep, an OPTIONAL env var, was never configured for
		// this deployment) gets ONLY the forced-high-effort half of
		// "deep = frontier tier + high effort" -- the model-tier half
		// silently does nothing. This is not a bug (ModelAndEffort's own
		// doc comment: the turn simply inherits today's default model),
		// but an operator otherwise has no signal that this is happening
		// at all.
		if flooredDepth == domainreviewtriage.DepthDeep && reviewModelDeep == "" {
			logger.Info("httpapi: review routed deep but no deep-tier model configured (NARVI_REVIEW_MODEL_DEEP unset), dispatching with the default model at forced high effort", "repo_full_name", prSession.RepoFullName, "pr_number", prSession.PrNumber)
		}
		// (§31.6): see internal/adapters/inbound/github/handler.go's
		// own identical addition for the full "why this carrier" reasoning.
		archDecisionTags := autoapproval.TagStrings(autoapproval.ClassifyChangedPaths(prCtx.ChangedPaths))
		archDecisionRoots := autoapproval.ClassifyChangedRoots(prCtx.ChangedPaths)
		triageRecordJSON, triageRecordErr := json.Marshal(domainreviewtriage.NewDecisionRecord(triageDecision, triageConfig, flooredDepth, triageProvenance, triageModelID, triageEffort, prCtx.ChangedFilesCount, prCtx.Diff == "", prCtx.DiffTruncated, archDecisionTags, archDecisionRoots))
		if triageRecordErr != nil {
			logger.Warn("httpapi: marshal review-depth decision record failed, turn will carry review_depth but no review_depth_decision", "error", triageRecordErr, "repo_full_name", prSession.RepoFullName, "pr_number", prSession.PrNumber)
			triageRecordJSON = nil
		}

		// AlwaysQueue, NOT CreateTurn's own RejectIfOpen -- see this file's
		// own top doc comment for why a manual re-review click is treated
		// exactly like another @mention on the same PR (coalesce.go's own
		// REUSE branch), never CreateTurn's own single-relaunch-at-a-time
		// REST policy.
		//
		// epistemicCheckDefault: hardcoded false, deliberately never the
		// operator's own platform.Config.EpistemicCheckDefault -- §20's
		// own devil's-advocate preamble (§20) is a BUILDER check ("domain/
		// turn: builder epistemic pre-action check", the Step's own title);
		// a review turn is never a build turn (this function creates
		// review-session turns, with review.RenderTurnPrompt's own
		// verdict-tool-instructions already appended above, an entirely
		// separate concern). Passing the real default here would leak the
		// preamble onto review turns with no code path ever exercising
		// ShouldInjectEpistemicPreamble's own planMode=false branch to stop
		// it (a review-retrigger turn is always planMode=false, never
		// true), so this call site is EXCLUDED at the source rather than
		// relying on some other gate downstream.
		//
		// intentSvc: literal nil, deliberately never a real
		// *intentclassifier.Service -- see this function's own top doc
		// comment ("No intentSvc parameter") for the full "why": a manual
		// re-review click carries no human reply for the plan_followup
		// classifier to legitimately read, so this path always falls open
		// to the safe, deterministic pre-existing "decline while a plan is
		// awaiting approval" behavior instead of guessing from
		// manualRetriggerPromptText/the pre-fetched diff.
		created, _, cerr := CreateTurnCore(ctx, pool, sessions, turns, plans, nil, auditLog, registry, sessionID, prompt, triageModelID, false, false, actorUserID, AlwaysQueue, CreateTurnOptions{ReviewHeadSHA: reviewHeadSHA, Effort: triageEffort, ReviewDepth: &reviewDepthStr, ReviewDepthDecision: triageRecordJSON})
		if cerr != nil {
			logger.Error("httpapi: retrigger review (create turn) failed", "status", cerr.Status, "message", cerr.Message)
			writeError(w, cerr.Status, cerr.Message)
			return
		}

		writeJSON(w, http.StatusCreated, restdtos.CreateTurnResponse{
			Id:     created.ID.String(),
			Status: restdtos.CreateTurnResponseStatus(created.Status),
		})
	}
}
