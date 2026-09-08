// This file (aggregate.go) implements Build -- the decision inbox's own
// read-model aggregation ("decision inbox: read model + API",
// §16). A READ MODEL, not new state (§16.2): every fact below is derived
// from Postgres rows and SourceControl reads that already exist for other
// reasons; this package writes NOTHING back to Postgres and introduces no
// new table.
//
// # The four kinds, and exactly how each is decided
//
//   - KindReadyToMerge: an open, non-draft PR the actor is assigned to
//     (directly, as requested reviewer, or via CODEOWNERS), authored by a
//     platform session (an artifacts row of type 'pr' exists for this
//     PR's own URL -- internal/app/sessionactor/pushpr.go's own
//     recordPRArtifact, §9.3), CI green at head, and passing this
//     Step's own INTERIM auto-approval-eligibility stand-in (internal/
//     domain/decisioninbox.ComputeAutoApprovalEligible -- see that
//     function's own doc comment for the full justification: §21's real
//     engine, and review_verdicts to back it, do not exist yet as of this
//     Step, confirmed empirically before writing this file).
//   - KindNeedsReview: every OTHER open, non-draft, non-handoff,
//     non-§17-excluded PR the actor is assigned to -- i.e. the residual
//     bucket for an assigned PR that is not (yet) ready_to_merge. §16.1's
//     own prose names "verdict >= medium or a formal review is gated" as
//     the typical case; this Step reads that as descriptive of why a PR
//     usually lands here, not as an additional AND-gate that could leave
//     a PR the user is genuinely assigned to invisible in their own
//     inbox merely because, say, it has never been reviewed at all yet
//     (no risk label whatsoever) -- every legitimately assigned PR
//     appears SOMEWHERE.
//   - KindAwaitingApproval: plan-mode plans the actor is entitled to
//     approve (authz.Authorize(ActionApprovePlan, ...), the exact same
//     verdict httpapi.canActOnPlan already renders for the real approve/
//     reject endpoints) PLUS handoff items (§14.4): a PR the actor is
//     assigned to that also carries the "handoff" label rides this kind
//     instead of needs_review/ready_to_merge, since a handoff decision
//     ("send to engineering?") is not an ordinary code-review action.
//     KNOWN SCOPE LIMIT, documented rather than silently left a gap: this
//     Step discovers handoff PRs via the SAME assignee/requested-reviewer
//     mechanism as any other PR -- a handoff PR whose deciding PM is
//     neither assigned nor a requested reviewer on the resulting GitHub
//     PR will not surface here. §14.4's own v2 (a dedicated child-session
//     escalation) is explicitly deferred already ("only if handoff volume
//     justifies it"); building a SEPARATE discovery mechanism ahead of
//     that need would be speculative scope this Step does not add.
//   - KindNeedsAttention (ADMIN ONLY, §16.1's own parenthetical, enforced
//     by Build itself -- never populated for a non-admin actor): failed,
//     resumable sessions; automations auto-paused (consecutive_failures
//     at or past automation.AutoPauseThreshold when status is 'paused' --
//     the ONLY durable signal that distinguishes an auto-pause from a
//     direct admin PauseAutomation call, since both write the identical
//     status='paused' transition and this table carries no separate
//     pause-reason column); dead-lettered outbox deliveries.
//
// # Structural exclusion (§17)
//
// Every PR candidate is checked against sentinel_fixes.fix_pr_number
// (SentinelFixStore.ExistsByFixPRNumber) BEFORE it is ever classified into
// a kind -- a sentinel-auto-fix follow-up PR is excluded outright, never
// merely filtered by a label or a convention someone could forget to
// apply consistently (§16.1: "Make this a structural exclusion, not a
// filter someone can forget").

package decisioninbox

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/narvidev/narvi/internal/adapters/outbound/postgres"
	"github.com/narvidev/narvi/internal/adapters/outbound/postgres/sqlcgen"
	"github.com/narvidev/narvi/internal/app/ports"
	appreviewverdict "github.com/narvidev/narvi/internal/app/reviewverdict"
	"github.com/narvidev/narvi/internal/domain/authz"
	"github.com/narvidev/narvi/internal/domain/autoapproval"
	"github.com/narvidev/narvi/internal/domain/automation"
	"github.com/narvidev/narvi/internal/domain/decisioninbox"
	"github.com/narvidev/narvi/internal/domain/handoff"
	"github.com/narvidev/narvi/internal/domain/reviewpost"
	"github.com/narvidev/narvi/internal/platform"
)

// maxAttentionRowsPerSource bounds each of the three needs_attention
// sub-scans (failed sessions, paused automations, dead-letter outbox) --
// §21.1's own "bounded from day one" discipline.
const maxAttentionRowsPerSource = 100

// Deps bundles every dependency Build needs -- constructed once at
// process wiring time (cmd/control-plane/main.go), mirroring every other
// app-layer Deps struct in this codebase.
type Deps struct {
	Plans          *postgres.PlanStore
	Sessions       *postgres.SessionStore
	Participants   *postgres.ParticipantStore
	Automations    *postgres.AutomationStore
	Outbox         *postgres.OutboxStore
	ReviewFindings *postgres.ReviewFindingStore
	SentinelFixes  *postgres.SentinelFixStore
	Artifacts      *postgres.ArtifactStore
	Identities     *postgres.IdentityStore

	// GitHubPRSessions backs the "open review" action: the forward,
	// non-claiming (repoFullName, prNumber) -> session_id lookup a PR row
	// needs to link to Narvi's own review readout instead of GitHub, when
	// -- and ONLY when -- Narvi has actually been mentioned on that exact
	// PR. Optional (nil-safe, mirroring ReleaseManifestChecks below): a
	// caller that never wires this simply never resolves a session id for
	// any PR row, which degrades the affordance (external GitHub link
	// only) without ever failing the read.
	GitHubPRSessions *postgres.GitHubPRSessionStore
	// ReleaseManifestChecks backs release-cut rows (§15): the ALREADY-computed,
	// ALREADY-persisted manifest check for a PR that internal/domain/
	// intent.DetectRelease identified as a release PR at review-session-
	// creation time (internal/adapters/inbound/github's own
	// triggerReleaseManifestCheckBestEffort) and internal/app/
	// releasereview.Run has since finished computing. A PR with no
	// persisted check -- never mentioned at all, or mentioned but not yet
	// processed by that background worker -- is simply not tagged as a
	// release cut; this package never fabricates one. Optional (nil-safe,
	// mirroring GitHubPRSessions above).
	ReleaseManifestChecks *postgres.ReleaseManifestCheckStore

	SCMCache *SCMCache

	TokenEncryptionKey []byte
	Timeouts           platform.Timeouts

	// ReviewVerdict bundles the (§21.1/§21.2) stores the REAL
	// auto-approval eligibility engine needs -- review_verdicts history
	// (the latest verdict per PR), repo_settings' own auto-approval
	// config, and the contradiction-rate outcome table. Replaces §16's
	// own interim internal/domain/decisioninbox.
	// ComputeAutoApprovalEligible, deleted by this Step -- see
	// buildPROpenItem/revalidateCore below for the two call sites.
	ReviewVerdict appreviewverdict.Deps
}

// Result is Build's own return shape.
type Result struct {
	Items []Item

	// SCMAsOf is when the PR-derived items (ready_to_merge/needs_review)
	// were actually fetched from GitHub -- nil when the actor has no
	// usable linked GitHub credential (no PR items were attempted at
	// all), never a silent "now" standing in for a cache hit's own real,
	// earlier fetch instant (§16.2: "the response carries its as-of
	// timestamp... never presented as live truth").
	SCMAsOf *time.Time

	// SCMFetchFailed is the third state SCMAsOf==nil alone cannot express:
	// true iff the actor's PR-derived rows in
	// this Result are a known-incomplete or degraded picture. ONE channel,
	// fed by several independent producers (deliberately reusing this SAME
	// field rather than adding a second one, so a client only ever has one
	// boolean to check):
	//
	//  1. The live PR fetch failed outright (buildPRItems returned an
	//     error -- a revoked token, a GitHub incident, a timeout). SCMAsOf
	//     stays nil here: no PR items were attempted at all.
	//  2. resolveActorGitHubCredential hit a genuine identity-store DB
	//     error or token-decrypt failure (P2-1) -- distinct from simply
	//     having no linked GitHub identity at all (a legitimate,
	//     NON-degraded empty state, which leaves this field false). SCMAsOf
	//     also stays nil here: no fetch was even attempted.
	//  3. SCMCache.ListOpenPRsForUser's own truncated return (P1-2): one of
	//     GitHub's two discovery queries itself failed while the other
	//     still returned a real, if partial, result. SCMAsOf IS set here
	//     (a genuine, if partial, fetch happened) and Items MAY be
	//     non-empty.
	//  4. buildPRItems' own per-PR §17 SentinelFixStore exclusion check
	//     erroring (P1-3): that ONE row is dropped (fails closed, excluded)
	//     but the overall read is no longer a complete picture either.
	//     SCMAsOf is set here too, same as (3).
	//  5. A per-PR ports.OpenPR.ReviewDecisionDegraded: githubapi.fetchReviewDecision
	//     itself failed for that ONE
	//     PR. Unlike (4), the row is NOT dropped -- buildPROpenItem still
	//     renders it, demoted out of ready_to_merge (that field's own doc
	//     comment) -- but the overall read is, again, no longer a complete
	//     picture. SCMAsOf is set here too, same as (3)/(4).
	//
	// UNLIKE this field's own previous doc comment claimed, SCMAsOf
	// non-nil and SCMFetchFailed true are NOT mutually exclusive as of
	// producers (3)/(4) above -- a partial-but-real fetch can legitimately
	// carry both a real as-of instant and a flag telling the caller not to
	// present the rows present as complete. The contract (dtos.schema.
	// json) previously documented scmAsOf==null as the ONLY signal here --
	// an outage was indistinguishable from "you have no GitHub linked" to
	// a contract-abiding client, which would render the wrong empty state.
	SCMFetchFailed bool

	// DecisionLatencyMedian/DecisionLatencySampleSize/
	// DecisionLatencyComputed mirror §21.1's own "not yet computed
	// sentinel, distinct from a real zero" discipline for every other
	// analytics rollup in this codebase -- DecisionLatencyComputed=false
	// means "no data in the window", never rendered identically to a
	// real, computed zero-second median.
	DecisionLatencyMedian     time.Duration
	DecisionLatencySampleSize int
	DecisionLatencyComputed   bool
}

// Build assembles, ranks, and returns the full decision inbox for
// (actorUserID, actorRole) as of now -- see this file's own top doc
// comment for the full per-kind design.
func Build(ctx context.Context, deps Deps, actorUserID pgtype.UUID, actorRole authz.Role, now time.Time) (Result, error) {
	logger := platform.Logger(ctx)

	var items []Item
	var scmAsOf *time.Time
	var scmFetchFailed bool

	login, token, ok, credDegraded := resolveActorGitHubCredential(ctx, deps, actorUserID)
	switch {
	case ok:
		prItems, asOf, degraded, err := buildPRItems(ctx, deps, login, token, now)
		if err != nil {
			logger.Error("decisioninbox: build pr items failed", "error", err)
			scmFetchFailed = true
		} else {
			items = append(items, prItems...)
			scmAsOf = &asOf
			if degraded {
				// P1-2/P1-3: a truncated (partial) GitHub read, or a
				// per-PR §17 exclusion-check error -- see Result.
				// SCMFetchFailed's own doc comment for the full producer
				// list. The fetch itself still succeeded (asOf above is a
				// real instant), so this is deliberately NOT an `else`
				// branch of the `err != nil` check above.
				scmFetchFailed = true
			}
		}
	case credDegraded:
		// P2-1: a genuine identity-store DB error or token-decrypt
		// failure resolving the actor's OWN GitHub credential -- routed
		// into the SAME degraded signal, never silently rendered
		// identically to "you have no GitHub linked" (ok=false,
		// credDegraded=false, which leaves scmFetchFailed correctly
		// false below).
		scmFetchFailed = true
	}

	planItems, err := buildPlanItems(ctx, deps, actorUserID, actorRole, now)
	if err != nil {
		logger.Error("decisioninbox: build plan items failed", "error", err)
	} else {
		items = append(items, planItems...)
	}

	if actorRole == authz.RoleAdmin {
		items = append(items, buildAttentionItems(ctx, deps, now, logger)...)
	}

	items = rank(items)

	median, sampleSize, computed, err := Metrics(ctx, deps, now)
	if err != nil {
		logger.Error("decisioninbox: compute decision latency failed", "error", err)
	}

	return Result{
		Items:                     items,
		SCMAsOf:                   scmAsOf,
		SCMFetchFailed:            scmFetchFailed,
		DecisionLatencyMedian:     median,
		DecisionLatencySampleSize: sampleSize,
		DecisionLatencyComputed:   computed,
	}, nil
}

// rank sorts items by internal/domain/decisioninbox's own decision-cost-
// then-age ordering (§16.1).
func rank(items []Item) []Item {
	keys := make([]decisioninbox.RankKey, len(items))
	for i, it := range items {
		keys[i] = decisioninbox.RankKey{Kind: it.Kind, EnteredQueueAt: it.EnteredQueueAt}
	}
	order := decisioninbox.SortIndex(keys)
	sorted := make([]Item, len(items))
	for i, idx := range order {
		sorted[i] = items[idx]
	}
	return sorted
}

// resolveActorGitHubCredential fetches actorUserID's own linked GitHub
// identity and decrypts its stored OAuth token -- mirrors httpapi.
// ApplySuggestion's own identical decrypt-and-use pattern
// (reviewfindings.go) exactly, applied here to the CURRENT actor rather
// than an acting maintainer on a specific finding.
//
// ok=false means this actor's PR-derived items are simply skipped --
// never an error that fails the whole Build call, since plan/attention
// items still have plenty to show independent of any GitHub credential.
// degraded distinguishes WHY:
//
//   - ok=false, degraded=false: no linked GitHub identity exists at all
//     (pgx.ErrNoRows from the identity lookup) or the linked identity
//     carries no stored token -- a legitimate, common, NON-degraded empty
//     state. Build renders this identically to "no GitHub linked",
//     exactly as before this fix.
//   - ok=false, degraded=true: the lookup or decrypt ITSELF could not be
//     completed -- a genuine identity-store DB error (anything other than
//     pgx.ErrNoRows) or a token-decrypt failure. Before this fix, this
//     collapsed into the exact same ok=false as "no linked identity",
//     so a client rendered "you have no GitHub linked" for what was
//     actually an outage. Build (below) now routes this into the SAME
//     Result.SCMFetchFailed degraded signal P1-2/P1-3 use (see that
//     field's own doc comment for the full producer list) -- ONE channel,
//     several producers, not a second one.
func resolveActorGitHubCredential(ctx context.Context, deps Deps, actorUserID pgtype.UUID) (externalID, token string, ok bool, degraded bool) {
	identity, err := deps.Identities.GetByUserAndProvider(ctx, actorUserID, sqlcgen.IdentityProviderGithub)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return "", "", false, false
		}
		platform.Logger(ctx).Error("decisioninbox: resolve actor github credential: identity lookup failed", "error", err)
		return "", "", false, true
	}
	if identity.AccessTokenEncrypted == nil {
		return "", "", false, false
	}
	plaintext, err := platform.DecryptToken(deps.TokenEncryptionKey, identity.AccessTokenEncrypted)
	if err != nil {
		platform.Logger(ctx).Error("decisioninbox: resolve actor github credential: decrypt token failed", "error", err)
		return "", "", false, true
	}
	// Never logged, here or anywhere it might propagate to -- mirrors
	// scmcredentials.go/reviewfindings.go's own identical discipline.
	return identity.ExternalID, string(plaintext), true, false
}

// buildPRItems fetches and classifies every open PR the actor is
// currently assigned to -- see this file's own top doc comment for the
// full ready_to_merge/needs_review/handoff decision and the §17
// structural exclusion.
//
// degraded is true iff this
// read is known to be an incomplete/partial picture despite otherwise
// succeeding -- see Result.SCMFetchFailed's own doc comment for the full
// producer list this feeds into; asOf is still a real, honest fetch
// instant even when degraded is true (a partial read is still a REAL
// read, just not a complete one).
func buildPRItems(ctx context.Context, deps Deps, actorGitHubID, token string, now time.Time) (items []Item, asOf time.Time, degraded bool, err error) {
	prs, asOf, truncated, err := deps.SCMCache.ListOpenPRsForUser(ctx, ports.ListOpenPRsForUserSpec{GitHubExternalID: actorGitHubID, Token: token}, now)
	if err != nil {
		return nil, time.Time{}, false, err
	}
	degraded = truncated

	// One fresh budget per Build call -- see codeOwnersBudget's own doc
	// comment for why this must never be shared
	// across actors/requests the way deps.SCMCache itself is.
	budget := newCodeOwnersBudget(maxCodeOwnerResolutionsPerBuild)

	items = make([]Item, 0, len(prs))
	for _, pr := range prs {
		if pr.Draft {
			continue
		}

		repoFullName := pr.Owner + "/" + pr.Repo

		excluded, existsErr := deps.SentinelFixes.ExistsByFixPRNumber(ctx, repoFullName, int32(pr.Number))
		if existsErr != nil {
			// §17's structural exclusion must fail CLOSED: a store error means "cannot prove this is NOT
			// a sentinel-auto-fix follow-up", treated identically to a
			// CONFIRMED one below -- excluded outright, never best-effort
			// passed through as if the check had simply found nothing.
			// isPlatformAuthored (below) already fails closed this same
			// way; this now matches it. This ALSO marks the overall read
			// degraded: a row was
			// just dropped due to an infra failure, not a confirmed
			// exclusion, so the read is no longer a complete picture --
			// see Result.SCMFetchFailed's own doc comment.
			platform.Logger(ctx).Error("decisioninbox: check sentinel-fix exclusion failed -- failing closed (excluding this pr)", "error", existsErr, "repo", repoFullName, "pr_number", pr.Number)
			degraded = true
			continue
		}
		if excluded {
			continue // §17 structural exclusion -- never a row, regardless of any other criterion.
		}

		if pr.ReviewDecisionDegraded {
			// a per-PR degraded review-decision read
			// (githubapi.fetchReviewDecision itself failed for this ONE PR)
			// means HasChangesRequested is not a confirmed fact for this
			// row -- buildPROpenItem (below) already fails this row closed
			// (demoted out of ready_to_merge, see its own doc comment), and
			// the OVERALL read is no longer a complete picture either,
			// mirroring producers (3)/(4) on Result.SCMFetchFailed's own
			// doc comment (a per-row degrade still marks the whole batch).
			degraded = true
		}
		item := buildPROpenItem(ctx, deps, pr, repoFullName, actorGitHubID, token, now, budget)
		items = append(items, item)
	}

	return items, asOf, degraded, nil
}

// openFindingsUnknownFailClosed is the OpenBlockingFindings value
// buildPROpenItem substitutes when countOpenFindings itself errors --
// any positive value fails ComputeAutoApprovalEligible
// closed (its own check is a bare "> 0"), so the exact magnitude carries
// no further meaning beyond that; 1 is chosen purely so a human reading a
// rendered row sees a small, plausible-looking finding count rather than
// an obviously-synthetic sentinel like MaxInt32.
const openFindingsUnknownFailClosed = 1

// buildPROpenItem assembles one Item for an already-filtered, non-draft,
// non-§17-excluded OpenPR.
func buildPROpenItem(ctx context.Context, deps Deps, pr ports.OpenPR, repoFullName, actorGitHubID, token string, now time.Time, budget *codeOwnersBudget) Item {
	provenance := resolvePRProvenance(ctx, deps, pr, repoFullName, actorGitHubID, token, now, budget)

	hasNeedsHuman, riskLabel, isHandoffPR := classifyPRLabels(pr.Labels)

	openFindings, findingsErr := countOpenFindings(ctx, deps, repoFullName, pr.Number)
	findingsUnknown := false
	if findingsErr != nil {
		// Fail CLOSED for the ELIGIBILITY computation below -- see countOpenFindings' own doc comment for why a
		// degraded zero there would be actively dangerous, not merely
		// imprecise. openFindings keeps the synthetic sentinel value for
		// THAT purpose only; findingsUnknown (
		// second round) is the separate signal that stops this same
		// sentinel from also being rendered on the wire as an honest,
		// real findings count -- see Item.FindingsUnknown's own doc
		// comment.
		platform.Logger(ctx).Error("decisioninbox: count open findings failed -- failing closed (treated as a blocking finding present)", "error", findingsErr, "repo", repoFullName, "pr_number", pr.Number)
		openFindings = openFindingsUnknownFailClosed
		findingsUnknown = true
	}
	ciGreen := pr.CIConclusion == ports.CIConclusionSuccess

	// sessionID resolves the review session Narvi has actually run
	// against this EXACT (repoFullName, pr.Number), if any -- see
	// resolveReviewSessionID's own doc comment for why this, and never
	// artifacts.GetPRArtifactByURL's authoring session (isPlatformAuthored
	// below), is the right session id for this purpose.
	sessionID := resolveReviewSessionID(ctx, deps, repoFullName, pr.Number)

	// isReleaseCut/manifestFindingsCount/aggregateReviewTriggered resolve
	// this PR's own release-cut status -- see resolveReleaseCut's own doc
	// comment.
	isReleaseCut, manifestFindingsCount, aggregateReviewTriggered, manifestCoveragePartial := resolveReleaseCut(ctx, deps, repoFullName, pr.Number)

	item := Item{
		RepoFullName:             repoFullName,
		PRNumber:                 pr.Number,
		Title:                    pr.Title,
		HTMLURL:                  pr.HTMLURL,
		HeadSHA:                  pr.HeadSHA,
		Provenance:               &provenance,
		RiskLabel:                riskLabel,
		CIGreen:                  ciGreen,
		Findings:                 openFindings,
		FindingsUnknown:          findingsUnknown,
		IsHandoff:                isHandoffPR,
		HasApprovingReview:       pr.HasApprovingReview,
		HasChangesRequested:      pr.HasChangesRequested,
		EnteredQueueAt:           pr.CreatedAt,
		SessionID:                sessionID,
		IsRelease:                isReleaseCut,
		ManifestFindingsCount:    manifestFindingsCount,
		AggregateReviewTriggered: aggregateReviewTriggered,
		ManifestCoveragePartial:  manifestCoveragePartial,
	}
	item.AgeSeconds = int64(decisioninbox.Age(item.EnteredQueueAt, now).Seconds())
	item.Stale = decisioninbox.IsStale(item.EnteredQueueAt, now, deps.Timeouts.DecisionInboxStaleAfter)

	switch {
	case isHandoffPR:
		item.Kind = decisioninbox.KindAwaitingApproval
	case isReleaseCut:
		// §16.1: "needs_review... includes release cuts with manifest
		// flags (§15)" -- a release cut is ALWAYS a human-judgment row,
		// never auto-merge-eligible, regardless of what the ordinary
		// eligibility computation below would have said. This branch
		// therefore never even runs isPlatformAuthored/
		// computeRealEligibility for a release-cut PR -- a release PR is
		// essentially never platform-authored anyway (it is opened by a
		// human cutting a release, not pushed by a Narvi session), and
		// even if it somehow were, §15's own manifest-check machinery
		// exists precisely because a release cut needs a human looking at
		// the compliance findings, not a fast-path auto-merge.
		item.Kind = decisioninbox.KindNeedsReview
	default:
		platformAuthored := isPlatformAuthored(ctx, deps, pr.HTMLURL)
		eligible := computeRealEligibility(ctx, deps, repoFullName, pr, ciGreen, hasNeedsHuman)
		// HasChangesRequested is a HARD merge blocker at RevalidateForMerge
		// (revalidate.go) but was previously never consulted HERE -- so such a PR sat
		// in the TOP ready_to_merge section with a Merge button that
		// would unconditionally 409 at click time. Demoted to
		// needs_review instead, mirroring RevalidateForMerge's own
		// identical, already-established gate, so the read model and the
		// merge gate never disagree about what "ready to merge" means.
		//
		// openFindings > 0 is ALSO kept as its own, separate AND-condition
		// here, deliberately never folded into computeRealEligibility
		// itself -- §21.2's own criteria list names Shippable/CI/floors/
		// diff-size/sensitive-path/head-SHA-freshness, and nothing about
		// per-finding status; review.Verdict carries no Finding data at
		// all (domain/review's own doc.go design call #4), so an open,
		// unresolved finding is a fact the verdict's own Shippable value
		// could be silently inconsistent with (a model reporting
		// RiskLevel=low while ALSO reporting a real, unresolved finding
		// via the SEPARATE findings array). §16's own interim engine
		// already treated this as a hard exclusion; keeping it here,
		// exactly like HasChangesRequested, preserves that safety
		// property without stretching §21.2's own literal criteria list
		// to cover something it never named.
		// !pr.ReviewDecisionDegraded is its own,
		// separate AND-condition here, mirroring !pr.HasChangesRequested
		// immediately beside it -- a degraded review-decision read is not
		// a confirmed "no changes requested", so it must demote this row
		// out of ready_to_merge exactly like a confirmed changes-request
		// would, never render an unconfirmed read as the all-clear a
		// ready_to_merge row promises. RevalidateForMerge/
		// RevalidateForAutoMerge (revalidate.go) enforce the SAME fact as
		// a hard block at click/auto-merge time regardless of what this
		// read-model row shows.
		if platformAuthored && eligible && !pr.HasChangesRequested && !pr.ReviewDecisionDegraded && openFindings == 0 {
			item.Kind = decisioninbox.KindReadyToMerge
		} else {
			item.Kind = decisioninbox.KindNeedsReview
		}
	}

	return item
}

// computeRealEligibility runs §21.2 stage 1's real auto-approval
// eligibility engine (internal/domain/autoapproval.ComputeEligible) for
// pr -- replacing §16's own interim internal/domain/decisioninbox.
// ComputeAutoApprovalEligible (deleted by this Step). Fails CLOSED
// (returns false) on every degraded path: no verdict ever posted for
// this PR (reviewverdict.GetLatest's own ok=false), or a genuine store
// error reading either the verdict or this repo's own eligibility
// config -- an auto-approval decision this codebase cannot fully
// evaluate must never default to "eligible" (this Step's own "fail
// direction matters here" requirement, mirroring isPlatformAuthored/
// countOpenFindings' own identical fail-closed precedent in this same
// file).
//
// ALSO records the §21.2 stage 2 "overridden" contradiction-rate signal
// (best-effort, never blocking this read) -- see reviewverdict.
// RecordOverridden's own doc comment: this fires when the verdict would
// have been eligible on every OTHER criterion but a human already
// disagreed (HasChangesRequested, or a needs-human label), computed here
// because this call site already has every fact needed at zero extra
// cost.
func computeRealEligibility(ctx context.Context, deps Deps, repoFullName string, pr ports.OpenPR, ciGreen, hasNeedsHuman bool) bool {
	record, hasVerdict, err := appreviewverdict.GetLatest(ctx, deps.ReviewVerdict, repoFullName, int32(pr.Number))
	if err != nil {
		platform.Logger(ctx).Error("decisioninbox: get latest review verdict failed -- failing closed (not eligible)", "error", err, "repo", repoFullName, "pr_number", pr.Number)
		return false
	}
	if !hasVerdict {
		return false
	}

	// a genuine repo_settings read error means this
	// repo's own configured policy cannot be established -- FAIL CLOSED
	// (not eligible), mirroring this function's own existing
	// GetLatest-error handling immediately above (a degraded READ-MODEL
	// row, never a hard failure of the whole Build call: this is a
	// best-effort aggregation, unlike revalidateCore's own action-endpoint
	// propagation for the identical error).
	cfg, cfgErr := appreviewverdict.LoadEligibilityConfig(ctx, deps.ReviewVerdict, repoFullName)
	if cfgErr != nil {
		platform.Logger(ctx).Error("decisioninbox: load eligibility config failed -- failing closed (not eligible)", "error", cfgErr, "repo", repoFullName, "pr_number", pr.Number)
		return false
	}

	// ChangedFileCount/TouchedBlastRadius are BOTH
	// derived here from pr -- pr is this call's own already-fetched,
	// server-side ports.OpenPR (buildPRItems' own live SCMCache.
	// ListOpenPRsForUser read), never the posted verdict's own
	// self-reported FilesChanged/BlastRadius. No new I/O.
	//
	// Phase 5 audit findings 1+2 (both fixed): changedFileCount is
	// pr.ChangedFilesCount, GitHub's own authoritative scalar -- never
	// len(pr.ChangedFiles), which githubapi caps at one page and which
	// used to also silently read as 0 whenever the underlying GitHub
	// fetch failed outright. touchedBlastRadiusKnown mirrors
	// revalidateCore's own identical wiring (revalidate.go) -- see
	// ports.OpenPR.ChangedFilesListDegraded's own doc comment for the
	// two independent ways it can go true (a failed fetch, or a
	// genuinely large diff whose listing was truncated at GitHub's own
	// one-page cap).
	changedFileCount := pr.ChangedFilesCount
	touchedBlastRadius := autoapproval.ClassifyChangedPaths(pr.ChangedFiles)
	touchedBlastRadiusKnown := !pr.ChangedFilesListDegraded

	// A genuine correctness bug: computed ONCE, ignoring BOTH human-disagreement signals --
	// HasNeedsHumanLabel here, and pr.HasChangesRequested, which is not
	// even a ComputeEligible INPUT at all (it is enforced entirely
	// OUTSIDE this engine: this file's own Kind-classification
	// AND-condition above, and revalidateCore's own hard block,
	// revalidate.go). This answers "would the engine's own real criteria
	// have approved this PR at all, on its own facts". eligible (this
	// function's own return value) is then ALGEBRAICALLY exactly this
	// same result, additionally gated on hasNeedsHuman --
	// ComputeEligible's own HasNeedsHumanLabel check (eligibility.go) is
	// unconditional and evaluated FIRST, independent of every other
	// criterion, so `eligible == eligibleIgnoringHumanSignals &&
	// !hasNeedsHuman` holds in every case -- deriving it this way calls
	// the engine exactly ONCE per PR instead of the previous, always-TWO-call
	// version, and closes the bug below at the same time.
	//
	// THE BUG THIS FIXES: the PREVIOUS version computed `eligible` FIRST
	// (with HasNeedsHumanLabel: hasNeedsHuman as a real input), then only
	// entered the RecordOverridden check when `!eligible`. But since
	// pr.HasChangesRequested is not a ComputeEligible input at all, a PR
	// with hasNeedsHuman == false, HasChangesRequested == true, and every
	// OTHER real criterion satisfied produced `eligible == true` from
	// that first call (nothing inside ComputeEligible could see
	// HasChangesRequested to disagree) -- so `!eligible` was FALSE and
	// the whole RecordOverridden block was skipped, unconditionally, for
	// every PR in exactly the population §21.2's own "contested" metric
	// most needs to see: the engine said yes, a human requesting changes
	// said no. RecordOverridden could only ever fire for the
	// HasNeedsHumanLabel half of "contested", never the
	// HasChangesRequested half -- the contradiction-rate read model's own
	// "overridden" count silently under-counted from day one, no matter
	// how many PRs a human overrode via changes-requested specifically.
	// The fix below no longer gates entry to the RecordOverridden check
	// on `eligible`/`!eligible` at all -- it gates directly on
	// eligibleIgnoringHumanSignals, which is exactly "would the engine
	// have approved this on its own criteria", independent of which
	// human-disagreement signal (if any) is ALSO present.
	eligibleIgnoringHumanSignals, _ := autoapproval.ComputeEligible(autoapproval.EligibilityInput{
		Verdict:                 record.Verdict,
		VerdictHeadSHA:          record.HeadSHA,
		CurrentHeadSHA:          pr.HeadSHA,
		CIGreen:                 ciGreen,
		HasNeedsHumanLabel:      false,
		ChangedFileCount:        changedFileCount,
		TouchedBlastRadius:      touchedBlastRadius,
		TouchedBlastRadiusKnown: touchedBlastRadiusKnown,
	}, cfg)
	eligible := eligibleIgnoringHumanSignals && !hasNeedsHuman

	// "Contested": the engine would have approved this PR on every
	// criterion it actually checks, but a human signal -- a needs-human
	// label, OR a reviewer requesting changes -- means it was NOT
	// actually auto-approved. reviewverdict.RecordOverridden's own doc
	// comment: recorded the first time this is observed for this (repo,
	// PR, head_sha), never re-recorded on every subsequent read (its own
	// idempotent ON CONFLICT DO NOTHING write).
	if eligibleIgnoringHumanSignals && (hasNeedsHuman || pr.HasChangesRequested) {
		appreviewverdict.RecordOverridden(ctx, deps.ReviewVerdict, repoFullName, int32(pr.Number), record.HeadSHA)
	}

	return eligible
}

// resolvePRProvenance determines WHY pr is assigned to actorGitHubID --
// §16.1's own "a first-class field" assignment provenance.
func resolvePRProvenance(ctx context.Context, deps Deps, pr ports.OpenPR, repoFullName, actorGitHubID, token string, now time.Time, budget *codeOwnersBudget) decisioninbox.Provenance {
	in := decisioninbox.ProvenanceInput{RepoFullName: repoFullName}

	for _, a := range pr.Assignees {
		if a.ExternalID == actorGitHubID {
			in.DirectlyAssigned = true
			break
		}
	}
	for _, r := range pr.RequestedReviewers {
		if r.ExternalID == actorGitHubID {
			in.RequestedReviewer = true
			break
		}
	}

	// budget.take gates this call at zero I/O once the per-build
	// CODEOWNERS-resolution cap is exhausted --
	// see codeOwnersBudget's own doc comment. Skipping it here only ever
	// degrades a display nicety (this ONE PR's provenance falls back to
	// the "un-pinned requested reviewer" default below, or plain
	// ProvenanceRequestedReviewer/ProvenanceDirect if either of those
	// already matched) -- CODEOWNERS resolution is never how a PR is
	// DISCOVERED (searchOpenPRs' own doc comment), so skipping it can
	// never hide a row or grant unintended access.
	//
	// Ref is pr.BaseRef, deliberately never pr.HeadSHA (related hardening): the PR's HEAD is attacker-
	// chosen (whoever opened/pushed the PR controls it), so resolving
	// CODEOWNERS there would let a PR's own author dictate which
	// CODEOWNERS file this call reads for classifying THEIR OWN PR --
	// GitHub's own real CODEOWNERS enforcement is evaluated against the
	// repo's base branch, never a PR's head, and this now matches that.
	if budget.take(ctx) {
		if owners, _, err := deps.SCMCache.ResolveCodeOwners(ctx, ports.ResolveCodeOwnersSpec{
			Owner: pr.Owner, Repo: pr.Repo, Ref: pr.BaseRef, Paths: pr.ChangedFiles, Token: token,
		}, now); err == nil {
			for _, o := range owners {
				if o.ExternalID == actorGitHubID {
					in.CodeOwnerMatch = true
					in.CodeOwnerPattern = o.Pattern
					break
				}
			}
		}
	}

	if !in.DirectlyAssigned && !in.RequestedReviewer && !in.CodeOwnerMatch {
		// This PR was only discoverable at all via the review-requested:
		// search qualifier matching through TEAM membership GitHub itself
		// resolved server-side (listopenprs.go's own top doc comment: "If
		// the requested person is on a team that is requested for review,
		// then review requests for that team will also appear in the
		// search results") -- this codebase has no further API surface
		// wired to re-derive WHICH team without a second, separate
		// team-membership listing call this Step's own scope does not add.
		// Reported as an un-pinned "requested reviewer" rather than
		// silently falling through to ResolveProvenance's own not-ok
		// zero value, which would incorrectly suggest this row should
		// never have appeared in the inbox at all.
		in.RequestedReviewer = true
	}

	provenance, _ := decisioninbox.ResolveProvenance(in)
	return provenance
}

// classifyPRLabels scans pr's own current labels for the three signals
// this Step's own interim eligibility/handoff classification needs.
//
// riskLabel picks the MOST RESTRICTIVE of the review:*-risk labels
// present -- any high-risk label wins over medium, which wins over low.
// GitHub's own labels array carries NO ordering
// guarantee a caller may rely on (verified directly: this codebase's own
// verdictnotifier.go issues AddLabels then a SEPARATE per-label
// RemoveLabel call, two independent GitHub calls -- a failed Remove
// durably leaves BOTH an old and a new risk label on the same PR at
// once, a genuinely reachable state, not a hypothetical), so picking
// "whichever label happens to appear last in the slice" would authorize
// a merge decision on an unspecified, externally-controlled ordering.
// This mirrors reviewpost.RiskLabel/review.baselineFromRisk's own
// already-established "unrecognized/ambiguous fails conservative toward
// the more alarming tier" convention, applied here to "more than one
// tier present at once" instead of "no tier recognized at all".
func classifyPRLabels(labels []string) (hasNeedsHuman bool, riskLabel string, isHandoff bool) {
	for _, l := range labels {
		switch l {
		case reviewpost.LabelNeedsHuman:
			hasNeedsHuman = true
		case reviewpost.LabelHighRisk:
			riskLabel = reviewpost.LabelHighRisk
		case reviewpost.LabelMediumRisk:
			if riskLabel != reviewpost.LabelHighRisk {
				riskLabel = reviewpost.LabelMediumRisk
			}
		case reviewpost.LabelLowRisk:
			if riskLabel == "" {
				riskLabel = reviewpost.LabelLowRisk
			}
		case handoff.Label:
			isHandoff = true
		}
	}
	return hasNeedsHuman, riskLabel, isHandoff
}

// maxCodeOwnerResolutionsPerBuild bounds the TOTAL number of
// ResolveCodeOwners calls ONE Build invocation will make across every
// candidate PR -- the per-inbox-build half of B3's own two-layer cap
// (the adapter's own maxCodeOwnerRefsPerCall, githubapi/
// resolvecodeowners.go, bounds a SINGLE PR's own CODEOWNERS fan-out;
// this bounds the SUM across up to maxOpenPRsForUser PRs in one page
// load) -- a victim whose review is requested on
// many PRs, each carrying a moderately large CODEOWNERS file, still
// cannot drive an unbounded number of outbound calls on the victim's own
// token in one inbox load.
const maxCodeOwnerResolutionsPerBuild = 200

// codeOwnersBudget bounds how many MORE ResolveCodeOwners calls the
// CURRENT Build invocation is still willing to make. A fresh budget is
// created once per Build call (buildPRItems) and threaded down through
// buildPROpenItem/resolvePRProvenance -- it must NEVER be shared across
// actors/requests the way deps.SCMCache itself is (SCMCache is
// constructed once, process-wide, at wiring time): a budget living
// there would leak across unrelated users' own inbox loads instead of
// bounding each one independently, exactly the per-victim isolation
// this cap exists to provide.
type codeOwnersBudget struct {
	remaining       int
	truncatedLogged bool
}

// newCodeOwnersBudget builds a fresh, per-Build-call budget.
func newCodeOwnersBudget(limit int) *codeOwnersBudget {
	return &codeOwnersBudget{remaining: limit}
}

// take reports whether the caller may still make one more
// ResolveCodeOwners call this Build invocation -- false once the
// per-build cap is exhausted, in which case the FIRST such call logs a
// warning (never silently); every later call this same Build invocation
// makes after that stays silent, so one truncated inbox load produces
// exactly one log line, not one per remaining PR.
func (b *codeOwnersBudget) take(ctx context.Context) bool {
	if b.remaining <= 0 {
		if !b.truncatedLogged {
			platform.Logger(ctx).Warn("decisioninbox: codeowners resolution budget exhausted for this inbox build -- remaining prs' codeowners provenance will be skipped", "max_per_build", maxCodeOwnerResolutionsPerBuild)
			b.truncatedLogged = true
		}
		return false
	}
	b.remaining--
	return true
}

// countOpenFindings counts repoFullName/prNumber's own review_findings
// rows that still represent an unresolved defect on the real head --
// reviewpost.FindingStatus.BlocksMerge decides, never an equality check
// here. A rebutted or fix-pending/open/merged/applied finding does not
// count (each has an explicit resolution); a fix_recorded one DOES,
// because recording a fix in shadow changed nothing on the head.
//
// A fetch failure is propagated to the caller --
// it must NEVER degrade to zero. This function's own doc comment used to
// justify a degraded zero here by claiming "this Step's own eligibility
// check already requires the risk label to be exactly LabelLowRisk
// before this count matters at all" -- that reasoning was backwards:
// ComputeAutoApprovalEligible's OpenBlockingFindings > 0 check and its
// RiskLabel == LabelLowRisk check are two INDEPENDENT AND-conditions,
// not one gated behind the other -- a low-risk PR WITH a genuinely open,
// unresolved finding is EXACTLY the population this count exists to
// keep out of ready_to_merge, so a degraded zero on a store error would
// silently flip that PR from ineligible to eligible, the opposite of
// this codebase's own fail-conservative discipline (isPlatformAuthored,
// this same file, already fails closed on ITS OWN store error -- this
// now matches it). Each of this function's own two callers fails closed
// in the way appropriate to its own context: aggregate.go's
// buildPROpenItem degrades the affected row to non-ready_to_merge rather
// than failing the whole read model; revalidate.go's RevalidateForMerge
// propagates the error outright, refusing the merge.
func countOpenFindings(ctx context.Context, deps Deps, repoFullName string, prNumber int) (int, error) {
	findings, err := deps.ReviewFindings.ListOpenAndRebutted(ctx, repoFullName, int32(prNumber))
	if err != nil {
		return 0, err
	}
	count := 0
	for _, f := range findings {
		if reviewpost.FindingStatus(f.Status).BlocksMerge() {
			count++
		}
	}
	return count, nil
}

// isPlatformAuthored reports whether SOME Narvi session pushed and opened
// the PR at htmlURL (§16.1's own "authored by a platform session"
// ready_to_merge criterion) -- see ArtifactStore.GetPRArtifactByURL's own
// doc comment.
func isPlatformAuthored(ctx context.Context, deps Deps, htmlURL string) bool {
	_, err := deps.Artifacts.GetPRArtifactByURL(ctx, htmlURL)
	return err == nil
}

// resolveReviewSessionID answers "a PR-shaped row carries no session id":
// given repoFullName/prNumber, does a Narvi review session already exist
// for THIS EXACT PR, and if so what is its id?
//
// Deliberately github_pr_sessions (deps.GitHubPRSessions), NEVER
// artifacts.GetPRArtifactByURL's own authoring session (isPlatformAuthored
// above): those answer two DIFFERENT questions that happen to often be
// about the same PR. isPlatformAuthored's own artifact row records which
// session PUSHED and OPENED a PR (recordPRArtifact, pushpr.go) -- that
// session was never itself created via a GitHub @mention (it is the
// ordinary create-session flow), so it carries no github_pr_sessions row
// and GetReviewReadout/GetReleaseManifestReadout (both keyed on exactly
// that row existing, "400 if it exists but was never created via a
// GitHub PR mention") would 400 on it. github_pr_sessions' own forward
// (repoFullName, prNumber) claim, by contrast, is EXACTLY the session
// those two read endpoints require -- the one a human's own "@narvi
// review" mention (or the release-manifest trigger, which fires on that
// SAME winning mention, releasemanifest.go) created for this PR. Using
// the wrong one here would silently offer an "Open review" action that
// 400s at click time -- worse than the external-GitHub-link gap this
// resolution replaces (this Step's own "an action offered must be one
// the backend can actually perform" rule).
//
// repoFullName/prNumber both come from the SAME already-fetched OpenPR
// this function's one caller (buildPROpenItem) is currently classifying
// -- this can never return a session id belonging to a DIFFERENT PR,
// since github_pr_sessions' own primary key is exactly (repo_full_name,
// pr_number).
//
// deps.GitHubPRSessions == nil (a caller that never wires this optional
// dependency) and pgx.ErrNoRows (Narvi has never been mentioned on this
// PR) both resolve to "" identically -- neither is an error, and a
// caller degrades gracefully to no "Open review" affordance either way.
// Any OTHER store error is logged and also resolves to "" -- best-effort,
// mirroring isPlatformAuthored's own identical "a lookup failure simply
// means the enhanced affordance is unavailable" posture; this is a
// display nicety, never something worth failing the whole inbox read
// over.
func resolveReviewSessionID(ctx context.Context, deps Deps, repoFullName string, prNumber int) string {
	if deps.GitHubPRSessions == nil {
		return ""
	}
	row, err := deps.GitHubPRSessions.GetByRepoAndPRNumber(ctx, repoFullName, int32(prNumber))
	if err != nil {
		if !errors.Is(err, pgx.ErrNoRows) {
			platform.Logger(ctx).Error("decisioninbox: resolve review session id failed", "error", err, "repo", repoFullName, "pr_number", prNumber)
		}
		return ""
	}
	if !row.SessionID.Valid {
		// Cannot actually happen against a real Postgres instance --
		// RepoKnownToDeployment's own generated doc comment: "a row only
		// ever COMMITS with a non-NULL session_id" (coalesce.go's single-
		// transaction EnsureRow+LockForUpdate+SetSessionID sequencing
		// rolls back a denied/failed claim wholesale). Handled anyway,
		// never a raw pgtype.UUID{}.String() call on an invalid value.
		return ""
	}
	return row.SessionID.String()
}

// resolveReleaseCut answers "release-cut rows never appear": does
// repoFullName/prNumber already have a persisted §15.2
// manifest check (internal/app/releasereview.Run, written by
// persistReleaseManifestCheck)? If so, this PR is a release cut --
// §16.1: "needs_review... includes release cuts with manifest flags
// (§15)" -- and manifestFindingsCount/aggregateReviewTriggered are its
// own already-computed, honestly-scoped facts.
//
// A PR intent.DetectRelease would classify as a release PR but that has
// never been mentioned (or was mentioned but whose background worker
// hasn't finished yet, releasereview.Worker) has no persisted row and is
// deliberately NOT tagged here -- this function never re-runs detection
// itself (it has neither the fresh head/base branch fetch nor the
// deployment's own configured branch-pattern/label that detection needs,
// and re-deriving them here would be a second, drifting copy of logic
// that already runs exactly once, at review-session-creation time). A
// release cut that has not yet been discovered/checked simply renders as
// an ordinary needs_review PR row for the short window until the
// background worker catches up -- an honest "not yet available" gap,
// never a fabricated one, matching GetReleaseManifestReadout's own
// identical "computed=false... never a 404" posture for the SAME
// underlying data.
//
// manifestFindingsCount is §15.2's own MECHANICAL findings only (admin
// overrides, red-at-merge, unreviewed reverts) -- NEVER the aggregate
// diff review's own composition findings (§15.3/§15.4), which nothing in
// this codebase computes yet (GetReleaseManifestReadout's own top doc
// comment: "this handler therefore never fabricates 'composition
// findings'"). aggregateReviewTriggered is §15.3's own real, already-
// computed TRIGGER decision -- distinct from, and never a stand-in for,
// a composition finding count.
//
// manifestCoveragePartial carries the persisted coverage_partial flag
// through unchanged. §15.2's finding computation runs over whatever
// ListMergedBetween returned, and that port call is allowed to truncate
// (its own second return value); when it did, len(findings) is a lower
// bound over an incomplete set. Both other consumers of this same row
// already refuse to hide that -- reviewpost.RenderManifestComment says so
// in the posted comment, GetReleaseManifestReadout exposes it as
// `coveragePartial` -- so the inbox row carries it too rather than being
// the one place a truncated scan reads as a clean audit.
func resolveReleaseCut(ctx context.Context, deps Deps, repoFullName string, prNumber int) (isReleaseCut bool, manifestFindingsCount int, aggregateReviewTriggered bool, manifestCoveragePartial bool) {
	if deps.ReleaseManifestChecks == nil {
		return false, 0, false, false
	}
	check, err := deps.ReleaseManifestChecks.GetLatest(ctx, repoFullName, int32(prNumber))
	if err != nil {
		if !errors.Is(err, pgx.ErrNoRows) {
			platform.Logger(ctx).Error("decisioninbox: resolve release cut failed", "error", err, "repo", repoFullName, "pr_number", prNumber)
		}
		return false, 0, false, false
	}

	var findings []json.RawMessage
	if err := json.Unmarshal(check.Findings, &findings); err != nil {
		// findings is NOT NULL DEFAULT '[]'::jsonb (migrations/
		// 000097_release_manifest_checks.up.sql) -- a genuinely malformed
		// value here means the persisted row itself is corrupt, not that
		// this PR isn't a release cut. Render the row anyway (this PR
		// unambiguously IS a release cut -- the check row exists), just
		// with an honest zero findings count rather than propagating a
		// decode error into the whole inbox read.
		platform.Logger(ctx).Error("decisioninbox: unmarshal release manifest findings failed, rendering as zero", "error", err, "repo", repoFullName, "pr_number", prNumber)
		// ...and partial, unconditionally: a count that could not be
		// decoded is not a count of zero. Reusing coverage_partial's own
		// field for this says exactly the one thing both cases have in
		// common and the one thing the reader needs -- "the number beside
		// this row is not a complete audit" -- rather than inventing a
		// second sentinel for a corrupt row nobody can act on
		// differently.
		return true, 0, check.AggregateReviewTriggered, true
	}
	return true, len(findings), check.AggregateReviewTriggered, check.CoveragePartial
}

// buildPlanItems returns every plan-mode plan actorUserID/actorRole is
// entitled to approve (authz.ActionApprovePlan, the SAME verdict
// httpapi.canActOnPlan already renders for the real approve/reject
// endpoints).
func buildPlanItems(ctx context.Context, deps Deps, actorUserID pgtype.UUID, actorRole authz.Role, now time.Time) ([]Item, error) {
	rows, err := deps.Plans.ListAwaitingApproval(ctx)
	if err != nil {
		return nil, err
	}

	items := make([]Item, 0, len(rows))
	for _, row := range rows {
		ownedOrJoined := row.SessionCreatedBy.Valid && row.SessionCreatedBy == actorUserID
		if !ownedOrJoined {
			if exists, err := deps.Participants.Exists(ctx, row.SessionID, actorUserID); err == nil && exists {
				ownedOrJoined = true
			}
		}

		actor := authz.Actor{UserID: actorUserID.String(), Role: actorRole}
		if err := authz.Authorize(actor, authz.ActionApprovePlan, authz.Resource{OwnedOrJoined: ownedOrJoined}); err != nil {
			continue
		}

		title := "plan-mode plan"
		if row.SessionTitle != nil && *row.SessionTitle != "" {
			title = *row.SessionTitle
		}

		item := Item{
			Kind:             decisioninbox.KindAwaitingApproval,
			Title:            title,
			PlanID:           row.ID.String(),
			SessionID:        row.SessionID.String(),
			PlanSessionTitle: title,
		}
		if row.CreatedAt.Valid {
			item.EnteredQueueAt = row.CreatedAt.Time
		}
		item.AgeSeconds = int64(decisioninbox.Age(item.EnteredQueueAt, now).Seconds())
		item.Stale = decisioninbox.IsStale(item.EnteredQueueAt, now, deps.Timeouts.DecisionInboxStaleAfter)

		items = append(items, item)
	}

	return items, nil
}

// buildAttentionItems returns every needs_attention row -- ADMIN ONLY;
// Build's own caller never invokes this for a non-admin actor. Each of
// the three sub-scans is independently best-effort: one failing is logged
// and simply contributes no rows, never failing the other two.
func buildAttentionItems(ctx context.Context, deps Deps, now time.Time, logger *slog.Logger) []Item {
	var items []Item

	sessions, err := deps.Sessions.ListFailed(ctx, maxAttentionRowsPerSource)
	if err != nil {
		logger.Error("decisioninbox: list failed sessions failed", "error", err)
	}
	for _, s := range sessions {
		title := "session"
		if s.Title != nil && *s.Title != "" {
			title = *s.Title
		}
		failureReason := ""
		if s.FailureReason != nil {
			failureReason = string(*s.FailureReason)
		}
		item := Item{
			Kind:          decisioninbox.KindNeedsAttention,
			Title:         title,
			SessionID:     s.ID.String(),
			FailureReason: failureReason,
		}
		if s.UpdatedAt.Valid {
			item.EnteredQueueAt = s.UpdatedAt.Time
		}
		item.AgeSeconds = int64(decisioninbox.Age(item.EnteredQueueAt, now).Seconds())
		item.Stale = decisioninbox.IsStale(item.EnteredQueueAt, now, deps.Timeouts.DecisionInboxStaleAfter)
		items = append(items, item)
	}

	pausedStatus := sqlcgen.AutomationStatusPaused
	automations, err := deps.Automations.List(ctx, pgtype.UUID{}, &pausedStatus)
	if err != nil {
		logger.Error("decisioninbox: list paused automations failed", "error", err)
	}
	for _, a := range automations {
		// A manually-paused automation (httpapi.PauseAutomation) writes
		// the IDENTICAL status='paused' transition an auto-pause does --
		// this table carries no separate pause-REASON column (migrations/
		// 000051_automations.up.sql). ConsecutiveFailures reaching
		// automation.AutoPauseThreshold is the one durable, positive
		// signal that distinguishes "the strike mechanism paused this"
		// from "an admin chose to pause it" -- a manual pause well before
		// any strikes accumulated leaves ConsecutiveFailures below the
		// threshold, correctly excluded here.
		if a.ConsecutiveFailures < int32(automation.AutoPauseThreshold) {
			continue
		}
		summary := ""
		if a.ArtifactSummary != nil {
			summary = *a.ArtifactSummary
		}
		item := Item{
			Kind:            decisioninbox.KindNeedsAttention,
			Title:           a.Name,
			AutomationID:    a.ID.String(),
			ArtifactSummary: summary,
		}
		if a.UpdatedAt.Valid {
			item.EnteredQueueAt = a.UpdatedAt.Time
		}
		item.AgeSeconds = int64(decisioninbox.Age(item.EnteredQueueAt, now).Seconds())
		item.Stale = decisioninbox.IsStale(item.EnteredQueueAt, now, deps.Timeouts.DecisionInboxStaleAfter)
		items = append(items, item)
	}

	entries, err := deps.Outbox.ListDeadLetter(ctx, maxAttentionRowsPerSource)
	if err != nil {
		logger.Error("decisioninbox: list dead-letter outbox entries failed", "error", err)
	}
	for _, e := range entries {
		lastErr := ""
		if e.LastError != nil {
			lastErr = *e.LastError
		}
		item := Item{
			Kind:       decisioninbox.KindNeedsAttention,
			Title:      fmt.Sprintf("outbox delivery: %s", e.Kind),
			OutboxID:   e.ID.String(),
			OutboxKind: e.Kind,
			LastError:  lastErr,
		}
		if e.CreatedAt.Valid {
			item.EnteredQueueAt = e.CreatedAt.Time
		}
		item.AgeSeconds = int64(decisioninbox.Age(item.EnteredQueueAt, now).Seconds())
		item.Stale = decisioninbox.IsStale(item.EnteredQueueAt, now, deps.Timeouts.DecisionInboxStaleAfter)
		items = append(items, item)
	}

	return items
}
