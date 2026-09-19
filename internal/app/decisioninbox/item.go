package decisioninbox

import (
	"time"

	"github.com/narvidev/narvi/internal/domain/decisioninbox"
)

// Item is one decision-inbox row, ready to convert 1:1 into the REST
// response DTO (contracts/rest/v1/dtos.schema.json's own DecisionInboxItem).
// Only the fields relevant to Kind are populated -- Go has no tagged-union
// type, and a flat struct with kind-scoped fields (documented per field
// below) matches this codebase's own established precedent for a
// similarly kind-varying shape (e.g. restdtos' own per-artifact-type
// looseness).
type Item struct {
	Kind decisioninbox.Kind

	// Title is a short, human-readable summary of the row -- a PR's own
	// title, a plan's session title, a session's own title, an
	// automation's own name, or an outbox row's own kind.
	Title string

	// EnteredQueueAt is when this row first became a pending decision --
	// SortIndex's own ranking input, and Age/IsStale's own reference
	// point. For a PR row specifically (KindReadyToMerge/KindNeedsReview,
	// and the handoff sub-case of KindAwaitingApproval) this is an
	// APPROXIMATION -- the PR's own GitHub creation time (pr.CreatedAt),
	// not the instant it became assigned/eligible for THIS actor
	// specifically -- see aggregate.go's buildPROpenItem for the full
	// "why", including why this errs in the OPPOSITE direction from the
	// outbox's own similarly-approximated timestamps (this one can only
	// ever UNDER-state how recently a PR became a
	// decision, which means the stale flag below can OVER-fire on an
	// old-but-recently-assigned PR, not fail safely quiet the way the
	// outbox's own approximation does).
	EnteredQueueAt time.Time
	AgeSeconds     int64
	Stale          bool

	// PR fields (KindReadyToMerge / KindNeedsReview -- including the
	// handoff sub-case, which rides KindAwaitingApproval instead, below).
	RepoFullName string
	PRNumber     int
	HTMLURL      string
	HeadSHA      string
	Provenance   *decisioninbox.Provenance
	RiskLabel    string
	CIGreen      bool
	// Findings is the PR's own still-open review-findings count -- see
	// FindingsUnknown's own doc comment immediately below for when this
	// value must NOT be trusted/rendered as a real count.
	Findings int
	// FindingsUnknown is true iff Findings above could not actually be
	// determined (countOpenFindings itself errored) -- buildPROpenItem
	// still fails the ELIGIBILITY
	// computation closed in this case (substituting openFindingsUnknown
	// FailClosed so a degraded read can never silently flip a PR eligible
	// that a real, positive open-finding count would have blocked), but
	// that internal fail-closed sentinel must never be presented on the
	// wire as an honest, real findings count -- decisionInboxItemToDTO
	// (httpapi) renders `findings: null` whenever this is true, mirroring
	// this same package's own established "never present a degraded value
	// as real" discipline (SCMFetchFailed, CIConclusionUnknown).
	FindingsUnknown bool
	IsHandoff       bool
	// HasApprovingReview is the PR's own current review-decision fact
	// (ports.OpenPR.HasApprovingReview) -- display only: §16.1 defines
	// ready_to_merge's own "approval" as the deterministic eligibility
	// engine's auto-approval, never a human GitHub review, so this field
	// feeds NO eligibility computation anywhere in this package (see
	// HasChangesRequested's own doc comment immediately below for the one
	// review-decision fact that DOES gate a merge). Populated
	// unconditionally by buildPROpenItem, mirroring CIGreen/Findings/
	// IsHandoff immediately above (this field used
	// to be fetched from GitHub and then read by nothing at all).
	HasApprovingReview bool
	// HasChangesRequested is the PR's own current review-decision fact
	// (ports.OpenPR.HasChangesRequested), reduced to each reviewer's
	// LATEST review. UNLIKE HasApprovingReview
	// above, this DOES gate an action -- RevalidateForMerge treats a true
	// value as a hard merge block -- so
	// buildPROpenItem also consults it when classifying Kind (a PR with
	// changes requested never classifies ready_to_merge: before this fix such a PR sat in the
	// TOP ready_to_merge section with a Merge button that would
	// unconditionally 409). Populated unconditionally, mirroring
	// HasApprovingReview.
	HasChangesRequested bool

	// VerdictID is review_verdicts.id for this PR's own CURRENT latest
	// verdict, whenever one has been posted (empty otherwise) -- set
	// unconditionally alongside the acceptance-display fields below (the
	// SAME GetLatest call already resolves it), so a client can name this
	// exact verdict back on AcceptReviewVerdictRequest.VerdictId (finding
	// F3, adversarial review: accept-verdict binds to the ONE verdict a
	// caller actually read, never to whichever verdict happens to be
	// latest when the accept request arrives).
	VerdictID string

	// AcceptanceID/AcceptanceJustification/AcceptedAt/AcceptedByUserID
	// (§21.1b: "human acceptance of a verdict the engine refuses") are set
	// iff an ACTIVE, APPLICABLE acceptance exists for this PR's own
	// current verdict AND current attempt (internal/domain/reviewverdict.
	// Acceptance.Applicable, given internal/app/reviewverdict.
	// HasNewerReviewAttempt's own resolved fact -- finding F1, adversarial
	// review: a not_assessed attempt posts no verdict, so verdict-id
	// equality alone cannot see it) AND its own recorded base ref/ancestor
	// chain still match this PR's own current, already-fetched base ref/
	// ancestor chain (finding F6 of an earlier round, adversarial review:
	// Applicable alone cannot see a moved base or a changed ancestor chain
	// -- see acceptanceContextStillFresh's own doc comment, aggregate.go).
	// A revoked acceptance, one that no longer binds to the current
	// verdict/attempt, or one a moved base/changed ancestor chain has
	// invalidated, renders identically to no acceptance at all: all four
	// fields are the empty string / zero time, mirroring this package's
	// own "absent fact -> zero value" convention elsewhere on this same
	// struct (e.g. Findings/FindingsUnknown). Display only -- none of the
	// four ever gates Kind (buildPROpenItem's own classification below is
	// unaffected: an accepted PR still classifies needs_review, never
	// ready_to_merge, because §21.1b's own acceptance is an authorisation
	// for a human's OWN Merge click, never a reclassification of the
	// engine's judgment) -- they exist so a maintainer scanning
	// needs_review can SEE that this row's own eligibility refusal has
	// already been authorised, by whom (AcceptedByUserID -- finding F6,
	// adversarial review: this field did not exist before this fix, even
	// though this doc comment already claimed a maintainer could see "by
	// whom" it was authorised), and can name AcceptanceID back on
	// RevokeReviewVerdictAcceptanceRequest.Id (finding F11 of an earlier
	// round, adversarial review: before that field existed, no read
	// surface ever returned an acceptance's own id, so revocation was
	// reachable only by a client that had kept the original 201 response
	// body).
	AcceptanceID            string
	AcceptanceJustification string
	AcceptedAt              time.Time
	AcceptedByUserID        string

	// AcceptanceMergeable/AcceptanceMergeBlockedReason (round 3, finding
	// R1, adversarial review) answer the question a maintainer+ actually
	// needs once they can SEE the acceptance above: does it unblock a
	// Merge click RIGHT NOW. Set (AcceptanceMergeable to true or false)
	// ONLY when AcceptanceID is non-empty -- both stay at their Go zero
	// value (false, "") otherwise, mirroring FindingsUnknown's own
	// "absent fact -> zero value" convention on this same struct. Before
	// this fix, the client's own hasAcceptedOverride was nothing but
	// "does an acceptance row exist", so a needs_review row with an open
	// review finding, or one with changes requested, rendered an enabled
	// Merge button that RevalidateForMerge would unconditionally refuse
	// (409) -- indistinguishable, on this row, from a row an acceptance
	// genuinely unblocks.
	//
	// buildPROpenItem computes this by calling computeRealEligibility a
	// SECOND time, WITH accepted=true, reusing the identical mandatory-
	// criteria set RevalidateForMerge itself enforces at click time (CI
	// green, blast radius known, sensitive path, every freshness check)
	// -- never a client-side or server-side heuristic re-deriving those
	// criteria independently, which is exactly how this drifted from the
	// real gate before. This SECOND call is PURELY a display computation
	// (T1, round 4, adversarial review: computeRealEligibility itself no
	// longer records anything -- see that function's own doc comment,
	// aggregate.go, and recordContestedIfApplicable's own doc comment for
	// why this call site must never be the one that does). Still
	// best-effort/non-authoritative, exactly like Kind itself (§16.2: "the
	// rendered queue is never trusted as authority") -- RevalidateForMerge
	// is re-run, unconditionally, at click time regardless of what this
	// says; a false AcceptanceMergeable therefore only ever hides a
	// button that might, rarely, still have worked (a live check settled
	// favorably between this read and a hypothetical click) -- never the
	// reverse (a shown button that 409s), which is the direction this fix
	// exists to close.
	//
	// Computed for EVERY PR-shaped row carrying an acceptance (T6, round
	// 4, adversarial review) -- ready_to_merge, needs_review, AND the
	// handoff sub-case of awaiting_approval -- never only needs_review as
	// a previous version of this comment implied: a handoff PR is refused
	// UNCONDITIONALLY by RevalidateForMerge regardless of any acceptance
	// (buildPROpenItem's own isHandoffPR branch says so with its own
	// fixed reason, no engine call needed), and a ready_to_merge row's own
	// acceptance is (trivially) mergeable, since accepted only ever
	// RELAXES the criteria an already-passing row already cleared. A
	// release cut's own isReleaseCut branch (buildPROpenItem) computes
	// AcceptanceMergeBlockedReason exactly like isHandoffPR does; only
	// AcceptanceMergeable itself stays unconditionally false there -- see
	// that branch's own comment for why.
	AcceptanceMergeable          bool
	AcceptanceMergeBlockedReason string

	// IsRelease is true iff this PR-shaped row is a release cut (§15)
	// whose §15.2 manifest check has already been computed and persisted
	// -- see resolveReleaseCut's own doc comment (aggregate.go) for the
	// full "why", including why a release PR not yet checked simply
	// renders as an ordinary PR row rather than being fabricated as one.
	// Populated unconditionally (true or false) for any PR-shaped row,
	// mirroring IsHandoff immediately above -- the field a client checks
	// to render this row's own distinct "release" shape (a link to the
	// release-review screen, never a Merge button: a release cut always
	// classifies KindNeedsReview, see buildPROpenItem) instead of the
	// ordinary PR shape.
	IsRelease bool
	// ManifestFindingsCount is §15.2's own mechanical manifest-findings
	// count (admin overrides, red-at-merge, unreviewed reverts) -- set
	// ONLY when IsRelease is true; meaningless (left at its zero value)
	// otherwise. NEVER the aggregate diff review's own composition
	// findings (§15.3/§15.4), which this codebase does not compute
	// anywhere -- see resolveReleaseCut's own doc comment.
	ManifestFindingsCount int
	// AggregateReviewTriggered is §15.3's own already-computed TRIGGER
	// decision (review.ShouldRunAggregateReview) -- set ONLY when
	// IsRelease is true. This is NOT a composition-findings count (§15.4
	// explicitly leaves that pass undispatched anywhere in this
	// codebase); it says only that the mechanical criteria for running
	// that pass were met, never that the pass itself produced anything.
	AggregateReviewTriggered bool
	// ManifestCoveragePartial is release_manifest_checks.coverage_partial
	// -- the persisted record that the constituent-PR listing this
	// manifest check ran over was TRUNCATED (MergedPRLister's own second
	// return value, §15.2), so ManifestFindingsCount above is a lower
	// bound over an incomplete set, never a complete audit. Set ONLY when
	// IsRelease is true.
	//
	// Why this is its own field rather than folded into the count: the
	// count is still a real, useful lower bound, and nulling it on the
	// wire would collide with null's established meaning there ("not a
	// release cut at all"). The alternative -- dropping the fact -- would
	// render a truncated scan's zero findings identically to a genuinely
	// clean release cut, which is precisely what reviewpost's own
	// RenderManifestComment and httpapi's own GetReleaseManifestReadout
	// (`coveragePartial`) already refuse to do for this SAME persisted
	// row. The inbox is where the human actually decides; it must not be
	// the one consumer of that row that claims a completeness guarantee
	// the port call never gave.
	ManifestCoveragePartial bool
	// CompositionReviewed/CompositionDecision are §15.3's own composition-
	// review COMPLETION/DECISION state -- confirmed-major fix: before this
	// pair existed, this row's own chip rendering (decisionInboxFormat.ts's
	// releaseChipData) had only AggregateReviewTriggered to go on, so it
	// rendered the SAME "aggregate review needed" text forever, even after
	// the composition pass had actually run, found (or not found) real
	// findings, AND been decided (Block/Acknowledge/Unblock) -- a human
	// acting on the queue could not tell "still needs the pass to run" from
	// "already handled" without leaving this row entirely. Set ONLY when
	// IsRelease is true, mirroring ManifestFindingsCount/
	// ManifestCoveragePartial's own identical gate immediately above.
	// CompositionReviewed is release_manifest_checks.composition_reviewed_at
	// IS NOT NULL (§15.3's own "not yet available" sentinel, the SAME fact
	// GetReleaseManifestReadout's own compositionReviewedAt renders).
	// CompositionDecision mirrors internal/domain/review.CompositionDecision's
	// own three values verbatim ("pending"/"blocked"/"acknowledged") --
	// meaningless (left at its Go zero value, "") whenever
	// CompositionReviewed is false, exactly like ManifestFindingsCount is
	// meaningless whenever IsRelease is false.
	CompositionReviewed bool
	CompositionDecision string

	// Plan fields (KindAwaitingApproval, non-handoff).
	PlanID string
	// SessionID is set whenever this row has a resolvable Narvi session:
	// a plan (KindAwaitingApproval), a failed session (KindNeedsAttention),
	// OR a PR-shaped row (ready_to_merge/needs_review, including a release
	// cut) for which github_pr_sessions carries a forward claim, i.e.
	// Narvi has actually run a review session against this exact PR (see
	// resolveReviewSessionID's own doc comment, aggregate.go). Left empty
	// for a PR Narvi has never been mentioned on -- a common, legitimate
	// case, never an error -- in which case a client falls back to an
	// external GitHub link instead of linking into a review screen that
	// does not exist for this PR.
	SessionID        string
	PlanSessionTitle string

	// Session fields (KindNeedsAttention: a failed, resumable session).
	FailureReason string

	// Automation fields (KindNeedsAttention: an auto-paused automation).
	AutomationID    string
	ArtifactSummary string

	// Outbox fields (KindNeedsAttention: a dead-lettered delivery).
	OutboxID   string
	OutboxKind string
	LastError  string
}
