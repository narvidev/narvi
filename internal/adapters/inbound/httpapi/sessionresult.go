package httpapi

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"
	"golang.org/x/sync/errgroup"

	"github.com/narvidev/narvi/contracts/gen/go/restdtos"
	"github.com/narvidev/narvi/internal/adapters/outbound/postgres"
	"github.com/narvidev/narvi/internal/adapters/outbound/postgres/sqlcgen"
	"github.com/narvidev/narvi/internal/app/ports"
	"github.com/narvidev/narvi/internal/app/reviewfreshness"
	appreviewtriage "github.com/narvidev/narvi/internal/app/reviewtriage"
	appreviewverdict "github.com/narvidev/narvi/internal/app/reviewverdict"
	"github.com/narvidev/narvi/internal/app/sessionactor"
	"github.com/narvidev/narvi/internal/app/shadowscm"
	plandomain "github.com/narvidev/narvi/internal/domain/plan"
	"github.com/narvidev/narvi/internal/domain/reposource"
	"github.com/narvidev/narvi/internal/domain/reviewverdict"
	"github.com/narvidev/narvi/internal/domain/session"
	"github.com/narvidev/narvi/internal/domain/turn"
	"github.com/narvidev/narvi/internal/platform"
)

// resultSummaryMaxChars is the most characters (Unicode code points) a
// result's summary carries (row 182's owner decision D7): the last run's
// final text, cut here and marked truncated, so a result stays a bounded
// read and never grows into the transcript. The full text stays in the
// event history (GET /api/sessions/{sessionID}/events).
const resultSummaryMaxChars = 4000

// SessionResultDeps is what GET /api/sessions/{sessionID}/result reads:
// Postgres (Pool, for the one read-only snapshot, and the stores read in
// it), and the code host for the one live read, a verdict's freshness --
// SourceControl (nil when none is configured: freshness then reads
// unconfirmed) with Outbound, §12.5's GitHub outbound axis, whose bot
// credential the code-review view and the auto-merge worker read pull
// requests with (nil when GitHub outbound is off: freshness then reads
// unconfirmed exactly as with no SourceControl, and no read is attempted).
// Timeouts bounds each live call and all of them together
// (SessionResultLiveReadBudget), and holds the suggested-delay table (the
// SessionResultDelay* fields).
type SessionResultDeps struct {
	Pool           *pgxpool.Pool
	Sessions       *postgres.SessionStore
	Turns          *postgres.TurnStore
	Events         *postgres.EventStore
	Artifacts      *postgres.ArtifactStore
	PRSessions     *postgres.GitHubPRSessionStore
	ReviewVerdicts *postgres.ReviewVerdictStore
	SourceControl  ports.SourceControl
	Outbound       *platform.GitHubOutboundConfig
	Timeouts       platform.Timeouts
}

// freshnessDeps is the one place GetSessionResult's live freshness read is
// configured: with GitHub outbound off there is no bot credential to read
// a pull request with, so it gets no code host at all -- the SAME
// "unconfirmed, no live read" path a nil SourceControl already takes --
// rather than a code host called with an empty token.
func (deps SessionResultDeps) freshnessDeps() reviewfreshness.Deps {
	if deps.Outbound == nil {
		return reviewfreshness.Deps{Timeouts: deps.Timeouts}
	}
	return reviewfreshness.Deps{SourceControl: deps.SourceControl, Token: deps.Outbound.BotToken(), Timeouts: deps.Timeouts}
}

// GetSessionResult backs GET /api/sessions/{sessionID}/result (technical
// plan §43.20, row 182's result): what one session has produced, as
// restdtos.SessionOutcome -- its activity, its last run with a bounded
// summary, the pull requests it opened, the pull request it reviews, and
// each one's review verdict with that verdict's freshness, or its absence.
// It is also the twin of the narvi_get_session_result MCP tool.
//
// The same gate as GetSession (get.go) and GetSessionStatus, deliberately:
// signed in, 400 on a malformed id, 404 when the session does not exist,
// no per-session visibility beyond that, because this codebase has none.
// The verdict data it copies is what GET /api/sessions/{sessionID}/review
// shows every role (authz.ActionViewAnalytics), read the same way
// (GetLatestRecord, owner decision D10).
//
// Every stored fact comes from ONE repeatable-read, read-only transaction:
// the activity (the status route's own statement and derivation), the last
// run, its events, the pull request artifacts, the review claims, attempts
// and verdicts, and the activity of each review session behind the review
// of a pull request it opened -- one snapshot, so the summary is read
// inside the run's own window as the snapshot saw it, and a review state
// never mixes an attempt from one instant with a verdict from another.
// The transaction ends before anything else happens. Then, outside it,
// each assessed verdict's freshness is read live (reviewfreshness.Assess:
// the merge path's own live read and comparison), concurrently, each read
// bounded by the platform.Timeouts constants the merge path uses and all
// of them together by SessionResultLiveReadBudget; a failed, timed-out or
// out-of-budget read reports the verdict unconfirmed, never current (owner
// decision D6). The answer carries suggestedDelaySeconds
// (resultReadDelay), short while anything the result reports can still
// change on its own -- the session, or the review session behind the
// review of a pull request it opened.
//
// No single pull request record can fail the result: one that names no
// pull request the session opened -- a creation suppressed in shadow mode
// (shadowscm.IsSyntheticPRRef), or a row this build cannot read -- is
// reported in excludedPullRequests with why (readPRArtifact), and the rest
// of the result stands.
//
// The response carries no events: the summary is the last run's final
// text, read by plandomain.FinalText -- the one reader of a turn's final
// text, which the plan views use too -- and cut at resultSummaryMaxChars
// (owner decision D7).
func GetSessionResult(deps SessionResultDeps) http.HandlerFunc {
	bounds := statusBoundsFrom(deps.Timeouts)
	freshnessDeps := deps.freshnessDeps()
	return func(w http.ResponseWriter, r *http.Request) {
		sessionID, ok := parseSessionID(w, r)
		if !ok {
			return
		}
		ctx := platform.WithSessionID(r.Context(), sessionID.String())
		logger := platform.Logger(ctx)

		var snap resultSnapshot
		err := pgx.BeginTxFunc(ctx, deps.Pool, pgx.TxOptions{IsoLevel: pgx.RepeatableRead, AccessMode: pgx.ReadOnly}, func(tx pgx.Tx) error {
			var err error
			snap, err = readResultSnapshot(ctx, deps, tx, sessionID, bounds)
			return err
		})
		if err != nil {
			var unreadable *unreadableResultError
			switch {
			case errors.Is(err, pgx.ErrNoRows):
				writeError(w, http.StatusNotFound, "session not found")
			case ctx.Err() != nil:
				logger.Debug("httpapi: session result read ended with its request", "error", err)
				writeError(w, http.StatusServiceUnavailable, "request cancelled")
			case errors.As(err, &unreadable):
				logger.Error("httpapi: session result facts are unreadable", "error", unreadable.err)
				writeError(w, http.StatusInternalServerError, "internal error")
			default:
				logger.Error("httpapi: read session result failed", "error", err)
				writeError(w, http.StatusInternalServerError, "internal error")
			}
			return
		}

		// The live reads, outside the transaction: one per assessed
		// verdict, each writing only its own review's freshness, all of
		// them within one budget -- a read it cuts short reports its
		// verdict unconfirmed (reviewfreshness.ReasonOutOfTime).
		liveCtx, cancelLive := context.WithTimeout(ctx, deps.Timeouts.SessionResultLiveReadBudget)
		g, gctx := errgroup.WithContext(liveCtx)
		for _, pending := range snap.live {
			g.Go(func() error {
				pending.review.Freshness = freshnessDTO(reviewfreshness.Assess(gctx, freshnessDeps, pending.record, pending.pr))
				return nil
			})
		}
		_ = g.Wait() // no goroutine returns an error: Assess never fails
		cancelLive()

		readLive := len(snap.live) > 0 && deps.SourceControl != nil
		snap.outcome.SuggestedDelaySeconds = wholeSecondsRoundedUp(resultReadDelay(snap.activity, snap.reviewsSettled, readLive, deps.Timeouts))
		writeJSON(w, http.StatusOK, snap.outcome)
	}
}

// unreadableResultError marks a row this build could not decode -- this
// build's defect, not the database's -- so it is logged as such.
type unreadableResultError struct{ err error }

func (e *unreadableResultError) Error() string { return e.err.Error() }
func (e *unreadableResultError) Unwrap() error { return e.err }

// resultSnapshot is what the snapshot transaction read: the response with
// every stored fact filled in, the verdicts whose freshness is still to be
// read live, each pointing at the review it belongs to, and whether every
// other session behind a review it reports is settled (reviewSessionsSettled).
type resultSnapshot struct {
	outcome        *restdtos.SessionOutcome
	activity       session.Activity
	live           []pendingFreshness
	reviewsSettled bool
}

// pendingFreshness is one assessed verdict whose freshness is read live.
type pendingFreshness struct {
	review *restdtos.SessionOutcomeReview
	record reviewverdict.Record
	pr     reviewfreshness.PullRequest
}

// resultStores are the stores bound to the snapshot transaction.
type resultStores struct {
	sessions       *postgres.SessionStore
	turns          *postgres.TurnStore
	events         *postgres.EventStore
	artifacts      *postgres.ArtifactStore
	prSessions     *postgres.GitHubPRSessionStore
	reviewVerdicts *postgres.ReviewVerdictStore
}

// readResultSnapshot reads every stored fact of the result on tx.
func readResultSnapshot(ctx context.Context, deps SessionResultDeps, tx pgx.Tx, sessionID pgtype.UUID, bounds statusBounds) (resultSnapshot, error) {
	st := resultStores{
		sessions:       deps.Sessions.WithTx(tx),
		turns:          deps.Turns.WithTx(tx),
		events:         deps.Events.WithTx(tx),
		artifacts:      deps.Artifacts.WithTx(tx),
		prSessions:     deps.PRSessions.WithTx(tx),
		reviewVerdicts: deps.ReviewVerdicts.WithTx(tx),
	}

	// The activity: the status route's own statement and derivation, so the
	// result says exactly what the status would at this snapshot.
	facts, err := st.sessions.ActivityFacts(ctx, sessionID, sessionactor.ReviewAutoRetriggerBudget)
	if err != nil {
		return resultSnapshot{}, err
	}
	in, _, err := activityInput(facts, bounds)
	if err != nil {
		return resultSnapshot{}, &unreadableResultError{err: err}
	}
	sessionRow, err := st.sessions.Get(ctx, sessionID)
	if err != nil {
		return resultSnapshot{}, err
	}

	activity := session.DeriveActivity(in)
	outcome := &restdtos.SessionOutcome{
		SessionId:            sessionID.String(),
		Activity:             restdtos.SessionOutcomeActivity(activity),
		PullRequests:         []restdtos.SessionOutcomePullRequest{},
		ExcludedPullRequests: []restdtos.SessionOutcomeExcludedPullRequest{},
	}
	if outcome.LastRun, err = readLastRun(ctx, st, sessionID, facts); err != nil {
		return resultSnapshot{}, err
	}

	// The pull requests the session opened, oldest first -- and, apart,
	// every pull request record that names none, each said explicitly.
	artifacts, err := st.artifacts.ListForSession(ctx, sessionID)
	if err != nil {
		return resultSnapshot{}, err
	}
	var produced []producedPR
	for _, a := range artifacts {
		if a.Type != sqlcgen.ArtifactTypePr {
			continue
		}
		pr, excluded := readPRArtifact(a, sessionRow.Repos)
		if excluded != nil {
			if excluded.Kind == restdtos.SessionOutcomeExcludedPullRequestKindUnreadable {
				platform.Logger(ctx).Warn("httpapi: session result: a pull request artifact is unreadable, reported as such", "artifact_id", a.ID.String(), "reason", excluded.Reason)
			}
			outcome.ExcludedPullRequests = append(outcome.ExcludedPullRequests, *excluded)
			continue
		}
		produced = append(produced, pr)
	}

	var live []pendingFreshness
	var reviewers []pgtype.UUID
	outcome.PullRequests = make([]restdtos.SessionOutcomePullRequest, len(produced))
	for i, pr := range produced {
		read, err := readReview(ctx, st, pr.repoFullName, pr.number, nil)
		if err != nil {
			return resultSnapshot{}, err
		}
		outcome.PullRequests[i] = restdtos.SessionOutcomePullRequest{
			RepoFullName: pr.repoFullName,
			Number:       int(pr.number),
			Url:          pr.url,
			CreatedAt:    pr.createdAt,
			Review:       read.review,
		}
		if read.pending != nil {
			read.pending.review = &outcome.PullRequests[i].Review
			live = append(live, *read.pending)
		}
		if read.reviewer.Valid && read.reviewer != sessionID {
			reviewers = append(reviewers, read.reviewer)
		}
	}

	// The pull request this session is the review session of, if any.
	claim, err := st.prSessions.GetBySessionID(ctx, sessionID)
	switch {
	case errors.Is(err, pgx.ErrNoRows):
	case err != nil:
		return resultSnapshot{}, err
	default:
		// Its review session is this session: the activity above is its.
		read, err := readReview(ctx, st, claim.RepoFullName, claim.PrNumber, &claim)
		if err != nil {
			return resultSnapshot{}, err
		}
		outcome.ReviewedPullRequest = &restdtos.SessionOutcomeReviewedPullRequest{
			RepoFullName: claim.RepoFullName,
			Number:       int(claim.PrNumber),
			Review:       read.review,
		}
		if read.pending != nil {
			read.pending.review = &outcome.ReviewedPullRequest.Review
			live = append(live, *read.pending)
		}
	}

	reviewsSettled, err := reviewSessionsSettled(ctx, st, reviewers, bounds)
	if err != nil {
		return resultSnapshot{}, err
	}

	outcome.ReviewScope = reviewScope(outcome)
	return resultSnapshot{outcome: outcome, activity: activity, live: live, reviewsSettled: reviewsSettled}, nil
}

// reviewSessionsSettled reports whether every one of reviewers -- the
// review sessions behind the reviews of the pull requests this session
// opened -- is settled, each by its own activity: the status route's own
// statement and derivation (session.DeriveActivity), in this snapshot.
// One that is not can change its pull request's review with no new input
// -- an attempt queued or running (the review reads in_progress), a
// re-review scheduled, a delivery under way -- so the result can change on
// its own, and resultReadDelay suggests the short delay. A review session
// whose facts this build cannot read counts as not settled, logged at
// WARN: a hint errs toward a shorter wait, and never fails the result.
func reviewSessionsSettled(ctx context.Context, st resultStores, reviewers []pgtype.UUID, bounds statusBounds) (bool, error) {
	seen := make(map[pgtype.UUID]bool, len(reviewers))
	for _, reviewer := range reviewers {
		if seen[reviewer] {
			continue
		}
		seen[reviewer] = true
		facts, err := st.sessions.ActivityFacts(ctx, reviewer, sessionactor.ReviewAutoRetriggerBudget)
		switch {
		case errors.Is(err, pgx.ErrNoRows):
			// No such session any more: nothing of it can progress.
			continue
		case err != nil:
			return false, err
		}
		in, _, err := activityInput(facts, bounds)
		if err != nil {
			platform.Logger(ctx).Warn("httpapi: session result: a review session's activity is unreadable, its review counted as able to change", "review_session_id", reviewer.String(), "error", err)
			return false, nil
		}
		if !session.DeriveActivity(in).Settled() {
			return false, nil
		}
	}
	return true, nil
}

// reviewScope says which pull requests the result reports verdicts for:
// the one the session reviews, else the ones it opened, else none -- said
// explicitly, so an empty list never reads as a clean review. An excluded
// record has no verdict, so it never makes the scope produced.
func reviewScope(outcome *restdtos.SessionOutcome) restdtos.SessionOutcomeReviewScope {
	switch {
	case outcome.ReviewedPullRequest != nil:
		return restdtos.SessionOutcomeReviewScopeReviewed
	case len(outcome.PullRequests) > 0:
		return restdtos.SessionOutcomeReviewScopeProduced
	default:
		return restdtos.SessionOutcomeReviewScopeNone
	}
}

// readLastRun renders the facts row's last run -- the newest terminal turn
// -- with its turn row's timing, cost and mode, the status's own
// failure-reason rule, and its summary; nil when no turn has ended.
func readLastRun(ctx context.Context, st resultStores, sessionID pgtype.UUID, facts sqlcgen.GetSessionActivityFactsRow) (*restdtos.SessionOutcomeLastRun, error) {
	if !facts.LastRunTurnID.Valid {
		return nil, nil
	}
	turns, err := st.turns.ListForSession(ctx, sessionID)
	if err != nil {
		return nil, err
	}
	var run *sqlcgen.Turn
	for i := range turns {
		if turns[i].ID == facts.LastRunTurnID {
			run = &turns[i]
			break
		}
	}
	if run == nil {
		return nil, &unreadableResultError{err: fmt.Errorf("last run %s is not among the session's turns", facts.LastRunTurnID.String())}
	}

	out := &restdtos.SessionOutcomeLastRun{
		TurnId:   run.ID.String(),
		Outcome:  restdtos.SessionOutcomeLastRunOutcome(run.Status),
		PlanMode: run.PlanMode,
	}
	if reason := lastRunFailureReasonValue(facts); reason != nil {
		out.FailureReason = &restdtos.SessionOutcomeLastRunFailureReason{Value: *reason}
	}
	if run.DispatchedAt.Valid {
		at := run.DispatchedAt.Time
		out.StartedAt = &at
	}
	if run.CompletedAt.Valid {
		at := run.CompletedAt.Time
		out.FinishedAt = &at
	}
	if cost, ok := appreviewtriage.NumericToFloat64(run.CostUsd); ok {
		out.CostUsd = &cost
	}

	// The summary: the run's final text within its own window of the event
	// log (turnContentBounds: its dispatch watermark, up to the next
	// dispatched turn's), read by the one reader of a turn's final text. A
	// run never dispatched has no window, and no text.
	if lower, upper, ok := turnContentBounds(turns, run.ID); ok {
		events, err := st.events.ListRecentForSession(ctx, sessionID, planContentEventFetchLimit)
		if err != nil {
			return nil, err
		}
		if text, found := plandomain.FinalText(sessionactor.ToContentEvents(events), lower, upper); found {
			capped, truncated := capSummary(text)
			out.Summary = restdtos.SessionOutcomeLastRunSummary{Text: &capped, Truncated: truncated}
		}
	}
	return out, nil
}

// capSummary cuts text to at most resultSummaryMaxChars characters (code
// points, never inside one), reporting whether it cut anything.
func capSummary(text string) (string, bool) {
	count := 0
	for i := range text {
		if count == resultSummaryMaxChars {
			return text[:i], true
		}
		count++
	}
	return text, false
}

// reviewRead is one pull request's review as readReview read it: the
// review, the verdict whose freshness is still to be read live (nil when
// none is), and the review session the claim names (invalid when there is
// no claim, or it names no session).
type reviewRead struct {
	review   restdtos.SessionOutcomeReview
	pending  *pendingFreshness
	reviewer pgtype.UUID
}

// readReview reads one pull request's review state from the record --
// its review session's claim (claim, when the caller already holds it,
// else looked up), that session's newest review attempt, whether that
// attempt posted, and the latest verdict (GetLatestRecord, the review
// readout's own read) -- and renders it. A verdict that is assessed and
// whose pull request the claim does not record as merged is returned as
// pending: its freshness is read live after the snapshot.
func readReview(ctx context.Context, st resultStores, repoFullName string, number int32, claim *sqlcgen.GithubPrSession) (reviewRead, error) {
	if claim == nil {
		row, err := st.prSessions.GetByRepoAndPRNumber(ctx, repoFullName, number)
		switch {
		case errors.Is(err, pgx.ErrNoRows):
		case err != nil:
			return reviewRead{}, err
		default:
			claim = &row
		}
	}
	var reviewer pgtype.UUID
	if claim != nil {
		reviewer = claim.SessionID
	}

	var attempt *reviewverdict.Attempt
	if claim != nil && claim.SessionID.Valid {
		row, err := st.turns.NewestReviewAttempt(ctx, claim.SessionID)
		switch {
		case errors.Is(err, pgx.ErrNoRows):
		case err != nil:
			return reviewRead{}, err
		default:
			attempt = &reviewverdict.Attempt{ID: row.ID.String(), Terminal: turn.IsTerminal(turn.State(row.Status))}
			if attempt.Terminal {
				if attempt.Posted, err = st.reviewVerdicts.ExistsForAttempt(ctx, row.ID); err != nil {
					return reviewRead{}, err
				}
			}
		}
	}

	var latest *reviewverdict.Record
	record, ok, err := appreviewverdict.GetLatestRecord(ctx, appreviewverdict.Deps{ReviewVerdicts: st.reviewVerdicts}, repoFullName, number)
	if err != nil {
		return reviewRead{}, err
	}
	if ok {
		latest = &record
	}

	status := reviewverdict.DeriveReviewStatus(latest, attempt)
	review := restdtos.SessionOutcomeReview{
		State:     restdtos.SessionOutcomeReviewState(status.State),
		Freshness: freshnessDTO(reviewfreshness.Assessment{State: reviewfreshness.StateNotApplicable}),
	}
	if status.Verdict != nil {
		v := restdtos.SessionOutcomeReviewVerdict(verdictDTO(*status.Verdict))
		review.Verdict = &v
	}
	if status.Superseded != nil {
		v := restdtos.SessionOutcomeReviewSupersededVerdict(verdictDTO(*status.Superseded))
		review.SupersededVerdict = &v
	}
	if status.State != reviewverdict.ReviewAssessed {
		return reviewRead{review: review, reviewer: reviewer}, nil
	}
	if mergedPerClaim(claim) {
		review.Freshness = freshnessDTO(reviewfreshness.Assessment{State: reviewfreshness.StateNotApplicable, Reason: reasonMergedPerClaim})
		return reviewRead{review: review, reviewer: reviewer}, nil
	}
	owner, repo, ok := reposource.SplitFullName(repoFullName)
	if !ok {
		return reviewRead{}, &unreadableResultError{err: fmt.Errorf("pull request repo %q is not owner/repo", repoFullName)}
	}
	return reviewRead{review: review, reviewer: reviewer, pending: &pendingFreshness{record: *status.Verdict, pr: reviewfreshness.PullRequest{Owner: owner, Repo: repo, Number: int(number)}}}, nil
}

// reasonMergedPerClaim is why a merged pull request's verdict freshness is
// not applicable.
const reasonMergedPerClaim = "the pull request has been merged"

// mergedPerClaim reports a pull request the claim records as merged: its
// verdict's freshness is then moot, decided with no live read. A merge is
// the one outcome the record can decide, because a merged pull request
// cannot be reopened. The claim's closed stamp cannot: the closed webhook
// writes it once and nothing clears it when the pull request is reopened
// (RecordMergeOutcome is its only writer), so it says the pull request was
// closed at some point, not that it is closed now. A pull request closed
// without merging is read live like any other, and the live read says so
// when it really is no longer open (reviewfreshness.ReasonNoLongerOpen).
func mergedPerClaim(claim *sqlcgen.GithubPrSession) bool {
	return claim != nil && claim.PrMerged != nil && *claim.PrMerged
}

// freshnessDTO renders an Assessment; an empty reason is null.
func freshnessDTO(a reviewfreshness.Assessment) restdtos.SessionOutcomeReviewFreshness {
	out := restdtos.SessionOutcomeReviewFreshness{State: restdtos.SessionOutcomeReviewFreshnessState(a.State)}
	if a.Reason != "" {
		reason := a.Reason
		out.Reason = &reason
	}
	return out
}

// resultReadDelay is the suggested delay before a client reads a result
// again, from platform.Timeouts' SessionResultDelay* table. While the
// result can still change on its own -- the session is not settled, or
// reviewsSettled is false: a review session behind a review it reports is
// not (an attempt queued or running, whose review reads in_progress; a
// re-review scheduled) -- the unsettled value: for the session, its status
// is the cheap way to watch it. Otherwise, a result that read a verdict's
// freshness live changes only when a pull request or its base moves, and
// asks the code host again on every read; one that read nothing live
// changes only with new input -- a prompt, a review requested, a push.
// Clamped to [SessionResultDelayFloor, SessionResultDelayCeiling] -- a
// defense: Validate keeps every table value inside them.
func resultReadDelay(activity session.Activity, reviewsSettled, readLive bool, t platform.Timeouts) time.Duration {
	var d time.Duration
	switch {
	case !activity.Settled() || !reviewsSettled:
		d = t.SessionResultDelayUnsettled
	case readLive:
		d = t.SessionResultDelayLiveRead
	default:
		d = t.SessionResultDelaySettled
	}
	return min(max(d, t.SessionResultDelayFloor), t.SessionResultDelayCeiling)
}

// verdictDTO copies one verdict record; a context field never recorded is
// null.
func verdictDTO(rec reviewverdict.Record) restdtos.SessionOutcomeVerdict {
	out := restdtos.SessionOutcomeVerdict{
		VerdictId: rec.ID,
		HeadSha:   rec.HeadSHA,
		RiskLevel: restdtos.SessionOutcomeVerdictRiskLevel(rec.Verdict.RiskLevel),
		Shippable: restdtos.SessionOutcomeVerdictShippable(rec.Verdict.Shippable),
		PostedAt:  rec.CreatedAt,
		Context: restdtos.SessionOutcomeVerdictContext{
			PolicyVersion:       rec.Context.PolicyVersion,
			AncestorChainLength: len(rec.Context.AncestorChain),
		},
	}
	if rec.AttemptID != "" {
		attempt := rec.AttemptID
		out.AttemptId = &attempt
	}
	if rec.Context.BaseRef != "" {
		ref := rec.Context.BaseRef
		out.Context.BaseRef = &ref
	}
	if rec.Context.BaseSHA != "" {
		sha := rec.Context.BaseSHA
		out.Context.BaseSha = &sha
	}
	return out
}

// producedPR is one pull request artifact, resolved.
type producedPR struct {
	repoFullName string
	number       int32
	url          string
	createdAt    time.Time
}

// prArtifactMetadata is what the session actor records on a pull request
// artifact (sessionactor.recordPRArtifact): the repository's bare name, as
// reposource.ParseOwnerRepo read it from the session's own repo URL, and
// the number.
type prArtifactMetadata struct {
	Repo   string `json:"repo"`
	Number int32  `json:"number"`
}

// The reasons a pull request artifact is excluded from pullRequests.
const (
	reasonShadowSuppressed      = "outbound writes to this repository were in shadow mode when the session tried to open this pull request, so its creation was recorded and suppressed: no pull request was created"
	reasonArtifactMetadata      = "the record's metadata could not be decoded"
	reasonArtifactNoPullRequest = "the record names no repository and pull request number"
)

// readPRArtifact reads one pull request artifact: the pull request the
// session opened (producedPRFromArtifact), or -- never an error that fails
// the result -- why it names none, for excludedPullRequests:
//
//   - shadow_suppressed: the ref a suppressed creation hands back, which the
//     session actor records like any other (sessionactor's
//     createPRBestEffort: "the direct trace of this one hop"), recognised by
//     shadowscm.IsSyntheticPRRef, the one predicate every layer that
//     suppresses agrees on (internal/domain/shadowsentinel). No pull request
//     exists: its URL points nowhere and is not reported.
//   - unreadable: a row this build cannot resolve, with the reason.
func readPRArtifact(a sqlcgen.Artifact, sessionRepos []byte) (producedPR, *restdtos.SessionOutcomeExcludedPullRequest) {
	meta, ok := decodePRArtifactMetadata(a)
	if !ok {
		return producedPR{}, unreadablePRArtifact(a, reasonArtifactMetadata)
	}
	if shadowscm.IsSyntheticPRRef(ports.PRRef{Number: int(meta.Number), URL: a.Url}) {
		out := &restdtos.SessionOutcomeExcludedPullRequest{
			Kind:      restdtos.SessionOutcomeExcludedPullRequestKindShadowSuppressed,
			CreatedAt: a.CreatedAt.Time,
			Reason:    reasonShadowSuppressed,
		}
		if owners := sessionRepoOwners(meta.Repo, sessionRepos); meta.Repo != "" && len(owners) == 1 {
			for owner := range owners {
				full := owner + "/" + meta.Repo
				out.RepoFullName = &full
			}
		}
		return producedPR{}, out
	}
	pr, reason := producedPRFromArtifact(a, meta, sessionRepos)
	if reason != "" {
		return producedPR{}, unreadablePRArtifact(a, reason)
	}
	return pr, nil
}

// unreadablePRArtifact reports a pull request artifact this build cannot
// resolve, with the URL it holds (null when it holds none).
func unreadablePRArtifact(a sqlcgen.Artifact, reason string) *restdtos.SessionOutcomeExcludedPullRequest {
	out := &restdtos.SessionOutcomeExcludedPullRequest{
		Kind:      restdtos.SessionOutcomeExcludedPullRequestKindUnreadable,
		CreatedAt: a.CreatedAt.Time,
		Reason:    reason,
	}
	if a.Url != "" {
		u := a.Url
		out.Url = &u
	}
	return out
}

// decodePRArtifactMetadata decodes an artifact's metadata; none at all is
// the zero value.
func decodePRArtifactMetadata(a sqlcgen.Artifact) (prArtifactMetadata, bool) {
	var meta prArtifactMetadata
	if len(a.Metadata) > 0 {
		if err := json.Unmarshal(a.Metadata, &meta); err != nil {
			return prArtifactMetadata{}, false
		}
	}
	return meta, true
}

// sessionRepoOwners is the set of owners of the session's repositories
// named name, each read by reposource.ParseOwnerRepo over its URL -- the
// exact derivation that opened the pull request.
func sessionRepoOwners(name string, sessionRepos []byte) map[string]bool {
	owners := map[string]bool{}
	for _, repo := range decodeSessionRepos(sessionRepos) {
		if owner, repoName, err := reposource.ParseOwnerRepo(repo.Url); err == nil && repoName == name {
			owners[owner] = true
		}
	}
	return owners
}

// producedPRFromArtifact resolves a pull request artifact whose metadata
// is meta. The artifact records the repository's bare name only; its owner
// is that of the session's own repository of that name (sessionRepoOwners)
// when exactly one of the session's repositories has it, else the one the
// pull request's URL names (github.com/owner/repo/pull/N). The number is
// the artifact's, else the URL's. A non-empty reason says why it names no
// pull request this build can resolve.
func producedPRFromArtifact(a sqlcgen.Artifact, meta prArtifactMetadata, sessionRepos []byte) (producedPR, string) {
	urlOwner, urlRepo, urlNumber, urlOK := parsePRURL(a.Url)
	if meta.Repo == "" && urlOK {
		meta.Repo = urlRepo
	}
	if meta.Number <= 0 && urlOK {
		meta.Number = urlNumber
	}
	if meta.Repo == "" || meta.Number <= 0 {
		return producedPR{}, reasonArtifactNoPullRequest
	}

	owners := sessionRepoOwners(meta.Repo, sessionRepos)
	var owner string
	switch {
	case len(owners) == 1:
		for o := range owners {
			owner = o
		}
	case urlOK && urlRepo == meta.Repo:
		owner = urlOwner
	default:
		return producedPR{}, fmt.Sprintf("the owner of repository %q is named by neither the session's repositories nor the record's URL", meta.Repo)
	}
	return producedPR{
		repoFullName: owner + "/" + meta.Repo,
		number:       meta.Number,
		url:          a.Url,
		createdAt:    a.CreatedAt.Time,
	}, ""
}

// parsePRURL reads owner, repo and number from a pull request URL of the
// form https://<host>/<owner>/<repo>/pull/<number>.
func parsePRURL(raw string) (owner, repo string, number int32, ok bool) {
	u, err := url.Parse(raw)
	if err != nil {
		return "", "", 0, false
	}
	parts := strings.Split(strings.Trim(u.Path, "/"), "/")
	if len(parts) != 4 || parts[0] == "" || parts[1] == "" || parts[2] != "pull" {
		return "", "", 0, false
	}
	n, err := strconv.ParseInt(parts[3], 10, 32)
	if err != nil || n <= 0 {
		return "", "", 0, false
	}
	return parts[0], parts[1], int32(n), true
}
