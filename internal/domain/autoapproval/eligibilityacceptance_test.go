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
