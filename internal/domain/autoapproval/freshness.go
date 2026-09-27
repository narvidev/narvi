package autoapproval

import "github.com/narvidev/narvi/internal/domain/review"

// FreshnessInput is CheckFreshness's own input: the half of
// EligibilityInput that decides whether a verdict still describes the
// pull request as it stands now -- its recorded context on one side, the
// pull request's live facts on the other (§21.1's amendment, §21.1b).
// Every field is EligibilityInput's field of the same name, with the
// same meaning and the same fail-conservative zero value; see that
// field's own doc comment. EligibilityInput.Freshness copies them.
//
// Nothing here is about whether the verdict is GOOD enough (CI, Shippable,
// diff size, sensitive paths): only whether it is still about the right
// code, under the current rules.
type FreshnessInput struct {
	VerdictAssessed      bool
	VerdictHeadSHA       string
	VerdictBaseRef       string
	VerdictBaseSHA       string
	VerdictAncestorChain []review.AncestorLink
	VerdictPolicyVersion int

	CurrentHeadSHA       string
	CurrentBaseRef       string
	CurrentBaseSHA       string
	CurrentAncestorChain []review.AncestorLink

	BaseAdvancedWithoutRewrite          bool
	AncestorChainAdvancedWithoutRewrite bool
}

// Freshness returns in's freshness half, field for field -- what
// computeEligibleCore hands CheckFreshness.
func (in EligibilityInput) Freshness() FreshnessInput {
	return FreshnessInput{
		VerdictAssessed:                     in.VerdictAssessed,
		VerdictHeadSHA:                      in.VerdictHeadSHA,
		VerdictBaseRef:                      in.VerdictBaseRef,
		VerdictBaseSHA:                      in.VerdictBaseSHA,
		VerdictAncestorChain:                in.VerdictAncestorChain,
		VerdictPolicyVersion:                in.VerdictPolicyVersion,
		CurrentHeadSHA:                      in.CurrentHeadSHA,
		CurrentBaseRef:                      in.CurrentBaseRef,
		CurrentBaseSHA:                      in.CurrentBaseSHA,
		CurrentAncestorChain:                in.CurrentAncestorChain,
		BaseAdvancedWithoutRewrite:          in.BaseAdvancedWithoutRewrite,
		AncestorChainAdvancedWithoutRewrite: in.AncestorChainAdvancedWithoutRewrite,
	}
}

// CheckFreshness is the ONE comparison of a verdict's recorded context
// against the pull request's live facts (§21.1b: "no consumer re-derives a
// context of its own") -- the freshness prefix of ComputeEligible's
// criteria (doc.go, items 2 to 4), in their order: assessed, head equal,
// context recorded, base commit known on both sides, base ref equal, base
// commit equal or a confirmed fast-forward, ancestor chain known and
// equal (per-link fast-forward tolerated when confirmed), policy version
// current. ReasonNone means every check passed; any other value names the
// first one that failed.
//
// computeEligibleCore calls it right after the needs-human escape hatch
// (which is a human override, not a freshness fact, and so stays first),
// so the merge path's own answer is this function's answer on every
// freshness criterion (TestCheckFreshness_EquivalentToEligibilityPrefix).
// A session's result (row 182, §43.20) calls it too, to say whether a
// verdict is current, stale or unconfirmed -- the same comparison, never a
// second one that could drift from the merge path's.
//
// Pure: a verdict can only be called current by a caller that supplies
// LIVE Current* facts. CheckFreshness has no way to know where its inputs
// came from; a caller that fills the Current* side from the recorded
// context itself learns only which checks fail on the record alone (the
// merge path's probe does exactly that, deliberately), never that the
// verdict is fresh.
func CheckFreshness(in FreshnessInput) Reason {
	// §21.1's amendment: "a review that did not complete has no risk
	// level, and inventing one ... is the same defect as a truncated scan
	// rendering as a clean one." Checked before anything else looks at
	// the verdict at all -- a not-assessed PR has no Verdict worth
	// reasoning about.
	if !in.VerdictAssessed {
		return ReasonNotAssessed
	}
	if in.VerdictHeadSHA == "" || in.VerdictHeadSHA != in.CurrentHeadSHA {
		return ReasonStaleVerdict
	}
	// §21.1's amendment: "head equality is necessary and not
	// sufficient... the gate is the verdict's whole persisted context --
	// head, base, ancestor chain and policy version -- matching the PR as
	// it stands now." VerdictBaseRef == "" means this row predates the
	// amendment: no context was ever recorded, so there is nothing to
	// confirm fresh -- treated as UNKNOWN, never as a match, exactly
	// because "treating unknown context as matching would reopen the
	// hole for every verdict already stored" (backfill: an old verdict
	// means exactly this, and forces a fresh review, never a silent
	// grandfather-in).
	if in.VerdictBaseRef == "" {
		return ReasonContextUnknown
	}
	// Finding F2: an empty base SHA on EITHER side is refused here, on its
	// own dedicated reason, BEFORE the equality comparison below ever runs
	// -- "" == "" would otherwise read as a trivially-matching pair,
	// exactly the same hole VerdictHeadSHA's own dedicated empty-string
	// check (above) already closes for the head sha. This also fails
	// closed the day a decoder regression, or a second SourceControl
	// adapter (CLAUDE.md: "don't couple a port to a single adapter" --
	// this port is EXPECTED to gain one), stops emitting either field:
	// both sides reading "" must never be indistinguishable from both
	// sides genuinely, confirmedly agreeing.
	if in.VerdictBaseSHA == "" || in.CurrentBaseSHA == "" {
		return ReasonBaseSHAUnknown
	}
	// D3 (second adversarial-review round): a base REF change (a retarget,
	// or a stacked PR's own parent merging and GitHub re-targeting onto
	// the grandparent) always refuses -- unconditionally, regardless of
	// BaseAdvancedWithoutRewrite, which says nothing about a DIFFERENT
	// branch. Split from the base-SHA comparison immediately below
	// (previously one combined condition) specifically so the ref check
	// can stay unconditional while the sha check alone gains the
	// fast-forward tolerance.
	if in.VerdictBaseRef != in.CurrentBaseRef {
		return ReasonBaseMoved
	}
	// The base SHA changed under an UNCHANGED ref -- refuse UNLESS the
	// caller has positively confirmed (BaseAdvancedWithoutRewrite) this
	// was an ordinary, unrelated fast-forward: "any unrelated merge to
	// trunk permanently disqualifies a verdict" is exactly the failure D3
	// exists to close, and BaseAdvancedWithoutRewrite's own doc comment
	// covers why this is a strict widening, never a loosening, of what
	// this engine already refuses.
	if in.VerdictBaseSHA != in.CurrentBaseSHA && !in.BaseAdvancedWithoutRewrite {
		return ReasonBaseMoved
	}
	// Round-11 finding A1: an unknown link (SHA == "") on EITHER side is
	// refused here, on its own dedicated reason, BEFORE the equality
	// comparison below ever runs -- exactly the same discipline the
	// VerdictBaseSHA/CurrentBaseSHA empty-string guard above already
	// applies one level up (finding F2): an unresolved link must never
	// silently degrade to "no ancestor chain to compare", which the
	// equality check below would otherwise treat as trivially matching a
	// genuinely-empty chain on the other side.
	if ancestorChainHasUnknownLink(in.VerdictAncestorChain) || ancestorChainHasUnknownLink(in.CurrentAncestorChain) {
		return ReasonAncestorChainUnknown
	}
	if !ancestorChainEqual(in.VerdictAncestorChain, in.CurrentAncestorChain, in.AncestorChainAdvancedWithoutRewrite) {
		return ReasonAncestorChainChanged
	}
	if in.VerdictPolicyVersion != CurrentPolicyVersion {
		return ReasonPolicyVersionMismatch
	}
	return ReasonNone
}

// FreshnessClass is what a CheckFreshness answer says about a verdict: that
// it still describes the pull request, that it provably does not, or that
// this could not be established.
type FreshnessClass string

// The three FreshnessClass values.
const (
	// FreshnessCurrent: the comparison passed. Only meaningful when the
	// Current* side was live (CheckFreshness's own doc comment).
	FreshnessCurrent FreshnessClass = "current"
	// FreshnessStale: a recorded fact and a live one disagree -- the head
	// moved, the base or the ancestor chain changed in a way not confirmed
	// forward-only, or the policy that produced the verdict is not the
	// current one.
	FreshnessStale FreshnessClass = "stale"
	// FreshnessUnconfirmed: a fact the comparison needs is missing on one
	// side -- no context was recorded, a base commit or an ancestor link is
	// unknown -- so it proves neither.
	FreshnessUnconfirmed FreshnessClass = "unconfirmed"
)

// ClassifyFreshness sorts a CheckFreshness reason into its FreshnessClass,
// following each Reason's own doc comment: the "changed" reasons are
// stale; the "could not be established" reasons are unconfirmed. Any other
// value -- ReasonNotAssessed, which a caller asking about an assessed
// verdict never gets, or a reason this function does not know -- is
// unconfirmed: never current by default.
func ClassifyFreshness(reason Reason) FreshnessClass {
	switch reason {
	case ReasonNone:
		return FreshnessCurrent
	case ReasonStaleVerdict, ReasonBaseMoved, ReasonAncestorChainChanged, ReasonPolicyVersionMismatch:
		return FreshnessStale
	default:
		return FreshnessUnconfirmed
	}
}
