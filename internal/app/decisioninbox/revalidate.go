package decisioninbox

import (
	"context"
	"errors"
	"fmt"

	"github.com/narvidev/narvi/internal/app/ports"
	appreviewverdict "github.com/narvidev/narvi/internal/app/reviewverdict"
	"github.com/narvidev/narvi/internal/domain/autoapproval"
	"github.com/narvidev/narvi/internal/domain/reposource"
	"github.com/narvidev/narvi/internal/domain/review"
	"github.com/narvidev/narvi/internal/platform"
)

// RevalidateForMerge re-checks, LIVE and never cached (§16.2, §5.2's own
// "the rendered queue is never trusted as authority" invariant), whether
// (repoFullName, prNumber) is currently eligible for actorGitHubID to
// merge through the decision inbox's own Merge endpoint. sourceControl is
// taken as a DIRECT parameter here, deliberately never deps.SCMCache --
// the whole point of this function is a fresh read, and threading it
// through the cache would silently defeat that (this is this package's
// ONE caller that must bypass the cache on purpose; every other read in
// this package goes through deps.SCMCache exactly because staleness is
// acceptable there, per §16.2's own "SCM data is cached... never
// presented as live truth" -- an action endpoint is the one place "live
// truth" is exactly what's required).
//
// Resolves the target PR via a live, actor-scoped ListOpenPRsForUser
// search (the human path's own natural "is this PR genuinely assigned to
// the clicking actor" proof own reasoning,
// unchanged by this Step), then delegates every remaining check to
// revalidateCore below -- the SAME core RevalidateForAutoMerge (§21.2
// stage 2) shares, so a human-clicked confirm and a machine-initiated
// merge are NEVER independently-maintained checks that could silently
// drift apart (§21.2: "a deliberate reuse, not a parallel merge path").
//
// A store error from the §17 sentinel-fix exclusion or the open-findings
// count (both inside revalidateCore) is propagated outright as err,
// refusing the merge (a fail-CLOSED requirement) -- neither is a legitimate "this PR fails eligibility"
// domain answer the way every OTHER ok=false return is; both are
// infrastructure failures this function has no safe way to interpret as
// "not blocking", so the caller (httpapi's MergePullRequest) surfaces a
// 500 and the merge simply does not proceed.
//
// ok=true only when every check passes, in which case headSHA is the
// PR's own CURRENT head SHA -- the caller passes this straight through to
// MergePR as its own optimistic-concurrency guard (ports.MergePRSpec.
// HeadSHA), so a push landing between this revalidation and the actual
// merge call fails loudly (a 409 from GitHub itself) rather than silently
// merging code nobody just re-checked. ok=false's own reason is a short,
// human-readable explanation suitable for a 409 response body.
func RevalidateForMerge(ctx context.Context, deps Deps, sourceControl ports.SourceControl, actorGitHubID, repoFullName string, prNumber int, token string) (ok bool, headSHA string, reason string, err error) {
	prs, truncated, err := sourceControl.ListOpenPRsForUser(ctx, ports.ListOpenPRsForUserSpec{GitHubExternalID: actorGitHubID, Token: token})
	if err != nil {
		return false, "", "", err
	}

	var target *ports.OpenPR
	for i := range prs {
		if prs[i].Owner+"/"+prs[i].Repo == repoFullName && prs[i].Number == prNumber {
			target = &prs[i]
			break
		}
	}
	if target == nil {
		if truncated {
			// The truncated signal, applied here: a degraded/partial
			// live read (e.g. one of GitHub's own
			// search queries failed) means this function genuinely cannot
			// tell "not assigned to you" from "we simply failed to see
			// it" -- asserting the former with confidence here would be a
			// false-confident 409 that could discourage a legitimate
			// retry. Fails as an error (500, prompting a retry) instead
			// of a confident domain "no".
			return false, "", "", fmt.Errorf("decisioninbox: revalidate for merge: could not confirm this pull request's current state (a degraded/partial GitHub read) -- please retry")
		}
		return false, "", "this pull request is no longer open, or no longer assigned to you", nil
	}

	return revalidateCore(ctx, deps, sourceControl, token, repoFullName, prNumber, *target)
}

// RevalidateForAutoMerge is RevalidateForMerge's own machine-initiated
// sibling (§21.2 stage 2) -- internal/app/automerge's own worker calls
// this instead, using the deployment's own bot token rather than any
// particular human actor's, since a background worker has no "clicking
// human" to scope a ListOpenPRsForUser search to (see ports.SourceControl.
// GetOpenPR's own doc comment for the full "why a different discovery
// primitive" reasoning). Every check AFTER target resolution is the
// IDENTICAL revalidateCore both functions share -- §21.2: "reuses the
// decision inbox's existing server-side re-validation-at-click contract
// unchanged... a deliberate reuse, not a parallel merge path."
//
// found=false (GetOpenPR's own confirmed-404 signal) is reported as a
// plain ok=false/reason, mirroring RevalidateForMerge's own "no longer
// open" case above -- a PR closed/merged through some other path between
// discovery and this call is an ordinary, expected race, never an error.
func RevalidateForAutoMerge(ctx context.Context, deps Deps, sourceControl ports.SourceControl, repoFullName string, prNumber int, botToken string) (ok bool, headSHA string, reason string, err error) {
	owner, repo, splitOK := reposource.SplitFullName(repoFullName)
	if !splitOK {
		return false, "", "", fmt.Errorf("decisioninbox: revalidate for auto-merge: repoFullName %q is not shaped owner/repo", repoFullName)
	}

	// Bounded by platform.Timeouts.GitHubGetOpenPRTimeout (G4, fourth
	// adversarial-review round; moved to its own dedicated field, H3,
	// fifth round): this call previously ran on the bare, unbounded ctx
	// this function was handed -- ctx's own only bound is whatever the
	// automerge worker's own tick context happens to carry
	// (internal/app/automerge/worker.go's own PumpOnce/mergeCandidate,
	// which sets none), so a hung GitHub call here could stall a merge
	// tick indefinitely. See GitHubGetOpenPRTimeout's own doc comment
	// (platform/timeouts.go) for what GetOpenPR actually does and why it
	// no longer reuses GitHubGetPRTimeout.
	getPRCtx, cancel := context.WithTimeout(ctx, deps.Timeouts.GitHubGetOpenPRTimeout)
	target, found, err := sourceControl.GetOpenPR(getPRCtx, owner, repo, prNumber, botToken)
	cancel()
	if err != nil {
		return false, "", "", err
	}
	// H3 (fifth adversarial-review round; corrected, sixth round -- the
	// previous version of this paragraph asserted a uniform failure model
	// for all five of GetOpenPR's sub-calls, sourced to
	// buildOpenPRFromDetail's own doc comment, which makes no such claim
	// and is wrong for two of the five). fetchOpenPRDetail's own failure
	// is a real Go error, already caught by this function's err != nil
	// check two lines above -- a deadline firing during THAT call cannot
	// reach the branch below at all. fetchReviewDecision and
	// fetchChangedFilePaths each swallow their own failure into a
	// degraded field instead, per their own doc comments (listopenprs.go,
	// ports.OpenPR.ReviewDecisionDegraded/ChangedFilesListDegraded).
	// fetchCIConclusionLive carries no degraded field for either of its
	// two GETs at all -- see that function's own doc comment and
	// terminal switch (listopenprs.go) for what it actually reports on a
	// half-read composite: not "silently missing whatever it would have
	// reported", but a confident CIConclusionSuccess whenever the GET
	// that failed was the only one that could have contradicted the GET
	// that didn't. That hazard predates this PR, is unchanged by it, and
	// is not fixed here -- it is out of scope for this change and tracked
	// as its own defect. What THIS guard closes is narrower: a deadline
	// firing partway through GetOpenPR's own five-call composite still
	// returns here with err == nil and a target reflecting whichever
	// later sub-call the deadline cut short. Left unchecked, that renders
	// downstream as an ordinary, permanent-looking eligibility refusal
	// rather than the transient, retry-worthy timeout it actually was.
	//
	// It does NOT render as a false approval, and the previous version of
	// this paragraph claimed it could. A deadline that cuts the CI read
	// short leaves the LATER changed-files GET failing on that same
	// expired ctx, so ChangedFilesListDegraded is set, and
	// ComputeEligible refuses on ReasonBlastRadiusUnknown before any
	// approval is reachable. Established with an httptest harness against
	// the real adapter rather than reasoned about: the false CI green
	// does occur, and it arrives inseparably bundled with the degraded
	// changed-files signal that refuses it. Reaching a false approval
	// needs an INDEPENDENT non-deadline failure on one CI GET with the
	// changed-files GET still succeeding -- the separately-tracked
	// hazard, in which a deadline plays no part. Detected
	// via getPRCtx's OWN error, checked for DeadlineExceeded specifically
	// -- never a bare non-nil check, which the cancel() call two lines
	// above would ALSO satisfy on the ordinary, well-within-budget
	// success path (a context's recorded error is set by whichever of
	// "its own deadline fired" or "someone called its cancel func"
	// happens FIRST, and never overwritten after -- so DeadlineExceeded
	// surviving past this function's own cancel() call means the
	// deadline is what actually fired here, not this function's own
	// routine cleanup).
	if errors.Is(getPRCtx.Err(), context.DeadlineExceeded) {
		return false, "", "", fmt.Errorf("decisioninbox: revalidate for auto-merge: get open pr timed out partway through its own five-call fetch (GitHubGetOpenPRTimeout) -- refusing rather than trusting whichever sub-call was cut short: %w", context.DeadlineExceeded)
	}
	if !found {
		return false, "", "this pull request is no longer open", nil
	}

	return revalidateCore(ctx, deps, sourceControl, botToken, repoFullName, prNumber, target)
}

// revalidateCore is the SHARED body of RevalidateForMerge/
// RevalidateForAutoMerge -- every check both functions apply to an
// already-resolved target OpenPR, so a human-clicked confirm and a
// machine-initiated merge can never independently drift on what "still
// eligible to merge" means. Re-derives the EXACT SAME criteria
// buildPROpenItem used to classify this PR as ready_to_merge in the
// first place (§17 exclusion, not a draft, not a handoff PR, platform-
// authored, zero open findings, the REAL §21.2 auto-approval eligibility
// engine), PLUS HasChangesRequested -- never a
// narrower "just check CI is still green" shortcut, since ANY of these
// facts (a new commit landed dropping CI red, a reviewer requested
// changes, a needs-human label applied...) could have changed since the
// cached queue was last rendered.
//
// sourceControl/token (finding F1 (§21.1's amendment)) are threaded through from
// whichever caller already resolved target, so this function can make
// its OWN fresh ResolveBranchSHA call for the base-freshness check below
// -- deliberately never target.BaseSHA (ports.OpenPR.BaseSHA's own doc
// comment: GitHub's per-PR CACHED `base.sha` snapshot, verified to lag
// the base branch's real tip by an unknown, sometimes month-scale
// margin). token mirrors CurrentHeadSHA's own already-live sourcing
// exactly: RevalidateForMerge passes the acting human's own OAuth token,
// RevalidateForAutoMerge passes the deployment's bot token -- the SAME
// credential each caller already used to resolve target itself.
func revalidateCore(ctx context.Context, deps Deps, sourceControl ports.SourceControl, token string, repoFullName string, prNumber int, target ports.OpenPR) (ok bool, headSHA string, reason string, err error) {
	if target.Draft {
		return false, "", "this pull request is a draft", nil
	}

	excluded, exErr := deps.SentinelFixes.ExistsByFixPRNumber(ctx, repoFullName, int32(prNumber))
	if exErr != nil {
		return false, "", "", fmt.Errorf("decisioninbox: revalidate for merge: check sentinel-fix exclusion: %w", exErr)
	}
	if excluded {
		return false, "", "this pull request is a sentinel-auto-fix follow-up -- it merges automatically once its own checks pass, never through this endpoint", nil
	}

	hasNeedsHuman, _, isHandoffPR := classifyPRLabels(target.Labels)
	if isHandoffPR {
		return false, "", "this pull request is a handoff item, not an ordinary code-review merge decision", nil
	}

	// a degraded review-decision read (GitHub's
	// reviews endpoint itself failed) must never be indistinguishable from
	// a clean "no changes requested" read -- checked BEFORE
	// HasChangesRequested itself so a degraded read gets its own distinct,
	// honest reason string rather than falsely claiming a reviewer
	// requested changes. FAIL CLOSED: this function is reached by BOTH the
	// human-clicked Merge endpoint AND (via RevalidateForAutoMerge, which
	// shares this exact core) the UNATTENDED auto-merge worker -- "we
	// could not tell" must block exactly like a confirmed changes-request
	// would, never silently pass through as "no". Logged (H2, fifth
	// adversarial-review round: made consistent with its two live-check
	// siblings below, ResolveBranchSHA's own failure and IsAncestor's own
	// failure, both of which log) even though the underlying fetch
	// happened earlier, outside this function -- this is still the one
	// place that decides to refuse ON it, so it is the right place to
	// record that decision.
	if target.ReviewDecisionDegraded {
		platform.Logger(ctx).Warn("decisioninbox: review-decision read was degraded, refusing merge -- could not confirm whether a reviewer requested changes", "repo_full_name", repoFullName, "pr_number", prNumber)
		return false, "", "this pull request's review decision could not be confirmed (a degraded GitHub read) -- failing closed rather than trusting an unconfirmed read", nil
	}
	if target.HasChangesRequested {
		return false, "", "this pull request has changes requested by a reviewer", nil
	}

	if !isPlatformAuthored(ctx, deps, target.HTMLURL) {
		return false, "", "this pull request was not authored by a platform session", nil
	}

	openFindings, findingsErr := countOpenFindings(ctx, deps, repoFullName, prNumber)
	if findingsErr != nil {
		return false, "", "", fmt.Errorf("decisioninbox: revalidate for merge: count open findings: %w", findingsErr)
	}
	if openFindings > 0 {
		// Mirrors buildPROpenItem's own identical "kept as its own,
		// separate AND-condition" reasoning (aggregate.go) -- never
		// folded into the eligibility engine itself.
		return false, "", "this pull request has an open, unresolved review finding", nil
	}
	ciGreen := target.CIConclusion == ports.CIConclusionSuccess

	record, hasVerdict, verdictErr := appreviewverdict.GetLatest(ctx, deps.ReviewVerdict, repoFullName, int32(prNumber))
	if verdictErr != nil {
		return false, "", "", fmt.Errorf("decisioninbox: revalidate for merge: get latest review verdict: %w", verdictErr)
	}
	if !hasVerdict {
		return false, "", "this pull request has no review verdict of record", nil
	}

	// a genuine repo_settings read error here means
	// this repo's OWN configured policy (its diff-size threshold, its
	// sensitive-tag list) cannot be established at all -- propagated
	// outright as err, exactly like the §17 sentinel-fix exclusion/
	// open-findings-count errors immediately above (this function's own
	// top doc comment: "propagated outright as err, refusing the merge").
	// FAIL CLOSED: an unattended-merge gate must never substitute the
	// engine's own WIDER built-in defaults for a repo's own narrower
	// configured policy merely because Postgres could not be read this
	// instant.
	cfg, cfgErr := appreviewverdict.LoadEligibilityConfig(ctx, deps.ReviewVerdict, repoFullName)
	if cfgErr != nil {
		return false, "", "", fmt.Errorf("decisioninbox: revalidate for merge: load eligibility config: %w", cfgErr)
	}
	// changedFileCount/touchedBlastRadius/touchedBlastRadiusKnown are ALL
	// derived here from target -- target is revalidateCore's own
	// already-fetched, server-side ports.OpenPR (RevalidateForMerge's
	// live ListOpenPRsForUser search, or RevalidateForAutoMerge's live
	// GetOpenPR call), never the posted verdict's own self-reported
	// FilesChanged/BlastRadius. No new I/O: every field read here was
	// already fetched by the SAME call that produced target.
	//
	// Phase 5 audit findings 1+2 (both fixed, the SAME root cause C1
	// fixed for the verdict's own self-report, now closed for THIS
	// adapter-fetched data too): changedFileCount is target.
	// ChangedFilesCount, GitHub's own authoritative scalar -- never
	// len(target.ChangedFiles), which githubapi caps at one page and
	// which used to also silently read as 0 whenever the underlying
	// GitHub fetch failed outright. touchedBlastRadiusKnown is
	// !target.ChangedFilesListDegraded -- see that field's own doc
	// comment (ports.OpenPR) for the two independent ways it can go
	// true (a failed fetch, or a genuinely large diff whose listing was
	// truncated at GitHub's own one-page cap): either way,
	// ComputeEligible now refuses this PR rather than silently trusting
	// an incomplete-or-absent classification of target.ChangedFiles.
	changedFileCount := target.ChangedFilesCount
	touchedBlastRadius := autoapproval.ClassifyChangedPaths(target.ChangedFiles)
	touchedBlastRadiusKnown := !target.ChangedFilesListDegraded

	// probe (G3/G4, fourth adversarial-review round) asks "setting the
	// base-freshness question aside entirely, is this PR already
	// ineligible" -- every criterion below is fully known WITHOUT any
	// live SCM call: head-SHA equality (VerdictHeadSHA/CurrentHeadSHA,
	// both already in hand), the verdict's own base-ref/ancestor-chain/
	// policy-version equality (record.Context vs. target, no new I/O,
	// mirroring CurrentHeadSHA's own identical sourcing), CI, Shippable,
	// diff size, and blast radius. CurrentBaseSHA is deliberately
	// ASSUMED equal to the verdict's own recorded VerdictBaseSHA -- the
	// most lenient possible stand-in, for two DIFFERENT reasons on
	// ComputeEligible's two base-SHA-comparison checks (corrected, H4,
	// fifth adversarial-review round: a prior version of this comment
	// claimed equality "trivially satisfies BOTH", which is false for the
	// first one). On ReasonBaseMoved, equality genuinely IS trivially
	// satisfying: an assumed-unchanged value can never register as
	// changed, regardless of what BaseAdvancedWithoutRewrite is. On
	// ReasonBaseSHAUnknown, the assumption satisfies nothing and is
	// beside the point -- that check (VerdictBaseSHA == "" ||
	// CurrentBaseSHA == "") fires on VerdictBaseSHA alone whenever IT is
	// empty, and assuming CurrentBaseSHA equal to an empty VerdictBaseSHA
	// does not change that: the probe still correctly refuses here, just
	// not because the two sides were made to agree. Either way, this
	// assumption cannot make either check MORE lenient than any real
	// currentBaseSHA would -- and affects nothing else the probe checks.
	// Any REAL currentBaseSHA can only be equally-or-LESS lenient than
	// this assumption on those two checks, and every other criterion is
	// unaffected by which base-SHA scenario is used -- so if the probe
	// already refuses, the real computation below refuses too (on this
	// exact reason, or an earlier-ordered one still, if the real base SHA
	// also happens to differ), and neither the live ResolveBranchSHA call
	// nor the live IsAncestor call below could ever have changed that
	// outcome. Skipping both in that case is what G3 ("order the checks
	// so the honest reason wins" -- a permanently ineligible PR must
	// never be told a live check's own transient failure is the reason it
	// was refused) and G4 (fewer live calls on the merge path than
	// strictly needed) both ask for, from the same restructuring.
	//
	// VerdictAssessed is unconditionally true in both the probe and the
	// final call below -- hasVerdict was already confirmed true above
	// (the !hasVerdict branch returns earlier) -- but is still passed
	// through explicitly, never left at its own zero value, mirroring
	// touchedBlastRadiusKnown's own identical "never rely on a caller
	// forgetting" discipline: a future edit to this function that moves
	// or removes the early !hasVerdict return must not silently
	// reintroduce ReasonNotAssessed's own fail-closed guard as the ONLY
	// thing standing between a not-assessed PR and a merge --
	// ComputeEligible checks it again regardless.
	probeInput := autoapproval.EligibilityInput{
		Verdict:              record.Verdict,
		VerdictAssessed:      true,
		VerdictHeadSHA:       record.HeadSHA,
		VerdictBaseRef:       record.Context.BaseRef,
		VerdictBaseSHA:       record.Context.BaseSHA,
		VerdictAncestorChain: record.Context.AncestorChain,
		VerdictPolicyVersion: record.Context.PolicyVersion,
		CurrentHeadSHA:       target.HeadSHA,
		CurrentBaseRef:       target.BaseRef,
		CurrentBaseSHA:       record.Context.BaseSHA, // assumed equal -- see doc comment above
		// CurrentAncestorChain (round-10 finding B) mirrors CurrentBaseSHA's
		// own identical "assumed equal to the verdict's own recorded
		// value" leniency immediately above, for the identical reason:
		// the REAL, live-resolved chain (currentAncestorChain, computed
		// further down this function, right before the final
		// ComputeEligible call) is deferred past this probe exactly like
		// the real currentBaseSHA is, so a PR already ineligible on some
		// OTHER, cheaper-to-check criterion never pays for a live GitHub
		// call this probe's own early-refusal makes moot. Never
		// target.AncestorChain's own raw ref+sha pairs here -- that
		// reads GitHub's own CACHED per-PR stack field, the same shape
		// finding F1 already proved stale-by-design, which is exactly
		// what made this whole comparison verify nothing before this fix.
		CurrentAncestorChain:       record.Context.AncestorChain,
		BaseAdvancedWithoutRewrite: true, // moot: the assumed SHA equality above already bypasses this check
		CIGreen:                    ciGreen,
		HasNeedsHumanLabel:         hasNeedsHuman,
		ChangedFileCount:           changedFileCount,
		TouchedBlastRadius:         touchedBlastRadius,
		TouchedBlastRadiusKnown:    touchedBlastRadiusKnown,
	}
	if _, probeReason := autoapproval.ComputeEligible(probeInput, cfg); probeReason != autoapproval.ReasonNone {
		return false, "", fmt.Sprintf("this pull request no longer meets the auto-approval eligibility criteria: %s", probeReason), nil
	}

	// The probe passed: on every criterion except base freshness, this PR
	// clears the bar, so the live base-branch-tip resolution (and, if
	// needed, the fast-forward-ancestry confirmation) now genuinely
	// decides whether it stays eligible.
	//
	// CurrentBaseSHA (finding F1 (§21.1's amendment)) is deliberately NOT
	// target.BaseSHA -- that field is GitHub's own per-PR CACHED
	// `base.sha` snapshot (ports.OpenPR.BaseSHA's own doc comment),
	// refreshed on GitHub's own schedule rather than on every push to the
	// base branch, and verified against real GitHub PRs to lag by an
	// unknown, sometimes month-scale margin. Comparing that cached field
	// against ITSELF (the verdict side reads the identical field, via
	// reviewcontext.Fetch's own pre-fix sourcing) detects nothing: the
	// SHA half of the freshness gate would pass whether or not the base
	// branch had actually moved. This calls the SAME ResolveBranchSHA a
	// verdict's own context was anchored to (internal/app/reviewcontext.
	// Fetch), so a genuine advance of the base branch's real tip between
	// verdict-time and merge-time now surfaces as a real SHA mismatch --
	// closing the exact hazard base_ref alone could never see: "whose
	// parent moved beneath it."
	//
	// A resolution failure fails CLOSED, but (H2, fifth adversarial-review
	// round) with its own dedicated, logged, early return immediately
	// below -- never by silently blanking CurrentBaseSHA and letting
	// execution fall through to autoapproval.ComputeEligible's own
	// empty-base-sha guard (finding F2, ReasonBaseSHAUnknown), which is
	// worded for a fact that was never recorded at all, not for a live
	// lookup that failed just now.
	//
	// Bounded by platform.Timeouts.DecisionInboxResolveBranchSHATimeout
	// (G4, fourth adversarial-review round): this call previously ran on
	// the bare, unbounded ctx this function was handed -- the SAME
	// unbounded-live-GitHub-call-on-the-merge-gate-action-path shape E4
	// (third round) had already fixed for the IsAncestor call below, one
	// call site over. See that field's own doc comment
	// (platform/timeouts.go) for which call site(s) it bounds.
	resolveCtx, cancel := context.WithTimeout(ctx, deps.Timeouts.DecisionInboxResolveBranchSHATimeout)
	currentBaseSHA, _, resolveErr := sourceControl.ResolveBranchSHA(resolveCtx, ports.ResolveBranchSHASpec{
		Owner:  target.Owner,
		Repo:   target.Repo,
		Branch: target.BaseRef,
		Token:  token,
	})
	cancel()
	if resolveErr != nil {
		// H2 (fifth adversarial-review round): previously swallowed with
		// no log, and currentBaseSHA fell through blank to
		// ComputeEligible's own ReasonBaseSHAUnknown ("this pull request's
		// base commit could not be established") -- a message written for
		// a fact that was NEVER recorded (VerdictBaseSHA/CurrentBaseSHA
		// never populated at all), not for a live lookup that simply
		// failed just now, and therefore indistinguishable to whoever
		// read it from a permanent refusal. Mirrors the IsAncestor
		// failure a few lines below (E6, third round; ordering fixed G3,
		// fourth round): logged, and returned EARLY with its own honest,
		// distinctly-worded reason, never falling through to a reason
		// that never mentions a live check failed at all. The probe above
		// already confirmed every OTHER criterion passes, so a failure
		// here really is the one and only thing standing between this PR
		// and eligibility -- the same "try again shortly" honesty
		// guarantee G3 established for the ancestor check below applies
		// here too.
		platform.Logger(ctx).Warn("decisioninbox: resolve base branch's live tip failed, refusing merge -- could not confirm the pull request's current base commit", "error", resolveErr, "repo_full_name", repoFullName, "pr_number", prNumber)
		return false, "", "this pull request's base commit could not be confirmed (a live check failed) -- try again shortly", nil
	}

	// baseAdvancedWithoutRewrite (D3, second adversarial-review round;
	// timeout/logging/message fixed, E4/E6, third round) tolerates an
	// ORDINARY, unrelated merge landing on the base branch between review
	// and merge -- "any unrelated merge to trunk permanently disqualifies
	// a verdict" is the exact failure this closes. Only even attempted
	// when it could possibly matter: the base ref is unchanged (a real
	// retarget still refuses unconditionally, autoapproval.
	// ComputeEligible's own doc comment) and the base sha genuinely
	// differs on both sides (both non-empty, since an empty side already
	// fails its own ReasonBaseSHAUnknown check regardless of this field).
	// See autoapproval.BaseAdvancedWithoutRewrite's own doc comment
	// (eligibility.go) for what a confirmed "yes" here actually
	// establishes and what it does not -- corrected twice over (E2, then
	// G1, fourth round) after two successive claims that this call's own
	// "yes" provably bounds the fresh diff were shown false against real
	// git; this call site deliberately never repeats either claim
	// itself, so there is exactly one place left to keep correct.
	//
	// Bounded by platform.Timeouts.DecisionInboxIsAncestorTimeout (E4,
	// third round): this call previously ran on the bare, unbounded ctx
	// this function was handed -- an unbounded live GitHub call sitting
	// on the merge-gate action path, the one place in this package that
	// is NOT allowed to serve a stale/cached answer (this function's own
	// top doc comment). SCMCache.IsAncestor's identical call
	// (scmcache.go) already bounds itself with this SAME field; this call
	// site was the one place that field's own doc comment claimed to
	// cover but did not.
	//
	// A resolution failure is logged (E6, third round: this call
	// previously swallowed ancestorErr with no log at all, unlike its
	// read-model sibling in aggregate.go, which already logs) and returns
	// EARLY with its own distinct, honest reason -- never falling through
	// to ComputeEligible's generic ReasonBaseMoved, which reads "the pull
	// request's base has changed since this verdict was produced" and
	// would otherwise tell the caller a fact this function never actually
	// established here: the base SHA genuinely did change (that part is
	// confirmed, or this branch would not be reached at all), but
	// whether that change was a safe, ordinary fast-forward or a genuine
	// rewrite is exactly what this failed call could not determine.
	// "The base changed" and "we could not check whether tolerating that
	// change was safe" are different facts, and conflating them tells an
	// operator retrying is pointless when it may well not be. Mirrors
	// ReviewDecisionDegraded's own identical "a degraded read fails
	// closed with a reason naming the degradation, not a fabricated
	// verdict" precedent a few lines above in this same function.
	//
	// G3 (fourth adversarial-review round): unlike before this fix, the
	// "try again shortly" reason returned below is now ALWAYS the honest
	// one -- the probe above already confirmed every OTHER criterion
	// passes, so a failure here really is the one and only thing
	// standing between this PR and eligibility, never a transient
	// message papering over some unrelated, permanent refusal (a
	// needs-human label, a stale verdict, an already-red build, ...) the
	// caller was never told about.
	var baseAdvancedWithoutRewrite bool
	if record.Context.BaseRef == target.BaseRef && record.Context.BaseSHA != "" && currentBaseSHA != "" && record.Context.BaseSHA != currentBaseSHA {
		ancestorCtx, cancel := context.WithTimeout(ctx, deps.Timeouts.DecisionInboxIsAncestorTimeout)
		confirmed, ancestorErr := sourceControl.IsAncestor(ancestorCtx, ports.IsAncestorSpec{
			Owner:      target.Owner,
			Repo:       target.Repo,
			Ancestor:   record.Context.BaseSHA,
			Descendant: currentBaseSHA,
			Token:      token,
		})
		cancel()
		if ancestorErr != nil {
			platform.Logger(ctx).Warn("decisioninbox: resolve base-advanced-without-rewrite ancestry failed, refusing merge -- could not confirm whether the base's forward movement was safe to tolerate", "error", ancestorErr, "repo_full_name", repoFullName, "pr_number", prNumber)
			return false, "", "this pull request's base has changed since this verdict was produced, and whether that change was safe to tolerate could not be confirmed (a live check failed) -- try again shortly", nil
		}
		baseAdvancedWithoutRewrite = confirmed
	}

	// currentAncestorChain (round-10 finding B) mirrors currentBaseSHA's
	// own identical "never the cached field, always a live resolution"
	// discipline immediately above, one link further: target.AncestorChain
	// (ports.OpenPR's own field) is GitHub's per-PR CACHED stack object,
	// the exact shape finding F1 already proved stale-by-design for the
	// immediate base -- comparing it against record.Context.AncestorChain
	// (itself now ALSO live-resolved at review-context-fetch time,
	// internal/app/reviewcontext.Fetch) would otherwise compare a live
	// fact against a cached one, unequal by construction, exactly the
	// hazard F1 closed for CurrentBaseSHA. §17.6 bounds this to AT MOST
	// ONE link today, so this is at most one further live call -- the
	// ref itself (a branch name) is trusted from the cached read, exactly
	// like BaseRef is; only the SHA is re-resolved live.
	// currentAncestorChain's own zero value (nil) is the correct answer
	// whenever target.AncestorChain reports no link at all, or the live
	// resolution below comes back empty -- never a link with a stale or
	// empty SHA, mirroring AncestorChainFromStack's own identical
	// empty-liveSHA-degrades-to-no-link discipline
	// (internal/domain/review/context.go).
	var currentAncestorChain []review.AncestorLink
	if len(target.AncestorChain) > 0 && target.AncestorChain[0].Ref != "" {
		ancestorSHACtx, cancel := context.WithTimeout(ctx, deps.Timeouts.DecisionInboxResolveBranchSHATimeout)
		liveAncestorSHA, _, liveAncestorErr := sourceControl.ResolveBranchSHA(ancestorSHACtx, ports.ResolveBranchSHASpec{
			Owner:  target.Owner,
			Repo:   target.Repo,
			Branch: target.AncestorChain[0].Ref,
			Token:  token,
		})
		cancel()
		if liveAncestorErr != nil {
			platform.Logger(ctx).Warn("decisioninbox: resolve ancestor chain's own live tip failed, refusing merge -- could not confirm the pull request's current ancestor chain", "error", liveAncestorErr, "repo_full_name", repoFullName, "pr_number", prNumber)
			return false, "", "this pull request's ancestor chain could not be confirmed (a live check failed) -- try again shortly", nil
		}
		if liveAncestorSHA != "" {
			currentAncestorChain = []review.AncestorLink{{Ref: target.AncestorChain[0].Ref, SHA: liveAncestorSHA}}
		}
	}

	eligible, eligReason := autoapproval.ComputeEligible(autoapproval.EligibilityInput{
		Verdict:                    record.Verdict,
		VerdictAssessed:            true,
		VerdictHeadSHA:             record.HeadSHA,
		VerdictBaseRef:             record.Context.BaseRef,
		VerdictBaseSHA:             record.Context.BaseSHA,
		VerdictAncestorChain:       record.Context.AncestorChain,
		VerdictPolicyVersion:       record.Context.PolicyVersion,
		CurrentHeadSHA:             target.HeadSHA,
		CurrentBaseRef:             target.BaseRef,
		CurrentBaseSHA:             currentBaseSHA,
		CurrentAncestorChain:       currentAncestorChain,
		BaseAdvancedWithoutRewrite: baseAdvancedWithoutRewrite,
		CIGreen:                    ciGreen,
		HasNeedsHumanLabel:         hasNeedsHuman,
		ChangedFileCount:           changedFileCount,
		TouchedBlastRadius:         touchedBlastRadius,
		TouchedBlastRadiusKnown:    touchedBlastRadiusKnown,
	}, cfg)
	if !eligible {
		return false, "", fmt.Sprintf("this pull request no longer meets the auto-approval eligibility criteria: %s", eligReason), nil
	}

	return true, target.HeadSHA, "", nil
}
