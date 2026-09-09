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
