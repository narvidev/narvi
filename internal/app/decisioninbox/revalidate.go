package decisioninbox

import (
	"context"
	"errors"
	"fmt"

	"github.com/narvidev/narvi/internal/app/ports"
	"github.com/narvidev/narvi/internal/app/reviewfreshness"
	appreviewverdict "github.com/narvidev/narvi/internal/app/reviewverdict"
	"github.com/narvidev/narvi/internal/domain/autoapproval"
	"github.com/narvidev/narvi/internal/domain/reposource"
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
// ONE deliberate exception (finding F4, adversarial review): this
// function alone passes honorAcceptance=true to revalidateCore --
// RevalidateForAutoMerge below always passes false. §21.1b's own
// acceptance is "a human chose to proceed despite it", recorded at the
// moment a maintainer+ clicks Accept; an unattended worker later
// consuming that SAME authorisation, with no human present at merge
// time, is a human-judgment waiver firing with nobody there to have
// judged anything -- see revalidateCore's own doc comment for the full
// reasoning.
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
//
// viaAcceptance/acceptanceID (finding F1, adversarial review) report
// whether ok=true was reached ONLY because an applicable acceptance
// waived a human-judgment criterion the engine itself would have
// refused on -- see revalidateCore's own doc comment for the full "why"
// this must never be discarded: a merge this is true for is not evidence
// the engine's own judgment stood, and the caller (httpapi.
// MergePullRequest) must record it as a DIFFERENT outcome than a clean
// auto-approval, never fold it into RecordConfirmed's own "confirmed"
// bucket. Both are the zero value (false, "") whenever ok=false, and
// acceptanceID is "" whenever viaAcceptance is false -- a caller must
// never read acceptanceID without first checking viaAcceptance.
func RevalidateForMerge(ctx context.Context, deps Deps, sourceControl ports.SourceControl, actorGitHubID, repoFullName string, prNumber int, token string) (ok bool, headSHA string, reason string, viaAcceptance bool, acceptanceID string, err error) {
	prs, truncated, err := sourceControl.ListOpenPRsForUser(ctx, ports.ListOpenPRsForUserSpec{GitHubExternalID: actorGitHubID, Token: token})
	if err != nil {
		return false, "", "", false, "", err
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
			return false, "", "", false, "", fmt.Errorf("decisioninbox: revalidate for merge: could not confirm this pull request's current state (a degraded/partial GitHub read) -- please retry")
		}
		return false, "", "this pull request is no longer open, or no longer assigned to you", false, "", nil
	}

	return revalidateCore(ctx, deps, sourceControl, nil, token, repoFullName, prNumber, *target, true)
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
// unchanged... a deliberate reuse, not a parallel merge path." -- WITH
// ONE deliberate exception (finding F4, adversarial review): this
// function passes honorAcceptance=false, always -- an unattended
// auto-merge tick never consults review_verdict_acceptances at all, so a
// PR ineligible only because of a waived human-judgment criterion
// (§21.1b) never merges through this path, no matter how applicable an
// acceptance on file would otherwise be. §21.1b's own acceptance is "a
// human chose to proceed despite it" -- authorising THIS human's own
// next Merge click, not arming an unattended worker to act on that
// judgment indefinitely with nobody present. See revalidateCore's own
// doc comment for the full reasoning, and note the worker's own
// candidate list (internal/app/automerge.Worker.pumpRepo) is additionally
// pre-filtered to Shippable==auto verdicts -- so ReasonNotShippableAuto
// was already unreachable here before this fix; ReasonDiffTooLarge was
// not, and is what this fix actually closes.
//
// memo (§21.2) is the worker's per-tick memo of base branches' required
// checks (RequiredChecksMemo): candidates into the same base read its
// requirements once per tick. nil reads them on every call.
//
// found=false (GetOpenPR's own confirmed-404 signal) is reported as a
// plain ok=false/reason, mirroring RevalidateForMerge's own "no longer
// open" case above -- a PR closed/merged through some other path between
// discovery and this call is an ordinary, expected race, never an error.
func RevalidateForAutoMerge(ctx context.Context, deps Deps, sourceControl ports.SourceControl, memo *RequiredChecksMemo, repoFullName string, prNumber int, botToken string) (ok bool, headSHA string, reason string, viaAcceptance bool, acceptanceID string, err error) {
	owner, repo, splitOK := reposource.SplitFullName(repoFullName)
	if !splitOK {
		return false, "", "", false, "", fmt.Errorf("decisioninbox: revalidate for auto-merge: repoFullName %q is not shaped owner/repo", repoFullName)
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
		return false, "", "", false, "", err
	}
	// H3 (fifth adversarial-review round; corrected, sixth round -- the
	// previous version of this paragraph asserted a uniform failure model
	// for all five of GetOpenPR's sub-calls, sourced to
	// buildOpenPRFromDetail's own doc comment, which makes no such claim
	// and is wrong for two of the five). fetchOpenPRDetail's own failure
	// is a real Go error, already caught by this function's err != nil
	// check two lines above -- a deadline firing during THAT call cannot
	// reach the branch below at all. fetchReviewDecision,
	// fetchChangedFilePaths and (since fixed) fetchCIConclusionLive each
	// swallow their own failure into a degraded field instead, per their
	// own doc comments (listopenprs.go, ports.OpenPR.
	// ReviewDecisionDegraded/ChangedFilesListDegraded/
	// CIConclusionDegraded) -- fetchCIConclusionLive USED TO carry no
	// degraded field for either of its two GETs at all, so a failed call
	// contributed nothing and a status GET confirming "success" beside a
	// failed check-runs GET produced a confident CIConclusionSuccess:
	// only a failure of BOTH GETs fell back to the honest
	// CIConclusionUnknown default. Fixed: CIConclusionDegraded is now
	// this composite's own third degraded signal, and ComputeEligible
	// refuses on it (ReasonCIConclusionDegraded) exactly like it already
	// refused on ReviewDecisionDegraded/ChangedFilesListDegraded's own
	// callers refusing via revalidateCore's hard block and
	// ReasonBlastRadiusUnknown respectively -- a half-read CI composite
	// can no longer produce an eligible PR. What THIS guard (below) closes
	// is a DIFFERENT, narrower hazard: a deadline firing partway through
	// GetOpenPR's own five-call composite still returns here with
	// err == nil and a target reflecting whichever later sub-call the
	// deadline cut short. Left unchecked, that renders downstream as an
	// ordinary, permanent-looking eligibility refusal rather than the
	// transient, retry-worthy timeout it actually was. Detected
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
		return false, "", "", false, "", fmt.Errorf("decisioninbox: revalidate for auto-merge: get open pr timed out partway through its own five-call fetch (GitHubGetOpenPRTimeout) -- refusing rather than trusting whichever sub-call was cut short: %w", context.DeadlineExceeded)
	}
	if !found {
		return false, "", "this pull request is no longer open", false, "", nil
	}

	return revalidateCore(ctx, deps, sourceControl, memo, botToken, repoFullName, prNumber, target, false)
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
//
// honorAcceptance (finding F4, adversarial review) is the ONE thing that
// deliberately differs between this core's two callers: true from
// RevalidateForMerge (a human is, right now, clicking Merge), false from
// RevalidateForAutoMerge (nobody is present). When false, this function
// never looks up review_verdict_acceptances at all -- accepted stays
// false unconditionally, so ComputeEligibleWithAcceptance below behaves
// EXACTLY like plain ComputeEligible, and a PR ineligible only via a
// waived human-judgment criterion (§21.1b: ReasonNotShippableAuto or
// ReasonDiffTooLarge) never merges unattended on the strength of an
// acceptance nobody re-confirmed at click time. Every OTHER check in
// this function is unaffected by honorAcceptance and stays byte-for-byte
// identical between the two callers, preserving this function's own
// "never independently drift" property for everything acceptance does
// NOT touch.
//
// viaAcceptance/acceptanceID (finding F1, adversarial review) are the
// third and fourth return values ComputeEligibleWithAcceptance's own
// doc comment names -- forwarded here, never discarded with `_`, so a
// caller (httpapi.MergePullRequest, internal/app/automerge's own worker)
// can tell "the engine's own judgment stood" apart from "this merged
// only because a human's acceptance waived a refusal" and record the
// SAME outcome the contradiction-rate read model was calibrated to
// never blur (§21.2). Both are the zero value whenever ok=false or
// honorAcceptance=false, since neither caller needs them in either case.
// The refusal-reason strings below are each defined ONCE and shared by
// every site that must describe the SAME underlying fact to a human --
// revalidateCore's own return values immediately below, AND
// buildPROpenItem's AcceptanceMergeBlockedReason assignments
// (aggregate.go), which describe what these SAME mandatory criteria
// found on a given row WITHOUT re-running revalidateCore itself (T6,
// round 4, adversarial review). Round-5 finding V3: these used to be six
// independent, hand-typed string literals in aggregate.go -- one
// (reasonReviewDecisionDegraded) had ALREADY drifted from this file's
// own copy, silently dropping its trailing clause, exactly the kind of
// paraphrase a maintainer changing the wording here has no way to
// notice. A single definition makes that drift structurally impossible
// rather than merely commented against.
const (
	reasonHandoffItem            = "this pull request is a handoff item, not an ordinary code-review merge decision"
	reasonReviewDecisionDegraded = "this pull request's review decision could not be confirmed (a degraded GitHub read) -- failing closed rather than trusting an unconfirmed read"
	reasonChangesRequested       = "this pull request has changes requested by a reviewer"
	reasonNotPlatformAuthored    = "this pull request was not authored by a platform session"
	reasonOpenFinding            = "this pull request has an open, unresolved review finding"
	reasonBaseCommitUnconfirmed  = "this pull request's base commit could not be confirmed (a live check failed) -- try again shortly"

	// reasonRequiredChecksNotRead is the read model's acceptance readout
	// when GitHub outbound is off (aggregate.go): the inbox reads no base
	// branch's required checks without the bot, so it cannot show the row
	// as mergeable. A configuration, not a failure to retry -- worded so.
	reasonRequiredChecksNotRead = "the checks this pull request's base branch requires are not read in the inbox while this deployment's GitHub outbound is off"

	// reasonNoLongerMeetsCriteriaFmt is this function's own probe-refusal
	// AND final-refusal wording -- used at BOTH points below (the probe,
	// before any live SCM call, and the final post-freshness-check
	// re-evaluation) so the two never independently drift from each
	// other either. NOT shared with aggregate.go's own
	// AcceptanceMergeBlockedReason "...even with its accepted override
	// applied" case: that field is a simpler, acceptance-specific
	// summary that deliberately never interpolates the engine's own live
	// Reason detail (%s here) -- a different fact, not a copy of this
	// one.
	reasonNoLongerMeetsCriteriaFmt = "this pull request no longer meets the auto-approval eligibility criteria: %s"
)

func revalidateCore(ctx context.Context, deps Deps, sourceControl ports.SourceControl, memo *RequiredChecksMemo, token string, repoFullName string, prNumber int, target ports.OpenPR, honorAcceptance bool) (ok bool, headSHA string, reason string, viaAcceptance bool, acceptanceID string, err error) {
	if target.Draft {
		return false, "", "this pull request is a draft", false, "", nil
	}

	excluded, exErr := deps.SentinelFixes.ExistsByFixPRNumber(ctx, repoFullName, int32(prNumber))
	if exErr != nil {
		return false, "", "", false, "", fmt.Errorf("decisioninbox: revalidate for merge: check sentinel-fix exclusion: %w", exErr)
	}
	if excluded {
		return false, "", "this pull request is a sentinel-auto-fix follow-up -- it merges automatically once its own checks pass, never through this endpoint", false, "", nil
	}

	hasNeedsHuman, _, isHandoffPR := classifyPRLabels(target.Labels)
	if isHandoffPR {
		return false, "", reasonHandoffItem, false, "", nil
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
		return false, "", reasonReviewDecisionDegraded, false, "", nil
	}
	if target.HasChangesRequested {
		return false, "", reasonChangesRequested, false, "", nil
	}

	if !isPlatformAuthored(ctx, deps, target.HTMLURL) {
		return false, "", reasonNotPlatformAuthored, false, "", nil
	}

	openFindings, findingsErr := countOpenFindings(ctx, deps, repoFullName, prNumber)
	if findingsErr != nil {
		return false, "", "", false, "", fmt.Errorf("decisioninbox: revalidate for merge: count open findings: %w", findingsErr)
	}
	if openFindings > 0 {
		// Mirrors buildPROpenItem's own identical "kept as its own,
		// separate AND-condition" reasoning (aggregate.go) -- never
		// folded into the eligibility engine itself.
		return false, "", reasonOpenFinding, false, "", nil
	}
	ciGreen := target.CIConclusion == ports.CIConclusionSuccess
	// ciConclusionDegraded is target.CIConclusionDegraded, verbatim --
	// wired into BOTH EligibilityInput literals below (the probe and the
	// final call), exactly like ciGreen itself, so a half-read CI
	// composite refuses at whichever of the two actually runs first.
	ciConclusionDegraded := target.CIConclusionDegraded

	record, hasVerdict, verdictErr := appreviewverdict.GetLatest(ctx, deps.ReviewVerdict, repoFullName, int32(prNumber))
	if verdictErr != nil {
		return false, "", "", false, "", fmt.Errorf("decisioninbox: revalidate for merge: get latest review verdict: %w", verdictErr)
	}
	if !hasVerdict {
		return false, "", "this pull request has no review verdict of record", false, "", nil
	}

	// acceptance ("human acceptance of a verdict the engine refuses",
	// §21.1b): a maintainer+ may have authorised proceeding past
	// ReasonNotShippableAuto/ReasonDiffTooLarge for THIS exact verdict
	// (record.ID) -- fetched once, here, so BOTH ComputeEligibleWithAcceptance
	// calls below (the probe and the final call) see the identical
	// answer. A genuine store error fails CLOSED (propagated as err,
	// mirroring every other store-error branch in this function's own
	// top doc comment) -- an acceptance lookup that cannot be confirmed
	// must never silently degrade to "no acceptance applies" AND must
	// never silently degrade to "an acceptance applies" either; erroring
	// out is the only honest answer. acceptanceOK=false (no active row,
	// or a row exists but Applicable reports it no longer binds to
	// record.ID, OR a NEWER review attempt has since run whether or not
	// it posted a verdict -- finding F1, adversarial review: verdict-id
	// equality alone cannot see a not_assessed attempt, since that
	// attempt posts no review_verdicts row at all) means accepted=false,
	// exactly like a PR that was never accepted at all -- Acceptance.
	// Applicable's own doc comment is what makes this the SAME code path
	// that also closes "a moved base makes it inapplicable":
	// ComputeEligibleWithAcceptance's own unconditional base/
	// ancestor-chain checks refuse regardless of accepted, so this line
	// alone need only answer "same verdict, no newer attempt, still not
	// revoked". hasNewerAttempt's own lookup error is ALSO fail-CLOSED,
	// propagated exactly like acceptanceErr immediately below -- an
	// action endpoint (this function, unlike buildPROpenItem's own
	// best-effort display read) must never proceed on an unconfirmed
	// attempt-freshness fact either direction.
	//
	// Gated on honorAcceptance (finding F4, adversarial review): when
	// false (the unattended auto-merge worker), this store is never even
	// READ -- accepted/acceptanceID stay at their zero values
	// unconditionally, so no acceptance on file, however applicable, can
	// influence this call. See this function's own top doc comment for
	// the full "why".
	var accepted bool
	if honorAcceptance {
		acceptance, acceptanceOK, acceptanceErr := appreviewverdict.GetActiveAcceptance(ctx, deps.ReviewVerdict.Acceptances, repoFullName, int32(prNumber))
		if acceptanceErr != nil {
			return false, "", "", false, "", fmt.Errorf("decisioninbox: revalidate for merge: get active review verdict acceptance: %w", acceptanceErr)
		}
		if acceptanceOK {
			hasNewerAttempt, newerErr := appreviewverdict.HasNewerReviewAttempt(ctx, deps.ReviewVerdict.Turns, acceptance)
			if newerErr != nil {
				return false, "", "", false, "", fmt.Errorf("decisioninbox: revalidate for merge: check newer review attempt: %w", newerErr)
			}
			if acceptance.Applicable(record.ID, hasNewerAttempt) {
				accepted = true
				acceptanceID = acceptance.ID
			}
		}
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
		return false, "", "", false, "", fmt.Errorf("decisioninbox: revalidate for merge: load eligibility config: %w", cfgErr)
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

	// requiredChecks is the base branch's required checks at target's head
	// (§21.2's "CI green means the required checks, not the checks that
	// reported"), read live with token -- the credential this path merges
	// with: the person's own token on a Merge click (so a Merge click never
	// depends on GitHub outbound, like the merge it makes), the bot's on
	// the auto-merge worker -- and evaluated against the checks target's
	// own CI read listed, the App behind an App's commit status identified
	// with the same token where a requirement names an App
	// (requiredchecks.go). The worker hands in one memo
	// per tick, so candidates into the same base share a read; a Merge
	// click hands in none and reads fresh. Read BEFORE the probe, unlike
	// the read model (computeRealEligibility): this function's refusal is
	// shown, as a 409 body or the auto-merge worker's log line, and a
	// required check that is still running or failed must be named in it
	// -- behind the probe it would refuse first as "CI is not green". A
	// failed read stands in the probe as "read, requiring nothing"
	// (probeRequiredChecks), so it refuses only in the final call, on
	// ReasonRequiredChecksUnknown, once every other criterion has passed:
	// a transient failure never masks a pull request's lasting reason (G3,
	// fourth round), and never falls back to the CI read alone.
	required, requiredErr := readRequiredChecksLive(ctx, deps, sourceControl, memo, requiredChecksSpec(target, token))
	if requiredErr != nil {
		platform.Logger(ctx).Warn("decisioninbox: read base branch's required checks failed -- eligibility will fail closed via ReasonRequiredChecksUnknown unless another criterion refuses first", "error", requiredErr, "repo_full_name", repoFullName, "pr_number", prNumber, "base_ref", target.BaseRef)
	}
	requiredChecks := requiredChecksFact(ctx, required, requiredErr, target, liveAppIDResolver(deps, sourceControl, token))

	// probe (G3/G4, fourth adversarial-review round) asks "setting the
	// base-freshness question aside entirely, is this PR already
	// ineligible" -- every criterion below is fully known WITHOUT any
	// live SCM call: head-SHA equality (VerdictHeadSHA/CurrentHeadSHA,
	// both already in hand), the verdict's own base-REF/policy-version
	// equality (record.Context vs. target, no new I/O, mirroring
	// CurrentHeadSHA's own identical sourcing), CI, Shippable, diff size,
	// and blast radius -- plus the base branch's required checks, read
	// just above as this function's one live call ahead of the probe
	// (requiredChecks' own comment says why), entering as read or, when
	// that read failed, as probeRequiredChecks' lenient stand-in. The
	// ANCESTOR CHAIN is NOT among these (round-11
	// finding E, corrected: a previous version of this paragraph listed
	// it alongside base-ref/policy-version as though the probe genuinely
	// compares it against target's own live value) -- CurrentAncestorChain
	// below is assigned record.Context.AncestorChain itself, the identical
	// value VerdictAncestorChain also is, so this probe's own
	// ancestorChainEqual comparison is ALWAYS trivially equal and can
	// never by itself refuse on ReasonAncestorChainChanged. D2 (round-12
	// sweep, execution-verified): the PREVIOUS version of this comment
	// extended that same "can never" claim to ReasonAncestorChainUnknown
	// too -- false, and executing ComputeEligible with both sides set to
	// the identical unknown-SHA marker (VerdictAncestorChain ==
	// CurrentAncestorChain == a single link with an empty SHA) refuses
	// with ReasonAncestorChainUnknown every time. That check
	// (ancestorChainHasUnknownLink) runs BEFORE the equality comparison
	// and inspects EACH side on its own terms, so an unknown marker baked
	// into the recorded verdict itself -- e.g. one posted while the
	// review-context fetch's own live ancestor resolution was failing --
	// refuses here regardless of how trivially the two sides agree. See
	// CurrentAncestorChain's own doc comment a few lines down for the
	// full "why", the same deferred-to-the-final-call treatment
	// CurrentBaseSHA gets, immediately below. CurrentBaseSHA is deliberately
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
		// AncestorChainAdvancedWithoutRewrite (round-11 finding A3) is
		// likewise moot here, for the identical reason: CurrentAncestorChain
		// is assigned the SAME value as VerdictAncestorChain immediately
		// above, so ancestorChainEqual's own per-link sha comparison this
		// flag would otherwise gate never even runs -- there is no sha
		// mismatch to tolerate when both sides are the identical slice.
		AncestorChainAdvancedWithoutRewrite: true,
		CIGreen:                             ciGreen,
		CIConclusionDegraded:                ciConclusionDegraded,
		RequiredChecks:                      probeRequiredChecks(requiredChecks), // the real fact once read; see requiredChecks above
		HasNeedsHumanLabel:                  hasNeedsHuman,
		ChangedFileCount:                    changedFileCount,
		TouchedBlastRadius:                  touchedBlastRadius,
		TouchedBlastRadiusKnown:             touchedBlastRadiusKnown,
	}
	// A session's result runs this probe's freshness half too
	// (reviewfreshness.ProbeInput), so the two answer a verdict the record
	// decides with the same reason
	// (TestAssess_AgreesWithTheMergePathOnRecordDecidedVerdicts).
	//
	// ComputeEligibleWithAcceptance, never a bare probeReason != ReasonNone
	// check: an applicable acceptance (accepted=true) can make eligible
	// true while STILL reporting the waived Reason (ReasonNotShippableAuto/
	// ReasonDiffTooLarge) for display -- see that function's own doc
	// comment. Checking the reason string alone here would misread "eligible,
	// via acceptance" as a refusal.
	if probeEligible, probeReason, _ := autoapproval.ComputeEligibleWithAcceptance(probeInput, cfg, accepted); !probeEligible {
		return false, "", fmt.Sprintf(reasonNoLongerMeetsCriteriaFmt, probeReason), false, "", nil
	}

	// The probe passed: on every criterion except base freshness, this PR
	// clears the bar, so the live facts -- the base branch's live tip, the
	// ancestor chain's, and, where either moved, whether it moved only
	// forward -- now genuinely decide whether it stays eligible.
	//
	// reviewfreshness.ReadLive reads those facts (row 182, §21.1b: a
	// session's result reads them through the same function and compares
	// them through the same autoapproval.CheckFreshness; the decision
	// inbox's cached read model still assembles its own copy -- see the
	// reviewfreshness package doc), and it
	// makes exactly the calls this function used to make inline, in the
	// same order, bounded by the same platform.Timeouts constants
	// (DecisionInboxResolveBranchSHATimeout, DecisionInboxIsAncestorTimeout
	// -- G4, fourth round, and E4, third round, bounded them): see its own
	// doc comment for why each fact is resolved live (finding F1 for the
	// base commit, round-10 finding B for the chain) and why an ancestry
	// check runs only where it could change the answer (D3, round-11 A3).
	//
	// A live check that fails refuses the merge here, logged, with its own
	// honest reason naming the check (H2, fifth round; E6, third round;
	// round-11 A1; round-12 D1) -- never by letting a blank value fall
	// through to a reason worded for a fact that was never recorded
	// (ReasonBaseSHAUnknown), or to ReasonBaseMoved, which would tell the
	// caller "the base changed" when what failed is whether tolerating
	// that change was safe, or to a nil chain, indistinguishable once
	// compared from "this PR was never in a stack at all". G3 (fourth
	// round): the probe above already confirmed every OTHER criterion
	// passes, so a failure here really is the one thing standing between
	// this PR and eligibility, and "try again shortly" is always the
	// honest thing to say.
	live, failure := reviewfreshness.ReadLive(ctx, deps.Timeouts, sourceControl, token, target, record.Context)
	if failure != nil {
		switch failure.Step {
		case reviewfreshness.StepResolveBase:
			platform.Logger(ctx).Warn("decisioninbox: resolve base branch's live tip failed, refusing merge -- could not confirm the pull request's current base commit", "error", failure.Err, "repo_full_name", repoFullName, "pr_number", prNumber)
			return false, "", reasonBaseCommitUnconfirmed, false, "", nil
		case reviewfreshness.StepBaseAncestry:
			platform.Logger(ctx).Warn("decisioninbox: resolve base-advanced-without-rewrite ancestry failed, refusing merge -- could not confirm whether the base's forward movement was safe to tolerate", "error", failure.Err, "repo_full_name", repoFullName, "pr_number", prNumber)
			return false, "", "this pull request's base has changed since this verdict was produced, and whether that change was safe to tolerate could not be confirmed (a live check failed) -- try again shortly", false, "", nil
		case reviewfreshness.StepAncestorRefUnreadable:
			platform.Logger(ctx).Warn("decisioninbox: ancestor chain link reported with no ref at all (a degraded stack read), refusing merge -- could not confirm the pull request's current ancestor chain", "repo_full_name", repoFullName, "pr_number", prNumber)
			return false, "", "this pull request's ancestor chain could not be confirmed (a live check failed) -- try again shortly", false, "", nil
		case reviewfreshness.StepResolveAncestor:
			platform.Logger(ctx).Warn("decisioninbox: resolve ancestor chain's own live tip failed, refusing merge -- could not confirm the pull request's current ancestor chain", "error", failure.Err, "repo_full_name", repoFullName, "pr_number", prNumber)
			return false, "", "this pull request's ancestor chain could not be confirmed (a live check failed) -- try again shortly", false, "", nil
		case reviewfreshness.StepAncestorAncestry:
			platform.Logger(ctx).Warn("decisioninbox: resolve ancestor-chain-advanced-without-rewrite ancestry failed, refusing merge -- could not confirm whether the ancestor chain's forward movement was safe to tolerate", "error", failure.Err, "repo_full_name", repoFullName, "pr_number", prNumber)
			return false, "", "this pull request's ancestor chain has changed since this verdict was produced, and whether that change was safe to tolerate could not be confirmed (a live check failed) -- try again shortly", false, "", nil
		default:
			// Unreachable: ReadLive reports only the steps above. Fails
			// closed, as an error, never as a pass.
			return false, "", "", false, "", fmt.Errorf("decisioninbox: revalidate for merge: live freshness read failed at an unknown step %q: %w", failure.Step, failure.Err)
		}
	}

	// ComputeEligibleWithAcceptance, threading the SAME accepted this
	// function's own probe call above already used -- accepted waives
	// ONLY ReasonNotShippableAuto/ReasonDiffTooLarge; every OTHER
	// criterion computed here (CI, blast radius, sensitive path, and
	// every freshness check this function's own live SCM calls above
	// just resolved) stays mandatory regardless (§21.1b).
	//
	// viaAcceptance (finding F1, adversarial review), UNLIKE the probe
	// call's own identical third return value above, is never discarded
	// here: this is the call that actually decides whether this PR
	// merges, so its own viaAcceptance is the one the caller must see --
	// see this function's own top doc comment for why silently dropping
	// it (as the code did before this fix) let an acceptance-driven
	// merge read, downstream, as indistinguishable evidence the engine's
	// own judgment was right.
	eligible, eligReason, viaAcceptance := autoapproval.ComputeEligibleWithAcceptance(autoapproval.EligibilityInput{
		Verdict:                             record.Verdict,
		VerdictAssessed:                     true,
		VerdictHeadSHA:                      record.HeadSHA,
		VerdictBaseRef:                      record.Context.BaseRef,
		VerdictBaseSHA:                      record.Context.BaseSHA,
		VerdictAncestorChain:                record.Context.AncestorChain,
		VerdictPolicyVersion:                record.Context.PolicyVersion,
		CurrentHeadSHA:                      live.HeadSHA,
		CurrentBaseRef:                      live.BaseRef,
		CurrentBaseSHA:                      live.BaseSHA,
		CurrentAncestorChain:                live.AncestorChain,
		BaseAdvancedWithoutRewrite:          live.BaseAdvancedWithoutRewrite,
		AncestorChainAdvancedWithoutRewrite: live.AncestorChainAdvancedWithoutRewrite,
		CIGreen:                             ciGreen,
		CIConclusionDegraded:                ciConclusionDegraded,
		RequiredChecks:                      requiredChecks,
		HasNeedsHumanLabel:                  hasNeedsHuman,
		ChangedFileCount:                    changedFileCount,
		TouchedBlastRadius:                  touchedBlastRadius,
		TouchedBlastRadiusKnown:             touchedBlastRadiusKnown,
	}, cfg, accepted)
	if !eligible {
		return false, "", fmt.Sprintf(reasonNoLongerMeetsCriteriaFmt, eligReason), false, "", nil
	}

	// acceptanceID is only ever meaningful alongside viaAcceptance=true
	// (accepted was computed above from THIS exact acceptance, if any) --
	// reported here rather than unconditionally so a caller that (against
	// this function's own contract) reads it without checking
	// viaAcceptance first still gets the honest "" rather than a stale id
	// left over from an acceptance that did not actually matter.
	if !viaAcceptance {
		acceptanceID = ""
	}
	return true, target.HeadSHA, "", viaAcceptance, acceptanceID, nil
}
