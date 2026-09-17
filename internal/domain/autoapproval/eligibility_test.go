package autoapproval_test

import (
	"testing"

	"github.com/narvidev/narvi/internal/domain/autoapproval"
	"github.com/narvidev/narvi/internal/domain/review"
)

// cleanVerdict is a Shippable == auto verdict -- the one baseline every
// table case below starts from, mutating EXACTLY one field/input at a
// time, so a failing case proves that ONE criterion and no other (this
// file's own mutation-testing discipline, matching the old
// internal/domain/decisioninbox/eligibility_test.go this package
// replaces). FilesChanged/BlastRadius are still populated here, matching
// what a real posted verdict looks like, but ComputeEligible never reads either field anymore;
// see TestComputeEligible_IgnoresModelSelfReportedFilesChangedAndBlastRadius
// below for the test that pins exactly that.
func cleanVerdict() review.Verdict {
	return review.Verdict{
		RiskLevel:         review.RiskLevelLow,
		Premise:           review.PremiseStateOK,
		BlastRadius:       []review.Tag{review.TagPublicAPI}, // display data only -- see doc comment above
		FilesChanged:      5,                                 // display data only -- see doc comment above
		TestsCoverage:     review.TestsCoverageStateAdequate,
		DocsDrift:         review.DocsDriftStateNone,
		ProposedShippable: review.ProposedShippableAuto,
		Shippable:         review.ShippableAuto,
	}
}

// cleanInput's ChangedFileCount/TouchedBlastRadius are the two SERVER-DERIVED fields -- deliberately set to
// the SAME small/non-sensitive shape cleanVerdict's own (now-inert)
// FilesChanged/BlastRadius already modeled, so this baseline's own
// "eligible" outcome means the same thing it always did, just gated on
// the correct fields now. TouchedBlastRadiusKnown is explicitly true --
// Phase 5 audit findings 1+2's own fix: this field's zero value is
// "unknown" (fail closed), so a clean baseline must set it explicitly,
// exactly like every OTHER field here is explicitly set rather than
// relying on a Go zero value to mean "clean".
//
// VerdictAssessed is explicitly true, and VerdictBaseRef/VerdictBaseSHA/
// VerdictPolicyVersion explicitly match CurrentBaseRef/CurrentBaseSHA/
// autoapproval.CurrentPolicyVersion (§21.1's amendment) -- mirrors
// TouchedBlastRadiusKnown's own "explicit, never a relied-upon zero
// value" discipline immediately above, extended to every new field this
// amendment adds: a clean baseline states its own freshness explicitly
// rather than accidentally passing because both sides happen to share a
// zero value.
func cleanInput() autoapproval.EligibilityInput {
	return autoapproval.EligibilityInput{
		Verdict:                 cleanVerdict(),
		VerdictAssessed:         true,
		VerdictHeadSHA:          "abc123",
		VerdictBaseRef:          "main",
		VerdictBaseSHA:          "base-abc123",
		VerdictAncestorChain:    nil,
		VerdictPolicyVersion:    autoapproval.CurrentPolicyVersion,
		CurrentHeadSHA:          "abc123",
		CurrentBaseRef:          "main",
		CurrentBaseSHA:          "base-abc123",
		CurrentAncestorChain:    nil,
		CIGreen:                 true,
		HasNeedsHumanLabel:      false,
		ChangedFileCount:        5,
		TouchedBlastRadius:      []review.Tag{review.TagPublicAPI}, // present, but NOT in the default sensitive list
		TouchedBlastRadiusKnown: true,
	}
}

func TestComputeEligible(t *testing.T) {
	t.Parallel()

	cfg := autoapproval.DefaultEligibilityConfig()

	tests := []struct {
		name         string
		in           autoapproval.EligibilityInput
		cfg          autoapproval.EligibilityConfig
		wantEligible bool
		wantReason   autoapproval.Reason
	}{
		{
			name:         "a clean, low-risk, fresh, CI-green verdict is eligible",
			in:           cleanInput(),
			cfg:          cfg,
			wantEligible: true,
			wantReason:   autoapproval.ReasonNone,
		},

		// --- criterion 1: the needs-human escape hatch, checked FIRST,
		// regardless of every other field's value ---
		{
			name:         "needs-human label overrides an otherwise-eligible verdict",
			in:           withNeedsHuman(cleanInput(), true),
			cfg:          cfg,
			wantEligible: false,
			wantReason:   autoapproval.ReasonNeedsHumanLabel,
		},

		// --- criterion 2 (§21.1's amendment): a verdict must actually
		// have been posted at all -- "not_assessed as a first-class
		// outcome ... a review that did not complete has no risk level" ---
		{
			name:         "a PR with no posted verdict at all is not_assessed, never silently eligible",
			in:           withVerdictAssessed(cleanInput(), false),
			cfg:          cfg,
			wantEligible: false,
			wantReason:   autoapproval.ReasonNotAssessed,
		},

		// --- criterion 3: the stale-head-SHA guard ---
		{
			name:         "a verdict head sha that differs from the current head sha is stale",
			in:           withCurrentHeadSHA(cleanInput(), "def456"),
			cfg:          cfg,
			wantEligible: false,
			wantReason:   autoapproval.ReasonStaleVerdict,
		},
		{
			name:         "an empty verdict head sha is stale (never treated as a wildcard match)",
			in:           withVerdictHeadSHA(cleanInput(), ""),
			cfg:          cfg,
			wantEligible: false,
			wantReason:   autoapproval.ReasonStaleVerdict,
		},
		{
			// Isolates the empty-string clause from the inequality clause
			// next to it: with a NON-empty CurrentHeadSHA (every other case
			// in this table), "" != CurrentHeadSHA is ALREADY true, so the
			// inequality clause alone would independently catch an empty
			// VerdictHeadSHA regardless of the dedicated empty-string
			// check -- proven by mutation testing (deleting the
			// `in.VerdictHeadSHA == ""` clause left every other case in
			// this table still green). This case sets CurrentHeadSHA to ""
			// too, so ONLY the dedicated empty-string check -- never a
			// same-value coincidence -- can still refuse a verdict that
			// never recorded a real head sha in the first place.
			name: "both head shas empty is still stale, never read as a trivially-matching pair",
			in: func() autoapproval.EligibilityInput {
				in := withVerdictHeadSHA(cleanInput(), "")
				return withCurrentHeadSHA(in, "")
			}(),
			cfg:          cfg,
			wantEligible: false,
			wantReason:   autoapproval.ReasonStaleVerdict,
		},

		// --- criterion 4 (§21.1's amendment): the verdict's whole
		// recorded context -- base ref/sha, ancestor chain, policy
		// version -- must match the PR as it stands now. Head equality
		// alone (criterion 3, above) is NOT sufficient: this is the
		// exact hazard the amendment names -- "a PR evaluated while
		// based on another PR's branch, then retargeted -- or whose
		// parent moved beneath it -- keeps an unchanged head." ---
		{
			// THE DECISIVE CASE: this is the test that would pass
			// without §21.1's amendment -- VerdictHeadSHA ==
			// CurrentHeadSHA holds (cleanInput's own baseline), so
			// criterion 3 alone is satisfied, and only the NEW base
			// comparison below can catch it.
			name:         "a PR whose base changed while its head did not must lose eligibility",
			in:           withCurrentBaseRef(cleanInput(), "release/2026.09"),
			cfg:          cfg,
			wantEligible: false,
			wantReason:   autoapproval.ReasonBaseMoved,
		},
		{
			name:         "a PR whose base SHA moved (parent pushed a new commit) while the base ref name stayed the same must lose eligibility",
			in:           withCurrentBaseSHA(cleanInput(), "base-def456"),
			cfg:          cfg,
			wantEligible: false,
			wantReason:   autoapproval.ReasonBaseMoved,
		},
		// --- D3 (second adversarial-review round): "any unrelated merge
		// to trunk permanently disqualifies a verdict" -- the SAME base
		// SHA movement immediately above, but now with the caller
		// POSITIVELY CONFIRMING (BaseAdvancedWithoutRewrite) it was an
		// ordinary, unrelated fast-forward, must NOT lose eligibility.
		// This is what makes auto-merge able to regain eligibility
		// through an ordinary sequence of events (an unrelated PR merging
		// to the same trunk) rather than being permanently stuck the
		// moment any base movement is observed. ---
		{
			name:         "a PR whose base SHA moved but is CONFIRMED a pure fast-forward (an unrelated merge to trunk) keeps eligibility",
			in:           withBaseAdvancedWithoutRewrite(withCurrentBaseSHA(cleanInput(), "base-def456"), true),
			cfg:          cfg,
			wantEligible: true,
			wantReason:   autoapproval.ReasonNone,
		},
		{
			// A base REF change (a genuine retarget, or a stacked PR's
			// own parent merging and GitHub re-targeting onto the
			// grandparent) must refuse UNCONDITIONALLY -- confirming the
			// SHA relationship says nothing about a DIFFERENT branch.
			// BaseAdvancedWithoutRewrite must never be read as a
			// blanket "trust this base", only as a narrow confirmation
			// about the base SHA comparison specifically.
			name:         "a PR whose base REF changed loses eligibility even when BaseAdvancedWithoutRewrite is (incorrectly, or irrelevantly) set",
			in:           withBaseAdvancedWithoutRewrite(withCurrentBaseRef(cleanInput(), "release/2026.09"), true),
			cfg:          cfg,
			wantEligible: false,
			wantReason:   autoapproval.ReasonBaseMoved,
		},
		{
			name:         "a verdict recorded with no base ref at all (predates this amendment) is UNKNOWN context, never a match",
			in:           withVerdictBaseRef(cleanInput(), ""),
			cfg:          cfg,
			wantEligible: false,
			wantReason:   autoapproval.ReasonContextUnknown,
		},
		// --- finding F2: an empty base SHA on either side must fail
		// closed on its OWN reason, never silently compare equal to an
		// equally-empty other side. VerdictBaseRef is left non-empty in
		// every case below (cleanInput's own baseline) so
		// ReasonContextUnknown's own, earlier, unrelated check cannot be
		// what actually fires -- isolating this criterion exactly like
		// "both head shas empty" isolates its own analogous check above. ---
		{
			// THE DECISIVE F2 CASE: both sides genuinely empty must never
			// read as a trivially-matching pair -- mutation-test target:
			// deleting the `in.VerdictBaseSHA == "" || in.CurrentBaseSHA
			// == ""` guard in eligibility.go turns this case's own
			// outcome from refused back into eligible (VerdictBaseSHA ==
			// CurrentBaseSHA == "" would then satisfy the equality check
			// immediately below it).
			name: "both base shas empty is refused on its own reason, never read as a trivially-matching pair",
			in: func() autoapproval.EligibilityInput {
				in := withVerdictBaseSHA(cleanInput(), "")
				return withCurrentBaseSHA(in, "")
			}(),
			cfg:          cfg,
			wantEligible: false,
			wantReason:   autoapproval.ReasonBaseSHAUnknown,
		},
		{
			name:         "an empty verdict base sha alone (a decoder/resolution regression) is refused, distinct from a real base-moved mismatch",
			in:           withVerdictBaseSHA(cleanInput(), ""),
			cfg:          cfg,
			wantEligible: false,
			wantReason:   autoapproval.ReasonBaseSHAUnknown,
		},
		{
			name:         "an empty current base sha alone (a failed live resolution) is refused, never silently treated as a confirmed match",
			in:           withCurrentBaseSHA(cleanInput(), ""),
			cfg:          cfg,
			wantEligible: false,
			wantReason:   autoapproval.ReasonBaseSHAUnknown,
		},
		{
			name:         "an ancestor chain that no longer matches the PR's current stacked ancestry loses eligibility",
			in:           withCurrentAncestorChain(cleanInput(), []review.AncestorLink{{Ref: "main", SHA: "sha-main-new"}}),
			cfg:          cfg,
			wantEligible: false,
			wantReason:   autoapproval.ReasonAncestorChainChanged,
		},
		{
			name: "an ancestor chain that matches in content but not ORDER is still a change (order-sensitive)",
			in: withCurrentAncestorChain(
				withVerdictAncestorChain(cleanInput(), []review.AncestorLink{{Ref: "a", SHA: "1"}, {Ref: "b", SHA: "2"}}),
				[]review.AncestorLink{{Ref: "b", SHA: "2"}, {Ref: "a", SHA: "1"}},
			),
			cfg:          cfg,
			wantEligible: false,
			wantReason:   autoapproval.ReasonAncestorChainChanged,
		},
		// --- round-11 finding A1: an unknown link (empty SHA) on either
		// side must be refused on its OWN reason (ReasonAncestorChainUnknown),
		// never read as "no ancestor chain to compare" -- which would
		// otherwise trivially match a genuinely-empty chain on the other
		// side. This is the exact scenario a verifier settled by running
		// ComputeEligible in a scratch copy: {main,""} (an unresolved
		// link, this package's own "could not be established" marker) vs
		// nil (a confirmed-empty current chain) used to read as
		// ReasonAncestorChainChanged -- an ordinary "your ancestry
		// changed" answer -- when the honest answer is "this was never
		// confirmed at all". ---
		{
			// THE DECISIVE A1 CASE. Mutation-test target: deleting the
			// `ancestorChainHasUnknownLink(in.VerdictAncestorChain) ||
			// ancestorChainHasUnknownLink(in.CurrentAncestorChain)` guard
			// in eligibility.go turns this case's own outcome from
			// refused-as-unknown into refused-as-changed (still
			// ineligible, since the chain LENGTHS still differ -- 1 vs 0
			// -- but on the wrong, less honest reason, silently losing the
			// distinction this finding exists to preserve).
			name:         "a verdict recorded with an unresolved ancestor link (empty sha) against a PR reporting no ancestor at all is UNKNOWN, never a length-mismatch 'changed'",
			in:           withVerdictAncestorChain(cleanInput(), []review.AncestorLink{{Ref: "main", SHA: ""}}),
			cfg:          cfg,
			wantEligible: false,
			wantReason:   autoapproval.ReasonAncestorChainUnknown,
		},
		{
			name:         "a live ancestor-chain read that could not be resolved (empty sha) against a verdict recording no ancestor at all is UNKNOWN, never a length-mismatch 'changed'",
			in:           withCurrentAncestorChain(cleanInput(), []review.AncestorLink{{Ref: "main", SHA: ""}}),
			cfg:          cfg,
			wantEligible: false,
			wantReason:   autoapproval.ReasonAncestorChainUnknown,
		},
		{
			// Two different unknowns must never silently match each
			// other -- the identical "two unknowns are not the same
			// known fact" discipline finding F2 already established for
			// VerdictBaseSHA/CurrentBaseSHA, one level further out.
			name: "an unresolved ancestor link on BOTH sides never silently matches -- two unknowns are not the same known fact",
			in: withCurrentAncestorChain(
				withVerdictAncestorChain(cleanInput(), []review.AncestorLink{{Ref: "main", SHA: ""}}),
				[]review.AncestorLink{{Ref: "main", SHA: ""}},
			),
			cfg:          cfg,
			wantEligible: false,
			wantReason:   autoapproval.ReasonAncestorChainUnknown,
		},
		// --- round-11 finding A3: the SAME fast-forward tolerance the
		// immediate base has had since round 4's D3, one level further
		// out -- an ancestor link whose ref is unchanged but whose sha
		// advanced must NOT permanently disqualify a verdict when the
		// caller has POSITIVELY CONFIRMED (AncestorChainAdvancedWithoutRewrite)
		// it was an ordinary, unrelated fast-forward. ---
		{
			name: "an ancestor link whose sha moved but is CONFIRMED a pure fast-forward keeps eligibility",
			in: withAncestorChainAdvancedWithoutRewrite(
				withCurrentAncestorChain(
					withVerdictAncestorChain(cleanInput(), []review.AncestorLink{{Ref: "main", SHA: "stack-base-old"}}),
					[]review.AncestorLink{{Ref: "main", SHA: "stack-base-new"}},
				),
				true,
			),
			cfg:          cfg,
			wantEligible: true,
			wantReason:   autoapproval.ReasonNone,
		},
		{
			name: "an ancestor link whose sha moved WITHOUT confirmation still loses eligibility",
			in: withCurrentAncestorChain(
				withVerdictAncestorChain(cleanInput(), []review.AncestorLink{{Ref: "main", SHA: "stack-base-old"}}),
				[]review.AncestorLink{{Ref: "main", SHA: "stack-base-new"}},
			),
			cfg:          cfg,
			wantEligible: false,
			wantReason:   autoapproval.ReasonAncestorChainChanged,
		},
		{
			// A REF change (a genuine stack restructure) must refuse
			// UNCONDITIONALLY -- confirming the SHA relationship says
			// nothing about a DIFFERENT branch, mirroring
			// BaseAdvancedWithoutRewrite's own identical "never a blanket
			// trust" contract.
			name: "an ancestor link whose REF changed loses eligibility even when AncestorChainAdvancedWithoutRewrite is (incorrectly, or irrelevantly) set",
			in: withAncestorChainAdvancedWithoutRewrite(
				withCurrentAncestorChain(
					withVerdictAncestorChain(cleanInput(), []review.AncestorLink{{Ref: "main", SHA: "stack-base-old"}}),
					[]review.AncestorLink{{Ref: "other", SHA: "stack-base-new"}},
				),
				true,
			),
			cfg:          cfg,
			wantEligible: false,
			wantReason:   autoapproval.ReasonAncestorChainChanged,
		},
		{
			name:         "a verdict recorded under an earlier eligibility policy version loses eligibility",
			in:           withVerdictPolicyVersion(cleanInput(), autoapproval.CurrentPolicyVersion-1),
			cfg:          cfg,
			wantEligible: false,
			wantReason:   autoapproval.ReasonPolicyVersionMismatch,
		},
		{
			name:         "a fully matching context (base, empty ancestor chain, current policy version) stays eligible",
			in:           cleanInput(),
			cfg:          cfg,
			wantEligible: true,
			wantReason:   autoapproval.ReasonNone,
		},

		// --- criterion 5: the live CI read must have been FULLY
		// performed -- ports.OpenPR.CIConclusionDegraded's own doc
		// comment (githubapi.fetchCIConclusionLive makes two independent
		// GETs, and either can fail without the other). Set here with
		// CIGreen ALSO left true, deliberately -- proving this refusal
		// fires on CIConclusionDegraded alone, on its own dedicated
		// reason, never merely riding CIGreen's own already-false
		// fail-closed path (which a real half-read composite also takes,
		// per fetchCIConclusionLive's own terminal switch, but which this
		// test isolates from). Mutation-test target: deleting the
		// `if in.CIConclusionDegraded` branch in eligibility.go must turn
		// this case from refused back into eligible. ---
		{
			name: "a degraded CI read is never eligible, even when CIGreen itself still reads true",
			in: func() autoapproval.EligibilityInput {
				in := cleanInput()
				in.CIConclusionDegraded = true
				return in
			}(),
			cfg:          cfg,
			wantEligible: false,
			wantReason:   autoapproval.ReasonCIConclusionDegraded,
		},
		{
			// Isolates the check-ORDER, mirroring the diff-size-vs-
			// blast-radius-unknown isolation further below: a half-read
			// CI composite -- CIGreen==false AND CIConclusionDegraded==
			// true at once, exactly what fetchCIConclusionLive's own
			// terminal switch actually produces for the row's own
			// reproduction scenario (a status GET confirming success
			// beside a failing check-runs GET) -- refuses on
			// ReasonCIConclusionDegraded, not on the later-checked
			// ReasonCINotGreen, so an operator reading the reason can
			// tell "could not be fully read" apart from "confirmed red".
			name:         "a half-read CI composite refuses on CIConclusionDegraded, not merely on CI-not-green",
			in:           withCIGreen(withCIConclusionDegraded(cleanInput(), true), false),
			cfg:          cfg,
			wantEligible: false,
			wantReason:   autoapproval.ReasonCIConclusionDegraded,
		},

		// --- criterion 6: CI green at the CURRENT head ---
		{
			name:         "CI not green at head is never eligible",
			in:           withCIGreen(cleanInput(), false),
			cfg:          cfg,
			wantEligible: false,
			wantReason:   autoapproval.ReasonCINotGreen,
		},

		// --- criterion 7: Shippable == auto, exercised via three
		// DISTINCT, independently-meaningful ways Shippable can fail to
		// be auto (risk baseline, coverage floor, premise floor) -- see
		// doc.go for why this ONE check is "no floor raised" in full,
		// never a redundant separate branch ---
		{
			name: "risk baseline alone (medium) prevents shippable=auto",
			in: withVerdict(cleanInput(), func(v review.Verdict) review.Verdict {
				v.RiskLevel = review.RiskLevelMedium
				v.Shippable = review.ComputeShippable(v.RiskLevel, v.TestsCoverage, v.Premise, review.DescriptionAdequacyOK, review.CounterReviewDone)
				return v
			}),
			cfg:          cfg,
			wantEligible: false,
			wantReason:   autoapproval.ReasonNotShippableAuto,
		},
		{
			name: "the coverage floor alone (insufficient) prevents shippable=auto",
			in: withVerdict(cleanInput(), func(v review.Verdict) review.Verdict {
				v.TestsCoverage = review.TestsCoverageStateInsufficient
				v.Shippable = review.ComputeShippable(v.RiskLevel, v.TestsCoverage, v.Premise, review.DescriptionAdequacyOK, review.CounterReviewDone)
				return v
			}),
			cfg:          cfg,
			wantEligible: false,
			wantReason:   autoapproval.ReasonNotShippableAuto,
		},
		{
			name: "the premise floor alone (questionable) prevents shippable=auto",
			in: withVerdict(cleanInput(), func(v review.Verdict) review.Verdict {
				v.Premise = review.PremiseStateQuestionable
				v.Shippable = review.ComputeShippable(v.RiskLevel, v.TestsCoverage, v.Premise, review.DescriptionAdequacyOK, review.CounterReviewDone)
				return v
			}),
			cfg:          cfg,
			wantEligible: false,
			wantReason:   autoapproval.ReasonNotShippableAuto,
		},

		// --- criterion 8: diff size under the configured threshold --
		// now gated on ChangedFileCount (the
		// server-fetched fact), never Verdict.FilesChanged. ---
		{
			name:         "a diff exceeding the configured file-count threshold is not eligible",
			in:           withChangedFileCount(cleanInput(), cfg.MaxFilesChanged+1),
			cfg:          cfg,
			wantEligible: false,
			wantReason:   autoapproval.ReasonDiffTooLarge,
		},
		{
			name:         "a diff exactly AT the configured threshold is still eligible (inclusive boundary)",
			in:           withChangedFileCount(cleanInput(), cfg.MaxFilesChanged),
			cfg:          cfg,
			wantEligible: true,
			wantReason:   autoapproval.ReasonNone,
		},

		// --- criterion 9: the changed-file facts must actually be
		// KNOWN before TouchedBlastRadius is trusted at all -- Phase 5
		// audit findings 1+2 (both fixed): a failed or page-truncated
		// GitHub changed-files fetch must refuse here, distinctly from
		// "we checked and it IS sensitive" (criterion 10 immediately
		// below). Mutation-test target: reverting this check (deleting
		// the `if !in.TouchedBlastRadiusKnown` branch in eligibility.go)
		// must turn the FIRST case below from refused back into eligible
		// (TouchedBlastRadius is nil here, which touchesSensitivePath
		// reads as "nothing sensitive" if this gate is ever removed). ---
		{
			name:         "changed-file facts unknown (e.g. a swallowed GitHub fetch error) is never eligible, even with an empty blast radius",
			in:           withTouchedBlastRadiusKnown(cleanInput(), false),
			cfg:          cfg,
			wantEligible: false,
			wantReason:   autoapproval.ReasonBlastRadiusUnknown,
		},
		{
			// Isolates the check-ORDER: a diff already too large is
			// refused on ITS OWN criterion first, never on the
			// blast-radius-unknown one, even when both facts are true at
			// once -- ComputeEligible must never depend on the caller
			// having populated TouchedBlastRadiusKnown correctly before
			// its OWN, earlier, independently-reliable size gate runs.
			name: "an oversized diff refuses on diff size, not on blast-radius-unknown, even when the blast radius is ALSO unknown",
			in: func() autoapproval.EligibilityInput {
				in := withChangedFileCount(cleanInput(), cfg.MaxFilesChanged+1)
				return withTouchedBlastRadiusKnown(in, false)
			}(),
			cfg:          cfg,
			wantEligible: false,
			wantReason:   autoapproval.ReasonDiffTooLarge,
		},
		{
			// The "known-empty" case Phase 5's own fix must keep legal:
			// a GENUINELY confirmed, complete fetch that found zero
			// changed files (TouchedBlastRadiusKnown=true) is NOT the
			// same as "unknown" and must be evaluated normally, not
			// refused merely for being empty.
			name: "a confirmed, complete changed-file listing that is genuinely empty is evaluated normally (known-empty is legal, distinct from unknown)",
			in: func() autoapproval.EligibilityInput {
				in := withChangedFileCount(cleanInput(), 0)
				in = withTouchedBlastRadius(in, nil)
				return withTouchedBlastRadiusKnown(in, true)
			}(),
			cfg:          cfg,
			wantEligible: true,
			wantReason:   autoapproval.ReasonNone,
		},

		// --- criterion 10: no sensitive path touched -- gated on
		// TouchedBlastRadius (the server-DERIVED fact,
		// autoapproval.ClassifyChangedPaths over the PR's real changed
		// files), never Verdict.BlastRadius. ---
		{
			name:         "a touched-blast-radius tag in the configured sensitive list is not eligible",
			in:           withTouchedBlastRadius(cleanInput(), []review.Tag{review.TagAuth}),
			cfg:          cfg,
			wantEligible: false,
			wantReason:   autoapproval.ReasonSensitivePathTouched,
		},
		{
			name:         "a touched-blast-radius tag NOT in the configured sensitive list stays eligible",
			in:           withTouchedBlastRadius(cleanInput(), []review.Tag{review.TagDependencies}),
			cfg:          cfg,
			wantEligible: true,
			wantReason:   autoapproval.ReasonNone,
		},
		{
			name:         "a custom per-repo sensitive list is honored over the built-in default",
			in:           withTouchedBlastRadius(cleanInput(), []review.Tag{review.TagDependencies}),                                                // not in the DEFAULT list...
			cfg:          autoapproval.EligibilityConfig{MaxFilesChanged: cfg.MaxFilesChanged, SensitiveTags: []review.Tag{review.TagDependencies}}, // ...but IS in this repo's own custom list
			wantEligible: false,
			wantReason:   autoapproval.ReasonSensitivePathTouched,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			gotEligible, gotReason := autoapproval.ComputeEligible(tc.in, tc.cfg)
			if gotEligible != tc.wantEligible {
				t.Errorf("ComputeEligible(...) eligible = %v, want %v", gotEligible, tc.wantEligible)
			}
			if gotReason != tc.wantReason {
				t.Errorf("ComputeEligible(...) reason = %q, want %q", gotReason, tc.wantReason)
			}
		})
	}
}

// TestComputeEligible_IgnoresModelSelfReportedFilesChangedAndBlastRadius is
// the C1 regression test at the pure-function
// level: this is the exact attack the reviewers verified reproducible --
// a reviewing agent posts a verdict with a tiny self-reported FilesChanged
// and an empty self-reported BlastRadius (so Shippable legitimately
// computes to auto), while ChangedFileCount/TouchedBlastRadius -- the
// SERVER-DERIVED facts a caller populates from the PR's real
// ports.OpenPR -- tell a completely different story: a 300-file diff
// touching a sensitive path. Both must independently refuse eligibility,
// proving the engine no longer trusts the model's own self-report for
// either criterion.
func TestComputeEligible_IgnoresModelSelfReportedFilesChangedAndBlastRadius(t *testing.T) {
	t.Parallel()

	cfg := autoapproval.DefaultEligibilityConfig()

	lyingVerdict := review.Verdict{
		RiskLevel:         review.RiskLevelLow,
		Premise:           review.PremiseStateOK,
		TestsCoverage:     review.TestsCoverageStateAdequate,
		DocsDrift:         review.DocsDriftStateNone,
		ProposedShippable: review.ProposedShippableAuto,
		FilesChanged:      1,   // LIE: the model claims a tiny, one-file diff...
		BlastRadius:       nil, // ...and claims it touches nothing sensitive at all.
	}
	lyingVerdict.Shippable = review.ComputeShippable(lyingVerdict.RiskLevel, lyingVerdict.TestsCoverage, lyingVerdict.Premise, review.DescriptionAdequacyOK, review.CounterReviewDone)
	if lyingVerdict.Shippable != review.ShippableAuto {
		t.Fatalf("test setup: lyingVerdict.Shippable = %v, want auto (the attack's own premise is that the server-computed Shippable legitimately computes to auto)", lyingVerdict.Shippable)
	}

	t.Run("diff size: real 300-file PR refuses despite a lying low FilesChanged", func(t *testing.T) {
		t.Parallel()
		in := autoapproval.EligibilityInput{
			Verdict:                 lyingVerdict,
			VerdictAssessed:         true,
			VerdictHeadSHA:          "sha-under-attack",
			VerdictBaseRef:          "main",
			VerdictBaseSHA:          "base-under-attack",
			VerdictPolicyVersion:    autoapproval.CurrentPolicyVersion,
			CurrentHeadSHA:          "sha-under-attack",
			CurrentBaseRef:          "main",
			CurrentBaseSHA:          "base-under-attack",
			CIGreen:                 true,
			HasNeedsHumanLabel:      false,
			ChangedFileCount:        300, // TRUTH: GitHub itself reports 300 changed files.
			TouchedBlastRadius:      nil,
			TouchedBlastRadiusKnown: true,
		}
		eligible, reason := autoapproval.ComputeEligible(in, cfg)
		if eligible {
			t.Fatal("ComputeEligible(...) eligible = true, want false -- a real 300-file diff must refuse regardless of what the model itself self-reported")
		}
		if reason != autoapproval.ReasonDiffTooLarge {
			t.Errorf("reason = %q, want %q", reason, autoapproval.ReasonDiffTooLarge)
		}
	})

	t.Run("sensitive path: real migrations+authz paths refuse despite a lying empty BlastRadius", func(t *testing.T) {
		t.Parallel()
		in := autoapproval.EligibilityInput{
			Verdict:              lyingVerdict,
			VerdictAssessed:      true,
			VerdictHeadSHA:       "sha-under-attack",
			VerdictBaseRef:       "main",
			VerdictBaseSHA:       "base-under-attack",
			VerdictPolicyVersion: autoapproval.CurrentPolicyVersion,
			CurrentHeadSHA:       "sha-under-attack",
			CurrentBaseRef:       "main",
			CurrentBaseSHA:       "base-under-attack",
			CIGreen:              true,
			HasNeedsHumanLabel:   false,
			ChangedFileCount:     1, // small, so this subtest isolates the sensitive-path criterion specifically
			// TRUTH: autoapproval.ClassifyChangedPaths, applied to the PR's
			// real changed files, found migrations + authz touched -- see
			// TestRevalidateForMerge_LyingVerdictAgainstReal300FileSensitivePR
			// (internal/app/decisioninbox) for this SAME attack proven at
			// the full integration level, through the real classifier
			// over real ports.OpenPR.ChangedFiles rather than a
			// pre-computed TouchedBlastRadius literal.
			TouchedBlastRadius:      []review.Tag{review.TagMigrations, review.TagAuth},
			TouchedBlastRadiusKnown: true, // confirmed, complete listing -- this subtest is about the SENSITIVE-PATH criterion, not the unknown-facts one
		}
		eligible, reason := autoapproval.ComputeEligible(in, cfg)
		if eligible {
			t.Fatal("ComputeEligible(...) eligible = true, want false -- a real sensitive-path touch must refuse regardless of what the model itself self-reported")
		}
		if reason != autoapproval.ReasonSensitivePathTouched {
			t.Errorf("reason = %q, want %q", reason, autoapproval.ReasonSensitivePathTouched)
		}
	})

	t.Run("conversely: a huge lying FilesChanged/sensitive lying BlastRadius do NOT block an actually-small/clean PR", func(t *testing.T) {
		t.Parallel()
		// The other direction of the SAME fix: Verdict.FilesChanged/
		// BlastRadius must be fully INERT, not just "insufficient on their
		// own to approve" -- a model that (for whatever reason)
		// over-reports risk must not be able to block a genuinely small,
		// clean PR either. Confirms the fields are ignored, not merely
		// overridden-when-worse.
		overReportingVerdict := lyingVerdict
		overReportingVerdict.FilesChanged = 9999
		overReportingVerdict.BlastRadius = []review.Tag{review.TagAuth, review.TagMigrations, review.TagSecrets}

		in := autoapproval.EligibilityInput{
			Verdict:                 overReportingVerdict,
			VerdictAssessed:         true,
			VerdictHeadSHA:          "sha-clean",
			VerdictBaseRef:          "main",
			VerdictBaseSHA:          "base-clean",
			VerdictPolicyVersion:    autoapproval.CurrentPolicyVersion,
			CurrentHeadSHA:          "sha-clean",
			CurrentBaseRef:          "main",
			CurrentBaseSHA:          "base-clean",
			CIGreen:                 true,
			HasNeedsHumanLabel:      false,
			ChangedFileCount:        1,
			TouchedBlastRadius:      nil,
			TouchedBlastRadiusKnown: true,
		}
		eligible, reason := autoapproval.ComputeEligible(in, cfg)
		if !eligible {
			t.Errorf("ComputeEligible(...) eligible = false (reason %q), want true -- Verdict.FilesChanged/BlastRadius must be fully inert, never gate anything in either direction", reason)
		}
	})
}

// TestDefaultEligibilityConfig_ReturnsFreshSlice pins that
// DefaultSensitiveTags never hands back a slice two callers could
// mutate into affecting each other -- a caller appending to (or
// overwriting an element of) its own returned slice must never leak into
// a LATER, independent call's own result.
func TestDefaultEligibilityConfig_ReturnsFreshSlice(t *testing.T) {
	t.Parallel()

	first := autoapproval.DefaultSensitiveTags()
	first[0] = review.Tag("tampered")

	second := autoapproval.DefaultSensitiveTags()
	if second[0] == review.Tag("tampered") {
		t.Fatalf("DefaultSensitiveTags: mutating one call's result affected a later call -- want an independent slice each time")
	}
}

func withNeedsHuman(in autoapproval.EligibilityInput, v bool) autoapproval.EligibilityInput {
	in.HasNeedsHumanLabel = v
	return in
}
func withCIGreen(in autoapproval.EligibilityInput, v bool) autoapproval.EligibilityInput {
	in.CIGreen = v
	return in
}
func withCIConclusionDegraded(in autoapproval.EligibilityInput, v bool) autoapproval.EligibilityInput {
	in.CIConclusionDegraded = v
	return in
}
func withCurrentHeadSHA(in autoapproval.EligibilityInput, sha string) autoapproval.EligibilityInput {
	in.CurrentHeadSHA = sha
	return in
}
func withVerdictHeadSHA(in autoapproval.EligibilityInput, sha string) autoapproval.EligibilityInput {
	in.VerdictHeadSHA = sha
	return in
}
func withVerdict(in autoapproval.EligibilityInput, mutate func(review.Verdict) review.Verdict) autoapproval.EligibilityInput {
	in.Verdict = mutate(in.Verdict)
	return in
}
func withChangedFileCount(in autoapproval.EligibilityInput, n int) autoapproval.EligibilityInput {
	in.ChangedFileCount = n
	return in
}
func withTouchedBlastRadius(in autoapproval.EligibilityInput, tags []review.Tag) autoapproval.EligibilityInput {
	in.TouchedBlastRadius = tags
	return in
}
func withTouchedBlastRadiusKnown(in autoapproval.EligibilityInput, known bool) autoapproval.EligibilityInput {
	in.TouchedBlastRadiusKnown = known
	return in
}
func withVerdictAssessed(in autoapproval.EligibilityInput, v bool) autoapproval.EligibilityInput {
	in.VerdictAssessed = v
	return in
}
func withVerdictBaseRef(in autoapproval.EligibilityInput, ref string) autoapproval.EligibilityInput {
	in.VerdictBaseRef = ref
	return in
}
func withCurrentBaseRef(in autoapproval.EligibilityInput, ref string) autoapproval.EligibilityInput {
	in.CurrentBaseRef = ref
	return in
}
func withCurrentBaseSHA(in autoapproval.EligibilityInput, sha string) autoapproval.EligibilityInput {
	in.CurrentBaseSHA = sha
	return in
}
func withVerdictBaseSHA(in autoapproval.EligibilityInput, sha string) autoapproval.EligibilityInput {
	in.VerdictBaseSHA = sha
	return in
}

// withBaseAdvancedWithoutRewrite (D3, second adversarial-review round)
// sets EligibilityInput.BaseAdvancedWithoutRewrite -- see that field's
// own doc comment for what it confirms and why the zero value (false)
// must remain the fail-conservative default.
func withBaseAdvancedWithoutRewrite(in autoapproval.EligibilityInput, confirmed bool) autoapproval.EligibilityInput {
	in.BaseAdvancedWithoutRewrite = confirmed
	return in
}
func withVerdictAncestorChain(in autoapproval.EligibilityInput, chain []review.AncestorLink) autoapproval.EligibilityInput {
	in.VerdictAncestorChain = chain
	return in
}
func withCurrentAncestorChain(in autoapproval.EligibilityInput, chain []review.AncestorLink) autoapproval.EligibilityInput {
	in.CurrentAncestorChain = chain
	return in
}
func withVerdictPolicyVersion(in autoapproval.EligibilityInput, v int) autoapproval.EligibilityInput {
	in.VerdictPolicyVersion = v
	return in
}

// withAncestorChainAdvancedWithoutRewrite (round-11 finding A3) sets
// EligibilityInput.AncestorChainAdvancedWithoutRewrite -- mirrors
// withBaseAdvancedWithoutRewrite's own identical shape one level further
// out; see that field's own doc comment (eligibility.go) for what it
// confirms and why the zero value (false) must remain the
// fail-conservative default.
func withAncestorChainAdvancedWithoutRewrite(in autoapproval.EligibilityInput, confirmed bool) autoapproval.EligibilityInput {
	in.AncestorChainAdvancedWithoutRewrite = confirmed
	return in
}
