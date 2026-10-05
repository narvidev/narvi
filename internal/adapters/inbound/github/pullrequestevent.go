// This file (pullrequestevent.go) implements §8.2's own ("sentinels +
// suggestions") merge-gating half (§17.4/§17.5): the new GitHub
// `pull_request` webhook lane that reacts to the ORIGIN pull request
// closing. §24.1 already documents (independently of this Step) that
// "nothing in this codebase today parses GitHub's pull_request event at
// all" -- eventTypePullRequest (payload.go) is used ONLY for the
// action=="labeled" manual re-trigger lane (§8.2) before this Step;
// this file adds the SAME event type's action=="closed" case, exactly
// the "one generic pull_request handler, switching on action" shape this
// Step's own design recommends so §20 (action=="synchronize")
// extends the same lane rather than reinventing the parsing/claim/dedupe
// plumbing independently.
//
// # Honest, named scope limit -- read before assuming this "merges PRs"
//
// This file implements the FULL decision pipeline §17.4 describes:
// parse -> look up sentinel_fixes -> gather the four checks' own inputs ->
// evaluate internal/domain/sentinelfix.EvaluateMergeGate -> record
// audit_log (§17.5) -> either merge or leave the fix PR as an ordinary
// needs_review item. What it does NOT implement is the actual GitHub-side
// cherry-pick-and-force-push mechanic (§17.4's own central operation:
// "only the fix session's own commits are cherry-picked onto the current
// tip of the default branch and force-pushed to the fix branch") or
// either of the two merge calls that would follow it (the
// not-yet-researched Stacks-API merge endpoint, §17.4/§17.6's own named
// open question; and the ordinary legacy merge endpoint, which this file
// deliberately does NOT call either, even though that endpoint itself is
// simple and well-documented -- calling it WITHOUT the cherry-pick having
// actually run first would merge the WRONG diff under squash/rebase-merge,
// exactly the bug cherry-picking exists to prevent, per §17.4's own
// explicit reasoning). fixMerger (below) is the one, explicitly-named
// extension point where that real mechanic belongs once built -- its one
// implementation here (notImplementedFixMerger) always denies, which is
// SAFE by construction: §17.4's own fallback for ANY failing check is
// "leave the fix PR as an ordinary needs_review item instead of forcing
// it through," so a fixMerger that never succeeds degrades this feature
// to "gate evaluated and audited, never auto-merged" rather than ever
// merging something incorrectly.
//
// mergeGateDataSource (below) is likewise a narrow, INJECTABLE interface
// separating "what are the three real-world facts (changed files, CI
// status, mergeable-cleanly)" from the pure policy (sentinelfix.
// EvaluateMergeGate) that consumes them -- githubMergeGateDataSource, its
// one real implementation, gets ChangedFiles for real (parsing the fix
// PR's own pre-fetched diff, reusing GetPullRequestDiff -- a call this
// ingress already makes elsewhere, §17.6's own "incremental addition to a
// call this ingress already makes" precedent) but CIStatus/MergeableCleanly
// are ALSO left as a named, NOT-yet-implemented gap (no GitHub API research
// for a combined-status/mergeable-state call was performed for this Step) --
// both fail closed (deny) rather than fabricate a "green"/"clean" answer.

package github

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strings"

	"github.com/jackc/pgx/v5/pgtype"

	"github.com/narvidev/narvi/internal/adapters/outbound/postgres"
	"github.com/narvidev/narvi/internal/app/auditlog"
	"github.com/narvidev/narvi/internal/app/autonomy"
	domainautonomy "github.com/narvidev/narvi/internal/domain/autonomy"
	"github.com/narvidev/narvi/internal/domain/reposource"
	"github.com/narvidev/narvi/internal/domain/sentinelfix"
	"github.com/narvidev/narvi/internal/platform"
)

// pullRequestEventPayload is the small subset of GitHub's real
// `pull_request` webhook event shape this lane needs
// (https://docs.github.com/webhooks/webhook-events-and-payloads#pull_request).
type pullRequestEventPayload struct {
	Action     string `json:"action"`
	Repository struct {
		FullName string `json:"full_name"`
	} `json:"repository"`
	PullRequest struct {
		Number int32 `json:"number"`
		Merged bool  `json:"merged"`
	} `json:"pull_request"`
}

// ErrCherryPickMergeNotImplemented is fixMerger's own one, honestly-named
// error (this file's own top doc comment) -- returned unconditionally by
// notImplementedFixMerger, causing this lane to always fall back to
// leaving the fix PR as an ordinary needs_review item, never an incorrect
// auto-merge.
var ErrCherryPickMergeNotImplemented = errors.New("github: sentinel-auto-fix cherry-pick-and-merge is not yet implemented (§17.4's own cherry-pick mechanic, and the not-yet-researched Stacks-API merge endpoint, §17.6, are both open items)")

// fixMerger is the extension point this file's own top doc comment names
// -- cherry-pick the fix session's own commits onto the current default-
// branch tip, force-push the fix branch, then merge it (via the Stacks
// API when stackRegistered, the legacy endpoint otherwise, per §17.4's own
// branching rule). stackRegistered is the authority §17.6 requires: a
// FRESH GetPullRequest.Stack field, never a locally-persisted boolean --
// the caller (handlePullRequestClosed, below) is responsible for
// obtaining it that way.
type fixMerger interface {
	CherryPickAndMerge(ctx context.Context, owner, repo string, fixPRNumber int, stackRegistered bool) error
}

// notImplementedFixMerger is this Step's own one real fixMerger --
// ALWAYS returns ErrCherryPickMergeNotImplemented (this file's own top
// doc comment explains why this is a safe, deliberate scope limit, not an
// oversight).
type notImplementedFixMerger struct{}

func (notImplementedFixMerger) CherryPickAndMerge(context.Context, string, string, int, bool) error {
	return ErrCherryPickMergeNotImplemented
}

// prDiffFetcher is githubMergeGateDataSource's own narrow slice of
// *githubapi.Adapter's real diff-fetching surface -- JUST
// GetPullRequestDiff, the PR's own CURRENT diff (this file's ChangedFiles
// below parses it for changed file paths, synchronously, at merge-gate-
// evaluation time; there is no later cross-read this needs to stay
// anchored against the way review_verdicts.head_sha does, so
// GetPullRequestDiff's own "always reflects the PR's current head" shape
// is exactly right here, unlike reviewcontext.Fetch's own now-different
// need -- see that function's own doc comment, fetch.go). Deliberately
// its OWN small interface (mirroring PullRequestResolver's own identical
// "small interface over a real outbound call" precedent this file already
// cites) rather than continuing to reuse reviewcontext.Fetcher for
// convenience: that interface was narrowed to exactly what review-turn-context
// assembly needs (GetPullRequest/GetCompareDiff),
// which no longer includes GetPullRequestDiff at all -- this call site's
// own need was always genuinely different, and borrowing a neighboring
// interface only worked by coincidence until that interface's own shape
// changed out from under it.
type prDiffFetcher interface {
	GetPullRequestDiff(ctx context.Context, owner, repo string, number int32, token string) (diff string, truncated bool, err error)
}

// mergeGateDataSource gathers the three REAL-WORLD facts sentinelfix.
// EvaluateMergeGate's own pure policy needs, beyond the toggle (which the
// caller reads directly from repo_settings) -- a narrow, injectable
// interface so the orchestration below is testable with a fake, mirroring
// this package's own established PullRequestResolver/prDiffFetcher
// precedent for "a small interface over a real outbound call."
type mergeGateDataSource interface {
	// ChangedFiles returns every file path the fix PR's own diff touches.
	ChangedFiles(ctx context.Context, owner, repo string, fixPRNumber int32) ([]string, error)
	// CIStatus reports whether CI is green at the fix branch's own tip.
	CIStatus(ctx context.Context, owner, repo string, fixPRNumber int32) (green bool, err error)
	// MergeableCleanly reports whether the fix PR is currently mergeable
	// with no conflict (the best available real proxy for "did the
	// cherry-pick/automatic stack-rebase apply cleanly" until §17.4's own
	// real cherry-pick mechanic exists -- see this file's own top doc
	// comment).
	MergeableCleanly(ctx context.Context, owner, repo string, fixPRNumber int32) (bool, error)
	// StackRegistered reports whether the fix PR currently belongs to a
	// GitHub-native stack -- confirmed-finding fix: §17.6 (and this file's
	// own fixMerger doc comment) is explicit that this signal must ALWAYS
	// be a FRESH GetPullRequest.Stack check, made at merge-gating time,
	// never a locally-persisted boolean (sentinel_fixes.stack_registered
	// is deliberately an observability-only column, per its own migration's
	// doc comment: registration and merge-gating can be arbitrarily far
	// apart in time, and the pair's own real stack status can change in
	// between). Called ONLY once decision.Allowed is already true, and
	// ONLY to decide WHICH merge endpoint fixMerger.CherryPickAndMerge
	// should use -- it is never one of sentinelfix.EvaluateMergeGate's own
	// four checks, and never widens or narrows that four-check policy.
	StackRegistered(ctx context.Context, owner, repo string, fixPRNumber int32) (bool, error)
}

// githubMergeGateDataSource is this Step's own one real mergeGateDataSource:
// ChangedFiles is REAL (parses the fix PR's own pre-fetched diff via the
// SAME GetPullRequestDiff call this ingress already makes elsewhere);
// StackRegistered is REAL too (a fresh GetPullRequest call, PullRequestResolver
// -- confirmed-finding fix, see mergeGateDataSource's own doc comment
// above); CIStatus/MergeableCleanly are a named, NOT-yet-implemented gap
// (this file's own top doc comment) -- both fail closed.
type githubMergeGateDataSource struct {
	diffFetcher  prDiffFetcher
	pullRequests PullRequestResolver
	botToken     string
	timeouts     platform.Timeouts
}

func (d *githubMergeGateDataSource) ChangedFiles(ctx context.Context, owner, repo string, fixPRNumber int32) ([]string, error) {
	if d.diffFetcher == nil {
		return nil, errors.New("github: sentinel-auto-fix merge gate: no diff fetcher configured")
	}
	diffCtx, cancel := context.WithTimeout(ctx, d.timeouts.GitHubPRDiffTimeout)
	defer cancel()
	diff, _, err := d.diffFetcher.GetPullRequestDiff(diffCtx, owner, repo, fixPRNumber, d.botToken)
	if err != nil {
		return nil, err
	}
	return parseChangedFilesFromDiff(diff), nil
}

func (d *githubMergeGateDataSource) CIStatus(context.Context, string, string, int32) (bool, error) {
	return false, errors.New("github: sentinel-auto-fix merge gate: CI-status check is not yet implemented (no GitHub combined-status API research was performed for this Step)")
}

func (d *githubMergeGateDataSource) MergeableCleanly(context.Context, string, string, int32) (bool, error) {
	return false, errors.New("github: sentinel-auto-fix merge gate: mergeable-cleanly check is not yet implemented (no GitHub mergeable-state API research was performed for this Step)")
}

// StackRegistered implements mergeGateDataSource: a FRESH
// PullRequestResolver.GetPullRequest call against the fix PR itself,
// checked at merge-gating time -- confirmed-finding fix, see
// mergeGateDataSource's own doc comment for why this must never be the
// persisted sentinel_fixes.stack_registered column instead. d.pullRequests
// == nil (this package's own handler_test.go, or any other minimal wiring
// that doesn't care about this Step) is reported as a plain error, never
// silently defaulted to false/true -- the caller (handlePullRequestClosed)
// treats a failure here exactly like any other CherryPickAndMerge failure:
// logged, no merge, fix PR left for ordinary human review.
func (d *githubMergeGateDataSource) StackRegistered(ctx context.Context, owner, repo string, fixPRNumber int32) (bool, error) {
	if d.pullRequests == nil {
		return false, errors.New("github: sentinel-auto-fix merge gate: no pull request resolver configured")
	}
	getCtx, cancel := context.WithTimeout(ctx, d.timeouts.GitHubGetPRTimeout)
	defer cancel()
	pr, err := d.pullRequests.GetPullRequest(getCtx, owner, repo, fixPRNumber, d.botToken)
	if err != nil {
		return false, err
	}
	return pr.Stack != nil, nil
}

// parseChangedFilesFromDiff extracts every file path this diff touches,
// from TWO distinct signal shapes in a unified diff (GitHub's own raw-diff
// format, the SAME shape GetPullRequestDiff already returns for the
// ordinary pre-fetched-context feature, §8.2):
//
//   - "+++ b/<path>" headers (deliberately reuses "+++", the NEW-side
//     header, rather than "---", so a file DELETED by the fix session
//     (named only by "--- a/<path>" with "+++ /dev/null") is not
//     double-counted incorrectly);
//   - "rename from <path>"/"rename to <path>" lines -- confirmed-finding
//     fix: a PURE rename (100% similarity, zero content change) produces
//     NEITHER a "+++" NOR a "---" line at all (verified directly against
//     real git behavior), so relying on "+++ " alone made this function
//     -- and therefore firstNonTestOrDocPath/EvaluateMergeGate, this
//     Step's own documented "independent of, and never assuming, §17.2's
//     spawn-time capability restriction" backstop -- BLIND to a sentinel-
//     fix session's own bash tool (never restricted by that capability
//     layer) simply `git mv`-ing an existing production file onto a
//     test/doc-looking path with no content change at all: changedFiles
//     came back empty, and firstNonTestOrDocPath's own doc comment treats
//     an empty list as "every file is test/doc, trivially" -- vacuously
//     passing the one check this Step ships as that backstop's authoritative,
//     tested-independent implementation. Both the OLD path ("rename from")
//     and the NEW path ("rename to") are recorded: the new path alone would
//     read as an innocent test/doc file (that IS the attack), so the old,
//     real production path must also appear in this function's own return
//     value for firstNonTestOrDocPath to ever see it and correctly deny.
//     A rename WITH a content change also emits real "+++"/"---" lines
//     alongside "rename from"/"rename to" -- recording the new path twice
//     in that case is harmless (firstNonTestOrDocPath only needs to see an
//     offending path once).
func parseChangedFilesFromDiff(diff string) []string {
	var files []string
	for _, line := range strings.Split(diff, "\n") {
		switch {
		case strings.HasPrefix(line, "+++ "):
			path := strings.TrimSpace(strings.TrimPrefix(line, "+++ "))
			path = strings.TrimPrefix(path, "b/")
			if path == "" || path == "/dev/null" {
				continue
			}
			files = append(files, path)
		case strings.HasPrefix(line, "rename from "):
			path := strings.TrimSpace(strings.TrimPrefix(line, "rename from "))
			if path != "" {
				files = append(files, path)
			}
		case strings.HasPrefix(line, "rename to "):
			path := strings.TrimSpace(strings.TrimPrefix(line, "rename to "))
			if path != "" {
				files = append(files, path)
			}
		}
	}
	return files
}

// handlePullRequestClosed implements this file's own top doc comment's
// full decision pipeline for ONE `pull_request` webhook delivery whose
// action is "closed" -- called from NewHandler (handler.go) BEFORE
// parseMention/the mention pipeline, since a closed-PR event is never a
// mention and must never reach CreateOrJoin. Always writes an HTTP
// response itself (200 in every case this function is reached for at all
// -- a webhook handler's own 200 means "delivery received and processed",
// never "the fix PR was merged"; GitHub does not distinguish those two
// concepts and this lane must not conflate them either).
func handlePullRequestClosed(
	ctx context.Context,
	w http.ResponseWriter,
	body []byte,
	sentinelFixes *postgres.SentinelFixStore,
	repoSettings *postgres.RepoSettingsStore,
	auditLog *postgres.AuditLogStore,
	dataSource mergeGateDataSource,
	merger fixMerger,
	gate *autonomy.Gate,
) {
	logger := platform.Logger(ctx)

	var payload pullRequestEventPayload
	if err := json.Unmarshal(body, &payload); err != nil {
		logger.Error("github: decode pull_request webhook payload failed", "error", err)
		w.WriteHeader(http.StatusBadRequest)
		return
	}

	fix, err := sentinelFixes.Get(ctx, payload.Repository.FullName, payload.PullRequest.Number)
	if err != nil {
		// No sentinel-auto-fix was ever triggered for this PR -- the
		// overwhelming common case for any repo not using this feature at
		// all, or a PR this feature never fired on. Acknowledge, nothing
		// to do.
		w.WriteHeader(http.StatusOK)
		return
	}
	// Only a 'fix_open' claim is actionable here -- 'pending'/'spawned'
	// (the fix session hasn't opened its own PR yet) has nothing to
	// merge-gate; 'fix_merged'/'abandoned' are already terminal.
	if fix.Status != "fix_open" || fix.FixPrNumber == nil {
		w.WriteHeader(http.StatusOK)
		return
	}

	if !payload.PullRequest.Merged {
		// §17.5: "If the origin PR itself is never merged (closed,
		// abandoned), the fix PR is simply left open as an ordinary
		// review item -- never silently discarded."
		if _, err := sentinelFixes.MarkAbandoned(ctx, fix.ID); err != nil {
			logger.Error("github: mark sentinel_fixes abandoned failed", "error", err)
		}
		w.WriteHeader(http.StatusOK)
		return
	}

	owner, repo, ok := reposource.SplitFullName(payload.Repository.FullName)
	if !ok {
		logger.Error("github: sentinel-auto-fix merge gate: repo_full_name not in owner/repo shape", "repo_full_name", payload.Repository.FullName)
		w.WriteHeader(http.StatusOK)
		return
	}

	// toggleEnabled is re-read FRESH, at merge-gating time (§17.4's own
	// fourth check: "the toggle is still enabled" -- an admin may have
	// disarmed it mid-flight since the fix was originally triggered).
	// Fail-closed on a missing row or read error, mirroring every other
	// per-repo policy flag's identical precedent in this codebase.
	toggleEnabled := false
	if settings, settingsErr := repoSettings.Get(ctx, payload.Repository.FullName); settingsErr == nil {
		toggleEnabled = settings.SentinelAutofixEnabled
	}

	changedFiles, filesErr := dataSource.ChangedFiles(ctx, owner, repo, *fix.FixPrNumber)
	ciGreen, ciErr := dataSource.CIStatus(ctx, owner, repo, *fix.FixPrNumber)
	cherryPickClean, cleanErr := dataSource.MergeableCleanly(ctx, owner, repo, *fix.FixPrNumber)

	var decision sentinelfix.MergeGateDecision
	switch {
	case filesErr != nil:
		decision = sentinelfix.MergeGateDecision{Allowed: false, Reason: "could not determine the fix PR's own changed files: " + filesErr.Error()}
	case ciErr != nil:
		decision = sentinelfix.MergeGateDecision{Allowed: false, Reason: "could not determine CI status: " + ciErr.Error()}
	case cleanErr != nil:
		decision = sentinelfix.MergeGateDecision{Allowed: false, Reason: "could not determine mergeable-cleanly state: " + cleanErr.Error()}
	default:
		decision = sentinelfix.EvaluateMergeGate(changedFiles, ciGreen, cherryPickClean, toggleEnabled)
	}

	// §40.2: an allowed gate is an unattended merge, which the autonomy
	// freeze holds -- read here, after the four checks and before the
	// fresh stack read and the merge. The gate runs once per close event of
	// the origin, so a held merge is not re-evaluated after the freeze
	// lifts: the fix pull request stays open as an ordinary review item,
	// and its claim stays fix_open. The audit row below records the skip.
	frozenSkip := domainautonomy.SkipReason("")
	if decision.Allowed {
		if skip, reason := gate.Check(ctx, domainautonomy.SiteSentinelFixMerge, "sentinel_fix_id", fix.ID.String(), "fix_pr_number", *fix.FixPrNumber); skip {
			frozenSkip = reason
			decision = sentinelfix.MergeGateDecision{Allowed: false, Reason: "skipped: autonomy is frozen"}
			if reason != domainautonomy.SkipFrozen {
				decision.Reason = "skipped: the autonomy freeze could not be read"
			}
		}
	}

	mergeAttempted := false
	mergeErrString := ""
	if decision.Allowed {
		mergeAttempted = true
		// Confirmed-finding fix: stackRegistered is resolved FRESH, right
		// here, at merge-gating time -- never fix.StackRegistered (the
		// persisted, observability-only column) -- see mergeGateDataSource.
		// StackRegistered's own doc comment for why. A failure to determine
		// it fresh is treated exactly like a CherryPickAndMerge failure
		// itself: logged into mergeErrString, no merge attempted, fix PR
		// left for ordinary human review -- never a silent fallback to a
		// stale/guessed value that could route the real merge call to the
		// wrong endpoint (legacy vs Stacks API), reproducing the exact
		// GitHub-documented failure this amendment exists to avoid.
		stackRegistered, stackErr := dataSource.StackRegistered(ctx, owner, repo, *fix.FixPrNumber)
		if stackErr != nil {
			mergeErrString = "could not determine fresh stack-registration state: " + stackErr.Error()
		} else if mergeErr := merger.CherryPickAndMerge(ctx, owner, repo, int(*fix.FixPrNumber), stackRegistered); mergeErr != nil {
			mergeErrString = mergeErr.Error()
		} else {
			if _, err := sentinelFixes.MarkMerged(ctx, fix.ID); err != nil {
				logger.Error("github: mark sentinel_fixes merged failed", "error", err)
			}
		}
	}

	// §17.5: recorded regardless of outcome -- "the origin PR, the review
	// session, the fix PR, and which of the four checks passed." actor_
	// user_id is NULL: "the same allowance already made in the audit_log
	// schema for actions with no human actor" (§17.5) -- this is a
	// system-initiated check, never a delegated human one (§17.4).
	auditDetail := map[string]any{
		"repo_full_name":   payload.Repository.FullName,
		"origin_pr_number": payload.PullRequest.Number,
		"fix_pr_number":    *fix.FixPrNumber,
		"origin_session":   fix.OriginReviewSessionID.String(),
		"allowed":          decision.Allowed,
		"reason":           decision.Reason,
		"merge_attempted":  mergeAttempted,
		"merge_error":      mergeErrString,
	}
	// "skipped" is present only on a merge the freeze held (§40.2), naming
	// its reason: frozen, or freeze_unreadable.
	if frozenSkip != "" {
		auditDetail["skipped"] = string(frozenSkip)
	}
	if err := auditlog.Record(ctx, auditLog, pgtype.UUID{}, "sentinel_fix.merge_gate_evaluated", "sentinel_fix", fix.ID.String(), auditDetail); err != nil {
		logger.Error("github: record sentinel_fix.merge_gate_evaluated audit log failed", "error", err)
	}

	w.WriteHeader(http.StatusOK)
}

// readPullRequestEventAction peeks at body's own top-level "action" field
// without fully decoding the payload -- used by NewHandler (handler.go)
// to decide, cheaply, whether a `pull_request` event is this file's own
// "closed" lane or the EXISTING "labeled" lane (payload.go's own
// parseMention) BEFORE committing to either one's own full parse.
func readPullRequestEventAction(body []byte) string {
	var envelope struct {
		Action string `json:"action"`
	}
	if err := json.Unmarshal(body, &envelope); err != nil {
		return ""
	}
	return envelope.Action
}
