package review_test

import (
	"testing"

	"github.com/narvidev/narvi/internal/domain/review"
)

// TestVerdict_ZeroValueIsNotAuto proves that an unpopulated Verdict (e.g.
// one constructed by mistake without going through ComputeShippable at
// all) reads as an unrecognized, non-"auto" Shippable -- the same
// zero-value-detectability property every enum in this package relies on.
// A caller that forgets to populate Shippable gets a value that is
// visibly wrong (neither ShippableAuto, ShippableNeedsHuman, nor
// ShippableBlock), never one that silently passes an `== ShippableAuto`
// eligibility check.
func TestVerdict_ZeroValueIsNotAuto(t *testing.T) {
	t.Parallel()

	var v review.Verdict
	if v.Shippable == review.ShippableAuto {
		t.Error("a zero-value Verdict.Shippable must not equal ShippableAuto")
	}
	if v.Shippable == review.ShippableNeedsHuman || v.Shippable == review.ShippableBlock {
		t.Error("a zero-value Verdict.Shippable must not equal any legal Shippable value")
	}
}

// TestVerdict_ShippableComputedFromFields builds a Verdict the way a
// caller (a later Step's verdict-posting tool) is contractually expected
// to: populating Shippable with exactly the Class of ComputeShippable's
// own result, derived from the same RiskLevel/TestsCoverage/Premise fields
// carried on the same Verdict, never a hand-set or ProposedShippable-
// derived value. This exercises Verdict's own documented CONTRACT
// end-to-end.
func TestVerdict_ShippableComputedFromFields(t *testing.T) {
	t.Parallel()

	v := review.Verdict{
		RiskLevel:         review.RiskLevelHigh,
		Premise:           review.PremiseStateQuestionable,
		BlastRadius:       []review.Tag{review.TagAuth, review.TagContracts},
		FilesChanged:      12,
		TestsCoverage:     review.TestsCoverageStateInsufficient,
		DocsDrift:         review.DocsDriftStateNone,
		ProposedShippable: review.ProposedShippableAuto,
	}
	v.Shippable = review.ComputeShippable(v.RiskLevel, v.TestsCoverage, v.Premise, review.DescriptionAdequacyOK, review.CounterReviewDone).Class()

	want := review.ShippableNeedsHuman // high baseline + insufficient floor, both needs_human; premise floor (questionable) also needs_human
	if v.Shippable != want {
		t.Fatalf("Verdict.Shippable = %s, want %s", v.Shippable, want)
	}

	// The model's own (wrong, optimistic) self-report must never have
	// influenced the authoritative field -- ProposedShippableAuto and the
	// computed ShippableNeedsHuman visibly disagree here, exactly the
	// scenario §21.2's "never trust the model's own verdict" rule exists
	// for.
	if string(v.ProposedShippable) == string(v.Shippable) {
		t.Fatal("test fixture error: this case is only meaningful when the model's proposal and the computed value disagree")
	}
}

// TestVerdict_ProposedShippableRequiresExplicitConversion documents (it
// cannot "test" a compile-time property at runtime) that ProposedShippable
// and Shippable are distinct Go types: assigning one to the other requires
// an explicit conversion, exercised here deliberately, so that a reviewer
// of this test can see exactly what such a conversion looks like -- the
// one visible, grep-able act doc.go's "server-computed Shippable" section
// says any laundering of a model's guess into the authoritative field
// would have to take.
func TestVerdict_ProposedShippableRequiresExplicitConversion(t *testing.T) {
	t.Parallel()

	proposed := review.ProposedShippableBlock
	converted := review.Shippable(proposed) // explicit conversion, never implicit
	if converted != review.ShippableBlock {
		t.Errorf("Shippable(ProposedShippableBlock) = %s, want %s", converted, review.ShippableBlock)
	}
}
