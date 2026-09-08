// This file (reviewreadout.go) implements §26.1's own merge-readout read
// model: GET /api/sessions/{sessionID}/review (§12.2 item 2's Code review
// view). Before this Step, nothing in this package ever read back a
// posted verdict for the browser -- PostReviewVerdict (reviewverdict.go)
// is a sandbox-bearer-authenticated WRITE endpoint the reviewing agent
// calls, never something the UI itself fetches from. This is that read
// endpoint: the latest verdict + digest (§26.1), every finding ever
// posted for the PR with its position RE-RESOLVED at read time (§22.1.1/
// §22.5's own instruction: re-resolve each finding's position by
// refetching the diff at the verdict's own persisted head_sha and
// re-running the position match, never a stored, potentially-stale line
// number), a bounded verdict history (§26.1 item 5), and the authoring
// session's own epistemic heads-up signal (§20.1/§20.2).
//
// Gated by the EXISTING authz.ActionViewAnalytics (§13.3 row 1: every
// role including viewer, read-only) -- mirrors reviewanalytics.go's own
// identical choice: this is a read surface, not an edit one, so it needs
// no narrower gate than "signed in".
//
// Every third-party-authored string this handler surfaces (digest
// summary/arch decisions/stack risks, finding descriptions/file paths/
// suggested fixes, rebuttal text, the PR's own title) is forwarded
// VERBATIM, never re-escaped or transformed here -- the SPA's own
// rendering layer (React's default text escaping) is the only place any
// of this ever reaches markup, exactly like every other REST handler in
// this package that forwards untrusted review content (reviewfindings.go's
// own findingToWire).

package httpapi

import (
	"context"
	"errors"
	"net/http"
	"strings"

	"github.com/jackc/pgx/v5"

	"github.com/narvidev/narvi/contracts/gen/go/restdtos"
	"github.com/narvidev/narvi/internal/adapters/outbound/githubapi"
	"github.com/narvidev/narvi/internal/adapters/outbound/postgres"
	"github.com/narvidev/narvi/internal/adapters/outbound/postgres/sqlcgen"
	"github.com/narvidev/narvi/internal/app/findingposition"
	"github.com/narvidev/narvi/internal/app/reviewcontext"
	appreviewverdict "github.com/narvidev/narvi/internal/app/reviewverdict"
	"github.com/narvidev/narvi/internal/domain/authz"
	"github.com/narvidev/narvi/internal/domain/review"
	"github.com/narvidev/narvi/internal/domain/reviewpost"
	"github.com/narvidev/narvi/internal/domain/reviewverdict"
	"github.com/narvidev/narvi/internal/domain/turn"
	"github.com/narvidev/narvi/internal/platform"
)

// errNilReviewContextFetcher is GetReviewReadout's own internal "fetcher
// is nil" signal -- never returned to a caller, only logged (the exact
// same degrade-to-null treatment a real fetch error gets, see this
// function's own doc comment). A production deployment always wires a
// real fetcher (platform.Load's own boot-required GitHub ingress) --
// this exists for a legitimately unconfigured/test caller, mirroring
// every other reviewcontext.Fetcher caller's own explicit nil guard.
var errNilReviewContextFetcher = errors.New("httpapi: review readout fetcher is nil")

// GetReviewReadout backs GET /api/sessions/{sessionID}/review. 404 if
// sessionID doesn't exist; 400 if it exists but was never created via a
// GitHub PR mention (no PR to read a review for); 403 if the caller fails
// authz.ActionViewAnalytics; otherwise 200 with restdtos.ReviewReadout,
// even when no verdict has ever been posted yet (latestVerdict null,
// findings/history empty -- an honest "not reviewed yet" state, never a
// 404, mirroring GetReviewAnalytics' own "a repo/PR with no data yet
// renders, it just renders empty" posture).
func GetReviewReadout(
	sessions *postgres.SessionStore,
	prSessions *postgres.GitHubPRSessionStore,
	reviewVerdictDeps appreviewverdict.Deps,
	reviewFindings *postgres.ReviewFindingStore,
	turns *postgres.TurnStore,
	fetcher reviewcontext.Fetcher,
	relocationResolver *findingposition.Resolver,
	sentinelFixes *postgres.SentinelFixStore,
	handoffSentinelRuns *postgres.HandoffSentinelStore,
	botToken string,
	timeouts platform.Timeouts,
) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		sessionID, ok := parseSessionID(w, r)
		if !ok {
			return
		}
		ctx := platform.WithSessionID(r.Context(), sessionID.String())
		r = r.WithContext(ctx)
		logger := platform.Logger(ctx)

		if _, err := sessions.Get(ctx, sessionID); err != nil {
			if errors.Is(err, pgx.ErrNoRows) {
				writeError(w, http.StatusNotFound, "session not found")
				return
			}
			logger.Error("httpapi: get session for review readout failed", "error", err)
			writeError(w, http.StatusInternalServerError, "internal error")
			return
		}

		if !authorize(w, r, authz.ActionViewAnalytics, authz.Resource{}) {
			return
		}

		prSession, err := prSessions.GetBySessionID(ctx, sessionID)
		if err != nil {
			if errors.Is(err, pgx.ErrNoRows) {
				writeError(w, http.StatusBadRequest, "this session has no associated GitHub pull request")
				return
			}
			logger.Error("httpapi: look up github_pr_sessions by session id failed", "error", err)
			writeError(w, http.StatusInternalServerError, "internal error")
			return
		}
		repoFullName := prSession.RepoFullName
		prNumber := prSession.PrNumber
		owner, repo, ok := splitRepoFullName(repoFullName)
		if !ok {
			logger.Error("httpapi: malformed repo_full_name on github_pr_sessions row", "repo_full_name", repoFullName)
			writeError(w, http.StatusInternalServerError, "internal error")
			return
		}

		resp := restdtos.ReviewReadout{
			RepoFullName: repoFullName,
			PrNumber:     int(prNumber),
			Findings:     []restdtos.ReviewReadoutFinding{},
			History:      []restdtos.ReviewVerdictHistoryEntry{},
			// SessionReuse (§12.2 item 2's own "coalesced-mention/
			// session-reuse info" gap): never null -- prSession above is
			// this exact PR's own github_pr_sessions claim row, which
			// must exist (the pgx.ErrNoRows branch just above already
			// returned) and therefore carries a real mention_count/
			// claimed_at by construction.
			SessionReuse: restdtos.ReviewReadoutSessionReuse{
				MentionCount: int(prSession.MentionCount),
				ClaimedAt:    prSession.ClaimedAt.Time,
			},
		}

		// Live GitHub read for title/state/labels -- best-effort, degrades
		// to null on failure (reviewcontext.Fetch's own established
		// posture), never fatal to this endpoint. VisualQa (§12.2 item 2's
		// own "visual-QA sentinel status" gap) rides this SAME call --
		// never a second outbound request just to re-read labels. fetcher
		// == nil degrades identically to a live-fetch error -- mirrors
		// every other reviewcontext.Fetcher caller's own explicit nil
		// guard (internal/app/reviewcontext's own alreadyanswered.go/
		// archdecisions.go/falsepositive.go, reviewretrigger.go's own
		// documented "diffFetcher == nil... nil-safe" contract) -- a
		// production deployment always wires a real adapter here
		// (platform.Load's own boot-required GitHub ingress), so this
		// guard exists for the SAME reason those callers' does: a
		// legitimately unconfigured/test caller must degrade, never panic.
		var pr githubapi.PullRequest
		var prErr error
		if fetcher == nil {
			prErr = errNilReviewContextFetcher
		} else {
			prCtx, cancel := context.WithTimeout(ctx, timeouts.GitHubGetPRTimeout)
			pr, prErr = fetcher.GetPullRequest(prCtx, owner, repo, prNumber, botToken)
			cancel()
		}
		if prErr != nil {
			logger.Warn("httpapi: live GetPullRequest for review readout failed, rendering title/visualQa as unavailable", "error", prErr)
		} else {
			title := pr.Title
			resp.PrTitle = &title
			// prState is deliberately never populated: *githubapi.
			// PullRequest carries no state/merged field at all today
			// (adapter.go's own PullRequest struct) -- stays null rather
			// than a guessed value, mirroring the degraded-fetch case
			// immediately above.
			resp.VisualQa = visualQaFromLabels(pr.Labels)
		}

		if sentinelFix, sfErr := sentinelFixes.Get(ctx, repoFullName, prNumber); sfErr != nil {
			if !errors.Is(sfErr, pgx.ErrNoRows) {
				logger.Error("httpapi: get sentinel fix for review readout failed", "error", sfErr)
				writeError(w, http.StatusInternalServerError, "internal error")
				return
			}
			// pgx.ErrNoRows: no sentinel-fix was ever triggered for this PR
			// -- resp.SentinelFix stays nil, its own honest zero value.
		} else {
			resp.SentinelFix = sentinelFixToWire(sentinelFix)
		}

		if handoffRun, hErr := handoffSentinelRuns.Get(ctx, repoFullName, prNumber); hErr != nil {
			if !errors.Is(hErr, pgx.ErrNoRows) {
				logger.Error("httpapi: get handoff sentinel run for review readout failed", "error", hErr)
				writeError(w, http.StatusInternalServerError, "internal error")
				return
			}
			// pgx.ErrNoRows: the handoff-readiness sentinel never flagged
			// this PR -- resp.HandoffReadiness stays nil.
		} else {
			resp.HandoffReadiness = handoffReadinessToWire(handoffRun)
		}

		latest, hasLatest, err := appreviewverdict.GetLatestRecord(ctx, reviewVerdictDeps, repoFullName, prNumber)
		if err != nil {
			logger.Error("httpapi: get latest review verdict failed", "error", err)
			writeError(w, http.StatusInternalServerError, "internal error")
			return
		}
		if hasLatest {
			wire := reviewReadoutVerdictToWire(latest)
			// SessionId: the URL's own sessionID, never latest's own
			// (unavailable -- reviewverdict.Record carries no session_id
			// field, review/doc.go's own established "an envelope around
			// only what analytics rollups need" scope). Provably correct
			// regardless: prSessions.GetBySessionID above already
			// confirmed THIS session owns (repoFullName, prNumber) via
			// github_pr_sessions' own per-PR atomic claim (§8.2) -- at
			// most one session ever owns a given PR, so the review
			// session that posted this PR's latest verdict IS this one.
			wire.SessionId = sessionID.String()
			resp.LatestVerdict = &wire
		}

		history, err := appreviewverdict.ListRecordsForPR(ctx, reviewVerdictDeps, repoFullName, prNumber)
		if err != nil {
			logger.Error("httpapi: list review verdict history failed", "error", err)
			writeError(w, http.StatusInternalServerError, "internal error")
			return
		}
		for _, rec := range history {
			resp.History = append(resp.History, restdtos.ReviewVerdictHistoryEntry{
				PostedAt:  rec.CreatedAt,
				RiskLevel: restdtos.ReviewVerdictHistoryEntryRiskLevel(rec.Verdict.RiskLevel),
				Shippable: restdtos.ReviewVerdictHistoryEntryShippable(rec.Verdict.Shippable),
				HeadSha:   rec.HeadSHA,
			})
		}

		findingRows, err := reviewFindings.ListAllForPR(ctx, repoFullName, prNumber)
		if err != nil {
			logger.Error("httpapi: list review findings failed", "error", err)
			writeError(w, http.StatusInternalServerError, "internal error")
			return
		}
		findings := make([]reviewpost.Finding, len(findingRows))
		for i, row := range findingRows {
			findings[i] = findingFromRow(row)
		}
		// §22.1.1/§22.5: re-resolve every finding's position against the
		// diff at the LATEST verdict's own head_sha -- refetched fresh
		// here, never a stored line number. Only meaningful when a verdict
		// exists at all; diff == "" (hasLatest false, or the refetch
		// itself failing) leaves every finding unanchored (0, 0), exactly
		// findingposition.ResolveAll's own documented degradation.
		var diff string
		if hasLatest && latest.HeadSHA != "" {
			if d, ok := reviewcontext.FetchDiffAt(ctx, logger, fetcher, timeouts, owner, repo, prNumber, botToken, latest.HeadSHA); ok {
				diff = d
			}
		}
		findings = findingposition.ResolveAll(ctx, relocationResolver, findings, diff, timeouts)
		for i, f := range findings {
			resp.Findings = append(resp.Findings, findingToReadoutWire(f, findingRows[i]))
		}

		// §20's builder epistemic-check heads-up, surfaced as a subtle
		// "Heads-up" indicator when minor/strong fired -- the most recent
		// non-'none' outcome any turn on THIS session ever reported.
		// turns.epistemic_outcome is a
		// build-turn-only mechanism (§20.2); a pure review session simply
		// never sets it, degrading this to null exactly like "never
		// recorded" -- indistinguishable, by design (this field's own
		// schema doc comment).
		if turnRows, err := turns.ListForSession(ctx, sessionID); err != nil {
			logger.Warn("httpapi: list turns for epistemic heads-up failed, omitting indicator", "error", err)
		} else {
			for _, t := range turnRows {
				if t.EpistemicOutcome == nil {
					continue
				}
				outcome := string(*t.EpistemicOutcome)
				if outcome == string(turn.EpistemicOutcomeNone) {
					continue
				}
				o := outcome
				resp.EpistemicOutcome = &o
			}
		}

		writeJSON(w, http.StatusOK, resp)
	}
}

// visualQaLabelPrefix is the GitHub label prefix a human applies to
// record this PR's own visual-QA outcome (§8's own "visual-qa: pass/skip"
// criterion, §12.2 item 2's own gap) -- Narvi never writes this label
// itself, only reads it back.
const visualQaLabelPrefix = "visual-qa:"

// visualQaFromLabels scans labelNames (a live PR's own current GitHub
// labels) for one starting with visualQaLabelPrefix and returns the text
// after the colon, trimmed of surrounding whitespace (a human might type
// "visual-qa: pass" or "visual-qa:pass" -- both are the same label in
// intent) -- nil when no such label is present. The first match wins;
// nothing here validates the suffix against a fixed vocabulary, since
// this is human-authored external text Narvi does not own.
func visualQaFromLabels(labelNames []string) *string {
	for _, name := range labelNames {
		if !strings.HasPrefix(name, visualQaLabelPrefix) {
			continue
		}
		value := strings.TrimSpace(strings.TrimPrefix(name, visualQaLabelPrefix))
		return &value
	}
	return nil
}

// sentinelFixToWire converts one sqlcgen.SentinelFix row into
// restdtos.ReviewReadoutSentinelFix -- §12.2 item 2's own "sentinel
// auto-fix PR link with its merge-gated state" gap.
func sentinelFixToWire(row sqlcgen.SentinelFix) *restdtos.ReviewReadoutSentinelFix {
	var fixPRNumber *int
	if row.FixPrNumber != nil {
		n := int(*row.FixPrNumber)
		fixPRNumber = &n
	}
	return &restdtos.ReviewReadoutSentinelFix{
		Status:          row.Status,
		FixPrNumber:     fixPRNumber,
		StackRegistered: row.StackRegistered,
	}
}

// handoffReadinessToWire converts one sqlcgen.HandoffSentinelRun row into
// restdtos.ReviewReadoutHandoffReadiness -- §12.2 item 2's own
// "handoff-readiness display" gap.
func handoffReadinessToWire(row sqlcgen.HandoffSentinelRun) *restdtos.ReviewReadoutHandoffReadiness {
	return &restdtos.ReviewReadoutHandoffReadiness{
		ContractDriftFlagged: row.ContractDriftFlagged,
		TodoCount:            int(row.TodoCount),
		FlaggedAt:            row.CreatedAt.Time,
	}
}

// splitRepoFullName splits "owner/repo" into its two parts -- ok=false on
// anything not matching that exact shape (github_pr_sessions.repo_full_name
// is always written in this form by this codebase's own GitHub ingress,
// so a mismatch here means a data integrity problem, never a legitimate
// input to degrade gracefully from).
func splitRepoFullName(repoFullName string) (owner, repo string, ok bool) {
	parts := strings.SplitN(repoFullName, "/", 2)
	if len(parts) != 2 || parts[0] == "" || parts[1] == "" {
		return "", "", false
	}
	return parts[0], parts[1], true
}

// findingFromRow converts one sqlcgen.ReviewFinding row into
// reviewpost.Finding -- field-by-field, never through BuildFinding (which
// RECOMPUTES IdentityHash; this row's own persisted hash is authoritative
// and must be forwarded verbatim, never re-derived).
func findingFromRow(row sqlcgen.ReviewFinding) reviewpost.Finding {
	f := reviewpost.Finding{
		IdentityHash: row.IdentityHash,
		Severity:     review.RiskLevel(row.Severity),
		FilePath:     row.FilePath,
		Description:  row.Description,
		SuggestedFix: row.SuggestedFix,
	}
	if row.SentinelKind != nil {
		k := reviewpost.SentinelKind(*row.SentinelKind)
		f.SentinelKind = &k
	}
	if row.Line != nil {
		line := int(*row.Line)
		f.Line = &line
	}
	return f
}

// findingToReadoutWire converts one position-resolved reviewpost.Finding
// (f) plus its own source sqlcgen.ReviewFinding row (row -- for the
// lifecycle fields ResolveAll never touches: status/rebuttalText) into
// restdtos.ReviewReadoutFinding.
func findingToReadoutWire(f reviewpost.Finding, row sqlcgen.ReviewFinding) restdtos.ReviewReadoutFinding {
	out := restdtos.ReviewReadoutFinding{
		IdentityHash: f.IdentityHash,
		Severity:     restdtos.ReviewReadoutFindingSeverity(f.Severity),
		FilePath:     f.FilePath,
		Description:  f.Description,
		SuggestedFix: f.SuggestedFix,
		Status:       restdtos.ReviewReadoutFindingStatus(row.Status),
		RebuttalText: row.RebuttalText,
		StartLine:    f.StartLine,
		EndLine:      f.EndLine,
	}
	if f.SentinelKind != nil {
		k := string(*f.SentinelKind)
		out.SentinelKind = &k
	}
	if f.Line != nil {
		line := *f.Line
		out.Line = &line
	}
	return out
}

// reviewReadoutVerdictToWire converts one reviewverdict.Record into
// restdtos.ReviewReadoutLatestVerdict -- the read-side mirror of
// digestInputFromWire (reviewverdict.go), forwarding every field
// verbatim.
func reviewReadoutVerdictToWire(rec reviewverdict.Record) restdtos.ReviewReadoutLatestVerdict {
	blastRadius := make([]restdtos.ReviewReadoutVerdictBlastRadiusElem, len(rec.Verdict.BlastRadius))
	for i, t := range rec.Verdict.BlastRadius {
		blastRadius[i] = restdtos.ReviewReadoutVerdictBlastRadiusElem(t)
	}

	out := restdtos.ReviewReadoutLatestVerdict{
		RiskLevel:         restdtos.ReviewReadoutVerdictRiskLevel(rec.Verdict.RiskLevel),
		Premise:           restdtos.ReviewReadoutVerdictPremise(rec.Verdict.Premise),
		BlastRadius:       blastRadius,
		FilesChanged:      rec.Verdict.FilesChanged,
		TestsCoverage:     restdtos.ReviewReadoutVerdictTestsCoverage(rec.Verdict.TestsCoverage),
		DocsDrift:         restdtos.ReviewReadoutVerdictDocsDrift(rec.Verdict.DocsDrift),
		ProposedShippable: restdtos.ReviewReadoutVerdictProposedShippable(rec.Verdict.ProposedShippable),
		Shippable:         restdtos.ReviewReadoutVerdictShippable(rec.Verdict.Shippable),
		Digest:            digestToWire(rec.Digest),
		FactCheckKilled:   rec.FactCheckKilled,
		HeadSha:           rec.HeadSHA,
		PostedAt:          rec.CreatedAt,
	}
	if rec.ReviewPath != "" {
		p := string(rec.ReviewPath)
		out.ReviewPath = &p
	}
	if rec.CounterReview != "" {
		c := string(rec.CounterReview)
		out.CounterReview = &c
	}
	if rec.FactCheck != "" {
		fc := string(rec.FactCheck)
		out.FactCheck = &fc
	}
	return out
}

// digestToWire converts one reviewpost.Digest into restdtos.Digest,
// forwarding every field verbatim -- the read-side mirror of
// digestInputFromWire (reviewverdict.go).
func digestToWire(d reviewpost.Digest) restdtos.Digest {
	out := restdtos.Digest{
		Summary:             d.Summary,
		DescriptionAdequacy: restdtos.DigestDescriptionAdequacy(d.DescriptionAdequacy),
		AdequacyExplanation: d.AdequacyExplanation,
	}
	if d.StackRisks != "" {
		v := d.StackRisks
		out.StackRisks = &v
	}
	if d.UnverifiedLimits != "" {
		v := d.UnverifiedLimits
		out.UnverifiedLimits = &v
	}
	if d.ProposedBody != "" {
		v := d.ProposedBody
		out.ProposedBody = &v
	}
	if d.ContestedPoints != "" {
		v := d.ContestedPoints
		out.ContestedPoints = &v
	}
	for _, ad := range d.ArchDecisions {
		decision := ad.Decision
		rejected := ad.RejectedAlternative
		conformance := ad.ConventionConformance
		out.ArchDecisions = append(out.ArchDecisions, restdtos.ArchDecision{
			Decision:              &decision,
			RejectedAlternative:   &rejected,
			ConventionConformance: &conformance,
		})
	}
	return out
}
