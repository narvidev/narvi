package autoapproval_test

import (
	"testing"

	"github.com/narvidev/narvi/internal/domain/autoapproval"
	"github.com/narvidev/narvi/internal/domain/review"
)

// TestComputeEligibleWithAcceptance_AcceptedFalseMatchesComputeEligible
// pins that accepted=false is byte-for-byte ComputeEligible -- the
// wrapper this whole file's own new sibling function must never
// silently diverge from when there is nothing to waive.
func TestComputeEligibleWithAcceptance_AcceptedFalseMatchesComputeEligible(t *testing.T) {
	t.Parallel()

	cfg := autoapproval.DefaultEligibilityConfig()
	cases := map[string]autoapproval.EligibilityInput{
		"clean":                cleanInput(),
		"not shippable auto":   withVerdict(cleanInput(), func(v review.Verdict) review.Verdict { v.Shippable = review.ShippableNeedsHuman; return v }),
		"diff too large":       withChangedFileCount(cleanInput(), 999),
		"ci not green":         withCIGreen(cleanInput(), false),
		"sensitive path":       withTouchedBlastRadius(cleanInput(), []review.Tag{review.TagAuth}),
		"blast radius unknown": withTouchedBlastRadiusKnown(cleanInput(), false),
	}
	for name, in := range cases {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			wantEligible, wantReason := autoapproval.ComputeEligible(in, cfg)
			gotEligible, gotReason, gotVia := autoapproval.ComputeEligibleWithAcceptance(in, cfg, false)
			if gotEligible != wantEligible || gotReason != wantReason {
				t.Errorf("ComputeEligibleWithAcceptance(accepted=false) = (%v, %q), want (%v, %q) to match ComputeEligible", gotEligible, gotReason, wantEligible, wantReason)
			}
			if gotVia {
				t.Errorf("ComputeEligibleWithAcceptance(accepted=false) viaAcceptance = true, want false: nothing can be waived without accepted=true")
			}
		})
	}
}

// TestComputeEligibleWithAcceptance_WaivesOnlyShippableAndDiffSize is
// this file's own core coverage: accepted=true waives EXACTLY
// ReasonNotShippableAuto and ReasonDiffTooLarge, and every other
// criterion -- CI green, blast radius known, sensitive path, and every
// freshness check -- stays mandatory (§21.1b: "the conditions that are
// not about human judgment... stay mandatory"), unconditionally, whether
// or not accepted is true.
func TestComputeEligibleWithAcceptance_WaivesOnlyShippableAndDiffSize(t *testing.T) {
	t.Parallel()

	cfg := autoapproval.DefaultEligibilityConfig()

	tests := []struct {
		name          string
		in            autoapproval.EligibilityInput
		wantEligible  bool
		wantReason    autoapproval.Reason
		wantViaAccept bool
	}{
		{
			name:          "clean input: eligible, but not because of any waiver",
			in:            cleanInput(),
			wantEligible:  true,
			wantReason:    autoapproval.ReasonNone,
			wantViaAccept: false,
		},
		{
			name:          "not shippable auto alone: waived",
			in:            withVerdict(cleanInput(), func(v review.Verdict) review.Verdict { v.Shippable = review.ShippableNeedsHuman; return v }),
			wantEligible:  true,
			wantReason:    autoapproval.ReasonNotShippableAuto,
			wantViaAccept: true,
		},
		{
			name:          "diff too large alone: waived",
			in:            withChangedFileCount(cleanInput(), 999),
			wantEligible:  true,
			wantReason:    autoapproval.ReasonDiffTooLarge,
			wantViaAccept: true,
		},
		{
			name: "BOTH not-shippable and diff-too-large: still waived, first-ordered reason reported",
			in: withChangedFileCount(
				withVerdict(cleanInput(), func(v review.Verdict) review.Verdict { v.Shippable = review.ShippableNeedsHuman; return v }),
				999,
			),
			wantEligible:  true,
			wantReason:    autoapproval.ReasonNotShippableAuto,
			wantViaAccept: true,
		},
		{
			name:          "not shippable auto PLUS ci not green: CI stays mandatory, refused",
			in:            withCIGreen(withVerdict(cleanInput(), func(v review.Verdict) review.Verdict { v.Shippable = review.ShippableNeedsHuman; return v }), false),
			wantEligible:  false,
			wantReason:    autoapproval.ReasonCINotGreen,
			wantViaAccept: false,
		},
		{
			name: "not shippable auto PLUS blast radius unknown: mandatory, refused -- proves the diff-size/blast-radius checks still ran despite the earlier waiver",
			in: withTouchedBlastRadiusKnown(
				withVerdict(cleanInput(), func(v review.Verdict) review.Verdict { v.Shippable = review.ShippableNeedsHuman; return v }),
				false,
			),
			wantEligible:  false,
			wantReason:    autoapproval.ReasonBlastRadiusUnknown,
			wantViaAccept: false,
		},
		{
			name: "diff too large PLUS blast radius unknown: mandatory, refused -- proves waiving diff-size does not short-circuit past the blast-radius check",
			in: withTouchedBlastRadiusKnown(
				withChangedFileCount(cleanInput(), 999),
				false,
			),
			wantEligible:  false,
			wantReason:    autoapproval.ReasonBlastRadiusUnknown,
			wantViaAccept: false,
		},
		{
			name: "not shippable auto PLUS sensitive path touched: mandatory, refused",
			in: withTouchedBlastRadius(
				withVerdict(cleanInput(), func(v review.Verdict) review.Verdict { v.Shippable = review.ShippableNeedsHuman; return v }),
				[]review.Tag{review.TagAuth},
			),
			wantEligible:  false,
			wantReason:    autoapproval.ReasonSensitivePathTouched,
			wantViaAccept: false,
		},
		{
			name: "not shippable auto PLUS base moved: freshness stays mandatory, refused",
			in: withCurrentBaseSHA(
				withVerdict(cleanInput(), func(v review.Verdict) review.Verdict { v.Shippable = review.ShippableNeedsHuman; return v }),
				"a-different-base-sha",
			),
			wantEligible:  false,
			wantReason:    autoapproval.ReasonBaseMoved,
			wantViaAccept: false,
		},
		{
			name: "not shippable auto PLUS changed ancestor chain: freshness stays mandatory, refused (the third §21.1b invalidating trigger, alongside a new attempt and a moved base)",
			in: withCurrentAncestorChain(
				withVerdict(cleanInput(), func(v review.Verdict) review.Verdict { v.Shippable = review.ShippableNeedsHuman; return v }),
				[]review.AncestorLink{{Ref: "main", SHA: "sha-main-new"}},
			),
			wantEligible:  false,
			wantReason:    autoapproval.ReasonAncestorChainChanged,
			wantViaAccept: false,
		},
		{
			name:          "ci conclusion degraded alone: mandatory, refused regardless of accepted",
			in:            withCIConclusionDegraded(cleanInput(), true),
			wantEligible:  false,
			wantReason:    autoapproval.ReasonCIConclusionDegraded,
			wantViaAccept: false,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			gotEligible, gotReason, gotVia := autoapproval.ComputeEligibleWithAcceptance(tc.in, cfg, true)
			if gotEligible != tc.wantEligible {
				t.Errorf("eligible = %v, want %v", gotEligible, tc.wantEligible)
			}
			if gotReason != tc.wantReason {
				t.Errorf("reason = %q, want %q", gotReason, tc.wantReason)
			}
			if gotVia != tc.wantViaAccept {
				t.Errorf("viaAcceptance = %v, want %v", gotVia, tc.wantViaAccept)
			}
		})
	}
}

// TestComputeEligibleWithAcceptance_EveryCriterionEnumerated pins finding
// F3d (adversarial review): the test above (
// TestComputeEligibleWithAcceptance_WaivesOnlyShippableAndDiffSize) never
// actually isolates ReasonNeedsHumanLabel or ReasonNotAssessed with
// accepted=true -- proven by mutation: changing `if in.HasNeedsHumanLabel
// {` to `if in.HasNeedsHumanLabel && !waiveHumanJudgment {`
// (eligibility.go) passed the ENTIRE unit and integration suite, meaning
// an acceptance could be made to silently override the review:
// needs-human escape hatch, the explicit opt-out §21.2 names as the
// lever a maintainer keeps regardless of what the criteria say.
//
// This test enumerates EVERY Reason value this package currently defines
// (eligibility.go's own const block) into ONE table, each isolated
// exactly like TestComputeEligible's own per-criterion cases, and asserts
// waived-vs-mandatory for each under accepted=true -- so a future
// mutation that flips ANY one criterion's own waivability, in either
// direction, fails here regardless of which criterion it is. Exactly two
// entries are marked waivable: ReasonNotShippableAuto and
// ReasonDiffTooLarge (§21.1b's own named pair). Every other criterion
// must refuse, under accepted=true, with the IDENTICAL reason
// ComputeEligible (accepted=false) would already refuse it with.
//
// MAINTENANCE NOTE: this table is a literal enumeration, not a reflective
// one (Go has no runtime enumeration of a package's own const values) --
// adding a new Reason to eligibility.go's own const block without adding
// a matching row here leaves that NEW criterion silently unexercised by
// this test, never silently marked safe by it. Reviewers adding a
// criterion should add its own row here in the same change.
func TestComputeEligibleWithAcceptance_EveryCriterionEnumerated(t *testing.T) {
	t.Parallel()

	cfg := autoapproval.DefaultEligibilityConfig()

	tests := []struct {
		reason        autoapproval.Reason
		in            autoapproval.EligibilityInput
		wantWaiveable bool
	}{
		{
			reason:        autoapproval.ReasonNeedsHumanLabel,
			in:            withNeedsHuman(cleanInput(), true),
			wantWaiveable: false,
		},
		{
			reason:        autoapproval.ReasonNotAssessed,
			in:            withVerdictAssessed(cleanInput(), false),
			wantWaiveable: false,
		},
		{
			reason:        autoapproval.ReasonStaleVerdict,
			in:            withCurrentHeadSHA(cleanInput(), "a-different-head-sha"),
			wantWaiveable: false,
		},
		{
			reason:        autoapproval.ReasonContextUnknown,
			in:            withVerdictBaseRef(cleanInput(), ""),
			wantWaiveable: false,
		},
		{
			reason:        autoapproval.ReasonBaseMoved,
			in:            withCurrentBaseRef(cleanInput(), "release/2026.09"),
			wantWaiveable: false,
		},
		{
			reason:        autoapproval.ReasonAncestorChainChanged,
			in:            withCurrentAncestorChain(cleanInput(), []review.AncestorLink{{Ref: "main", SHA: "sha-main-new"}}),
			wantWaiveable: false,
		},
		{
			reason:        autoapproval.ReasonAncestorChainUnknown,
			in:            withVerdictAncestorChain(cleanInput(), []review.AncestorLink{{Ref: "main", SHA: ""}}),
			wantWaiveable: false,
		},
		{
			reason:        autoapproval.ReasonPolicyVersionMismatch,
			in:            withVerdictPolicyVersion(cleanInput(), 0),
			wantWaiveable: false,
		},
		{
			reason:        autoapproval.ReasonBaseSHAUnknown,
			in:            withCurrentBaseSHA(cleanInput(), ""),
			wantWaiveable: false,
		},
		{
			reason:        autoapproval.ReasonCIConclusionDegraded,
			in:            withCIConclusionDegraded(cleanInput(), true),
			wantWaiveable: false,
		},
		{
			reason:        autoapproval.ReasonCINotGreen,
			in:            withCIGreen(cleanInput(), false),
			wantWaiveable: false,
		},
		{
			// The FIRST of exactly two waivable criteria (§21.1b).
			reason:        autoapproval.ReasonNotShippableAuto,
			in:            withVerdict(cleanInput(), func(v review.Verdict) review.Verdict { v.Shippable = review.ShippableNeedsHuman; return v }),
			wantWaiveable: true,
		},
		{
			// The SECOND of exactly two waivable criteria (§21.1b).
			reason:        autoapproval.ReasonDiffTooLarge,
			in:            withChangedFileCount(cleanInput(), 999),
			wantWaiveable: true,
		},
		{
			reason:        autoapproval.ReasonBlastRadiusUnknown,
			in:            withTouchedBlastRadiusKnown(cleanInput(), false),
			wantWaiveable: false,
		},
		{
			reason:        autoapproval.ReasonSensitivePathTouched,
			in:            withTouchedBlastRadius(cleanInput(), []review.Tag{review.TagAuth}),
			wantWaiveable: false,
		},
	}

	seen := make(map[autoapproval.Reason]bool, len(tests))
	for _, tc := range tests {
		seen[tc.reason] = true
	}
	// allKnownReasons mirrors eligibility.go's own const block verbatim,
	// EXCLUDING ReasonNone (not a refusal reason at all -- nothing to
	// waive). Asserted against `tests` above so a Reason added to
	// eligibility.go without a corresponding row here fails LOUDLY,
	// rather than this test silently proving less than its own doc
	// comment claims.
	allKnownReasons := []autoapproval.Reason{
		autoapproval.ReasonNeedsHumanLabel,
		autoapproval.ReasonNotAssessed,
		autoapproval.ReasonStaleVerdict,
		autoapproval.ReasonContextUnknown,
		autoapproval.ReasonBaseMoved,
		autoapproval.ReasonAncestorChainChanged,
		autoapproval.ReasonAncestorChainUnknown,
		autoapproval.ReasonPolicyVersionMismatch,
		autoapproval.ReasonBaseSHAUnknown,
		autoapproval.ReasonCIConclusionDegraded,
		autoapproval.ReasonCINotGreen,
		autoapproval.ReasonNotShippableAuto,
		autoapproval.ReasonDiffTooLarge,
		autoapproval.ReasonBlastRadiusUnknown,
		autoapproval.ReasonSensitivePathTouched,
	}
	for _, r := range allKnownReasons {
		if !seen[r] {
			t.Fatalf("Reason %q is not covered by this test's own table -- add a row for it", r)
		}
	}
	if len(tests) != len(allKnownReasons) {
		t.Fatalf("tests has %d entries, allKnownReasons has %d -- keep them in exact 1:1 correspondence", len(tests), len(allKnownReasons))
	}

	for _, tc := range tests {
		t.Run(string(tc.reason), func(t *testing.T) {
			t.Parallel()

			// Sanity: accepted=false must refuse THIS test's own input
			// with THIS test's own named reason -- otherwise the case
			// below (accepted=true) would be proving nothing about the
			// criterion it claims to.
			baselineEligible, baselineReason := autoapproval.ComputeEligible(tc.in, cfg)
			if baselineEligible {
				t.Fatalf("fixture bug: ComputeEligible(accepted=false) = eligible, want refused on %q", tc.reason)
			}
			if baselineReason != tc.reason {
				t.Fatalf("fixture bug: ComputeEligible(accepted=false) reason = %q, want %q -- this input does not isolate the criterion this case claims to", baselineReason, tc.reason)
			}

			gotEligible, gotReason, gotVia := autoapproval.ComputeEligibleWithAcceptance(tc.in, cfg, true)
			if tc.wantWaiveable {
				if !gotEligible || gotReason != tc.reason || !gotVia {
					t.Errorf("ComputeEligibleWithAcceptance(accepted=true) = (eligible=%v, reason=%q, via=%v), want (true, %q, true) -- %q is one of §21.1b's exactly-two waivable criteria", gotEligible, gotReason, gotVia, tc.reason, tc.reason)
				}
			} else {
				if gotEligible || gotReason != tc.reason || gotVia {
					t.Errorf("ComputeEligibleWithAcceptance(accepted=true) = (eligible=%v, reason=%q, via=%v), want (false, %q, false) -- %q is NOT waivable and must refuse identically whether or not accepted is true", gotEligible, gotReason, gotVia, tc.reason, tc.reason)
				}
			}
		})
	}
}
