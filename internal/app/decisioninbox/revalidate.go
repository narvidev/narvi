package decisioninbox

import (
	"context"
	"fmt"

	"github.com/narvidev/narvi/internal/app/ports"
	appreviewverdict "github.com/narvidev/narvi/internal/app/reviewverdict"
	"github.com/narvidev/narvi/internal/domain/autoapproval"
	"github.com/narvidev/narvi/internal/domain/reposource"
	"github.com/narvidev/narvi/internal/domain/review"
)

// convertAncestorChain converts links (ports.PRAncestorLink, this port's
// own domain-free copy of the identical shape, ports.PRAncestorLink's own
// doc comment) into review.AncestorLink -- the one place this codebase
// converts between the two, mirroring how every other ports<->domain
// boundary in this file already converts (e.g. autoapproval.
// ClassifyChangedPaths over target.ChangedFiles, immediately below this
// function's own two real call sites). A nil links converts to a nil
// result, never an empty-but-non-nil slice -- both compare equal under
// autoapproval.ComputeEligible's own ancestorChainEqual (that function's
// own doc comment), so this is a preference for the common case, not a
// correctness requirement.
func convertAncestorChain(links []ports.PRAncestorLink) []review.AncestorLink {
	if links == nil {
		return nil
	}
	out := make([]review.AncestorLink, len(links))
	for i, l := range links {
		out[i] = review.AncestorLink{Ref: l.Ref, SHA: l.SHA}
	}
	return out
}

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

	target, found, err := sourceControl.GetOpenPR(ctx, owner, repo, prNumber, botToken)
	if err != nil {
		return false, "", "", err
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
	// would, never silently pass through as "no".
	if target.ReviewDecisionDegraded {
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
	// ChangedFileCount/TouchedBlastRadius are BOTH
	// derived here from target -- target is revalidateCore's own
	// already-fetched, server-side ports.OpenPR (RevalidateForMerge's
	// live ListOpenPRsForUser search, or RevalidateForAutoMerge's live
	// GetOpenPR call), never the posted verdict's own self-reported
	// FilesChanged/BlastRadius. No new I/O: every field read here was
	// already fetched by the SAME call that produced target.
	//
	// Phase 5 audit findings 1+2 (both fixed, the SAME root cause C1
	// fixed for the verdict's own self-report, now closed for THIS
	// adapter-fetched data too): ChangedFileCount is target.
	// ChangedFilesCount, GitHub's own authoritative scalar -- never
	// len(target.ChangedFiles), which githubapi caps at one page and
	// which used to also silently read as 0 whenever the underlying
	// GitHub fetch failed outright. TouchedBlastRadiusKnown is
	// !target.ChangedFilesListDegraded -- see that field's own doc
	// comment (ports.OpenPR) for the two independent ways it can go
	// true (a failed fetch, or a genuinely large diff whose listing was
	// truncated at GitHub's own one-page cap): either way,
	// ComputeEligible now refuses this PR rather than silently trusting
	// an incomplete-or-absent classification of target.ChangedFiles.
	// VerdictAssessed is unconditionally true here -- hasVerdict was
	// already confirmed true above (the !hasVerdict branch returns
	// earlier) -- but is still passed through explicitly, never left at
	// its own zero value, mirroring TouchedBlastRadiusKnown's own
	// identical "never rely on a caller forgetting" discipline: a future
	// edit to this function that moves or removes the early !hasVerdict
	// return must not silently reintroduce ReasonNotAssessed's own fail-
	// closed guard as the ONLY thing standing between a not-assessed PR
	// and a merge -- ComputeEligible checks it again regardless.
	//
	// VerdictBaseRef/VerdictBaseSHA/VerdictAncestorChain/
	// VerdictPolicyVersion (§21.1's amendment) come from record.Context
	// -- the SAME review_verdicts row record.HeadSHA itself came from,
	// resolved by appreviewverdict.GetLatest above. CurrentBaseRef/
	// CurrentAncestorChain mirror CurrentHeadSHA's own identical "target
	// is this function's own already-fetched, LIVE ports.OpenPR"
	// sourcing -- no new I/O.
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
	// A resolution failure fails CLOSED to an empty CurrentBaseSHA,
	// mirroring every other genuinely-unknown-fact convention this
	// function already applies (ReviewDecisionDegraded, ChangedFilesListDegraded)
	// -- autoapproval.ComputeEligible's own empty-base-sha guard (finding
	// F2) then refuses on its own distinct reason, rather than this
	// function inventing a SECOND fail-closed path that could drift from
	// the engine's own.
	currentBaseSHA, _, resolveErr := sourceControl.ResolveBranchSHA(ctx, ports.ResolveBranchSHASpec{
		Owner:  target.Owner,
		Repo:   target.Repo,
		Branch: target.BaseRef,
		Token:  token,
	})
	if resolveErr != nil {
		currentBaseSHA = ""
	}

	// baseAdvancedWithoutRewrite (D3, second adversarial-review round)
	// tolerates an ORDINARY, unrelated merge landing on the base branch
	// between review and merge -- "any unrelated merge to trunk
	// permanently disqualifies a verdict" is the exact failure this
	// closes. Only even attempted when it could possibly matter: the base
	// ref is unchanged (a real retarget still refuses unconditionally,
	// autoapproval.ComputeEligible's own doc comment) and the base sha
	// genuinely differs on both sides (both non-empty, since an empty
	// side already fails its own ReasonBaseSHAUnknown check regardless of
	// this field). See ports.SourceControl.IsAncestor's own doc comment
	// for what this call answers and why it is sound: a three-dot diff is
	// defined against the merge-base of (base, head), which does not move
	// merely because the base branch's tip advances, as long as its OLD
	// tip remains an ancestor of the new one.
	//
	// A resolution failure fails CLOSED to false (never confirmed),
	// mirroring currentBaseSHA's own identical "a resolution failure
	// fails CLOSED" convention immediately above -- ComputeEligible then
	// refuses on ReasonBaseMoved exactly as it did before this field
	// existed, never a new, silently-permissive default.
	var baseAdvancedWithoutRewrite bool
	if record.Context.BaseRef == target.BaseRef && record.Context.BaseSHA != "" && currentBaseSHA != "" && record.Context.BaseSHA != currentBaseSHA {
		confirmed, ancestorErr := sourceControl.IsAncestor(ctx, ports.IsAncestorSpec{
			Owner:      target.Owner,
			Repo:       target.Repo,
			Ancestor:   record.Context.BaseSHA,
			Descendant: currentBaseSHA,
			Token:      token,
		})
		if ancestorErr == nil {
			baseAdvancedWithoutRewrite = confirmed
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
		CurrentAncestorChain:       convertAncestorChain(target.AncestorChain),
		BaseAdvancedWithoutRewrite: baseAdvancedWithoutRewrite,
		CIGreen:                    ciGreen,
		HasNeedsHumanLabel:         hasNeedsHuman,
		ChangedFileCount:           target.ChangedFilesCount,
		TouchedBlastRadius:         autoapproval.ClassifyChangedPaths(target.ChangedFiles),
		TouchedBlastRadiusKnown:    !target.ChangedFilesListDegraded,
	}, cfg)
	if !eligible {
		return false, "", fmt.Sprintf("this pull request no longer meets the auto-approval eligibility criteria: %s", eligReason), nil
	}

	return true, target.HeadSHA, "", nil
}
