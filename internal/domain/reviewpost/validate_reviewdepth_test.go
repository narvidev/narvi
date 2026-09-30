package reviewpost_test

import (
	"errors"
	"testing"

	"github.com/narvidev/narvi/internal/domain/review"
	"github.com/narvidev/narvi/internal/domain/reviewpost"
	"github.com/narvidev/narvi/internal/domain/reviewtriage"
)

// deepValidInput mirrors validInput() but additionally fills in the three
// §26.3 deep-path-only digest fields, marks ReviewDepth deep, and
// (§26.4) sets CounterReview -- a caller on the deep path must
// pass ALL of these to validate.
func deepValidInput() reviewpost.VerdictInput {
	in := validInput()
	in.ReviewDepth = reviewtriage.DepthDeep
	in.Digest.ArchDecisions = []reviewpost.ArchDecision{
		{Decision: "Introduced a new triage package", RejectedAlternative: "Folding it into review", ConventionConformance: "Matches the one-pure-function-per-concern pattern"},
	}
	in.Digest.StackRisks = "None beyond the new migration."
	in.Digest.UnverifiedLimits = "Did not test against a live GitHub org with real stacked PRs."
	in.CounterReview = review.CounterReviewDone
	return in
}

func TestValidateVerdictInput_DeepPath(t *testing.T) {
	tests := []struct {
		name    string
		mutate  func(in *reviewpost.VerdictInput)
		wantErr error
	}{
		{
			name:    "fully populated deep-path digest is valid",
			mutate:  func(_ *reviewpost.VerdictInput) {},
			wantErr: nil,
		},
		{
			name:    "deep path with empty ArchDecisions is rejected",
			mutate:  func(in *reviewpost.VerdictInput) { in.Digest.ArchDecisions = nil },
			wantErr: reviewpost.ErrEmptyDigestArchDecisions,
		},
		{
			// Adversarial-review fix, D2's own "hollow check" aggravator:
			// a non-empty slice containing only an all-blank ArchDecision
			// must be rejected exactly like an empty slice -- a bare
			// len() > 0 check would have let this through.
			name: "deep path with a single all-blank ArchDecision entry is rejected",
			mutate: func(in *reviewpost.VerdictInput) {
				in.Digest.ArchDecisions = []reviewpost.ArchDecision{{}}
			},
			wantErr: reviewpost.ErrEmptyDigestArchDecisions,
		},
		{
			// A non-blank field in ANY position (not just Decision) still
			// counts -- this is a "does at least one field carry real
			// content" check, not "is Decision specifically non-blank".
			name: "deep path with only ConventionConformance filled in is accepted",
			mutate: func(in *reviewpost.VerdictInput) {
				in.Digest.ArchDecisions = []reviewpost.ArchDecision{{ConventionConformance: "matches CLAUDE.md's no-I/O-in-domain rule"}}
			},
			wantErr: nil,
		},
		{
			// A mix of one all-blank entry and one real entry still
			// passes -- the check only requires AT LEAST ONE real entry,
			// never that every entry be non-blank.
			name: "deep path with one blank entry alongside one real entry is accepted",
			mutate: func(in *reviewpost.VerdictInput) {
				in.Digest.ArchDecisions = []reviewpost.ArchDecision{{}, {Decision: "Introduced a new triage package"}}
			},
			wantErr: nil,
		},
		{
			name:    "deep path with empty StackRisks is rejected",
			mutate:  func(in *reviewpost.VerdictInput) { in.Digest.StackRisks = "" },
			wantErr: reviewpost.ErrEmptyDigestStackRisks,
		},
		{
			name:    "deep path with whitespace-only StackRisks is rejected",
			mutate:  func(in *reviewpost.VerdictInput) { in.Digest.StackRisks = "   " },
			wantErr: reviewpost.ErrEmptyDigestStackRisks,
		},
		{
			name:    "deep path with empty UnverifiedLimits is rejected",
			mutate:  func(in *reviewpost.VerdictInput) { in.Digest.UnverifiedLimits = "" },
			wantErr: reviewpost.ErrEmptyDigestUnverifiedLimits,
		},
		{
			name:    "deep path with missing counterReview (zero value) is rejected -- §26.4: schema-required on the deep path",
			mutate:  func(in *reviewpost.VerdictInput) { in.CounterReview = "" },
			wantErr: reviewpost.ErrInvalidCounterReview,
		},
		{
			name:    "deep path with garbled counterReview is rejected",
			mutate:  func(in *reviewpost.VerdictInput) { in.CounterReview = "maybe" },
			wantErr: reviewpost.ErrInvalidCounterReview,
		},
		{
			name:    "deep path with counterReview=skipped is a VALID payload (the skip itself only floors Shippable, it does not fail validation)",
			mutate:  func(in *reviewpost.VerdictInput) { in.CounterReview = review.CounterReviewSkipped },
			wantErr: nil,
		},
		{
			name:    "deep path with counterReview=uncorroborated is rejected -- only the server resolves that state, a payload cannot claim it",
			mutate:  func(in *reviewpost.VerdictInput) { in.CounterReview = review.CounterReviewUncorroborated },
			wantErr: reviewpost.ErrServerOnlyCounterReview,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			in := deepValidInput()
			tc.mutate(&in)

			err := reviewpost.ValidateVerdictInput(in)
			if tc.wantErr == nil {
				if err != nil {
					t.Errorf("ValidateVerdictInput() = %v, want nil", err)
				}
				return
			}
			if !errors.Is(err, tc.wantErr) {
				t.Errorf("ValidateVerdictInput() = %v, want %v", err, tc.wantErr)
			}
		})
	}
}

// TestValidateVerdictInput_LightPathNeverRequiresFullDigest pins the
// light-path invariant explicitly (§26.1: "the light path requests the
// full digest but does not hard-require it") -- an empty ArchDecisions/
// StackRisks/UnverifiedLimits on a light-path (or ReviewDepth=="",
// pre-existing) verdict must still validate cleanly. Mutating
// ValidateVerdictInput's own `in.ReviewDepth == reviewtriage.DepthDeep`
// guard into an unconditional check must fail this test.
func TestValidateVerdictInput_LightPathNeverRequiresFullDigest(t *testing.T) {
	in := validInput()
	in.ReviewDepth = reviewtriage.DepthLight
	// Digest.ArchDecisions/StackRisks/UnverifiedLimits are already empty
	// on validInput()'s own Digest -- left untouched here deliberately.

	if err := reviewpost.ValidateVerdictInput(in); err != nil {
		t.Fatalf("ValidateVerdictInput() on the light path with an empty ArchDecisions/StackRisks/UnverifiedLimits = %v, want nil", err)
	}
}

// TestValidateVerdictInput_UnresolvedDepthNeverRequiresFullDigest covers
// the ReviewDepth == "" case explicitly (a pre-existing caller, or a turn
// whose depth could not be resolved) -- must degrade like the light path,
// never like the deep path.
func TestValidateVerdictInput_UnresolvedDepthNeverRequiresFullDigest(t *testing.T) {
	in := validInput()
	in.ReviewDepth = ""

	if err := reviewpost.ValidateVerdictInput(in); err != nil {
		t.Fatalf("ValidateVerdictInput() with an unresolved ReviewDepth = %v, want nil", err)
	}
}

// TestValidateVerdictInput_DeepDigestChecksRunLast proves the three new
// deep-path checks run AFTER every pre-existing check (added at the end
// of the fixed order, per ValidateVerdictInput's own doc comment) -- a
// deep-path payload with BOTH an empty top-level Summary AND an empty
// deep-path digest must still report ErrEmptySummary first.
func TestValidateVerdictInput_DeepDigestChecksRunLast(t *testing.T) {
	in := deepValidInput()
	in.Summary = ""
	in.Digest.ArchDecisions = nil

	err := reviewpost.ValidateVerdictInput(in)
	if !errors.Is(err, reviewpost.ErrEmptySummary) {
		t.Errorf("ValidateVerdictInput() = %v, want %v (pre-existing checks run first)", err, reviewpost.ErrEmptySummary)
	}
}

// TestValidateVerdictInput_CounterReviewCheckedLastOfAll proves §26.4's
// own CounterReview check is appended at the very END of the
// deep-path-only block (this function's own "each added at the end of the
// existing fixed order" discipline) -- a deep-path payload with BOTH an
// empty UnverifiedLimits AND a garbled CounterReview must still report
// ErrEmptyDigestUnverifiedLimits first, never ErrInvalidCounterReview.
func TestValidateVerdictInput_CounterReviewCheckedLastOfAll(t *testing.T) {
	in := deepValidInput()
	in.Digest.UnverifiedLimits = ""
	in.CounterReview = "bogus"

	err := reviewpost.ValidateVerdictInput(in)
	if !errors.Is(err, reviewpost.ErrEmptyDigestUnverifiedLimits) {
		t.Errorf("ValidateVerdictInput() = %v, want %v (the three pre-existing deep-path digest checks run before CounterReview)", err, reviewpost.ErrEmptyDigestUnverifiedLimits)
	}
}

// TestValidateVerdictInput_FactCheckCheckedBeforeDeepPathBlock proves
// §26.6's own FactCheck check runs BEFORE the deep-path-only block
// (it is unconditional, so it must never be positioned inside a block that
// only ever runs on the deep path) -- a deep-path payload with BOTH a
// garbled FactCheck AND an empty ArchDecisions must report
// ErrInvalidFactCheck first, never ErrEmptyDigestArchDecisions.
func TestValidateVerdictInput_FactCheckCheckedBeforeDeepPathBlock(t *testing.T) {
	in := deepValidInput()
	in.FactCheck = "bogus"
	in.Digest.ArchDecisions = nil

	err := reviewpost.ValidateVerdictInput(in)
	if !errors.Is(err, reviewpost.ErrInvalidFactCheck) {
		t.Errorf("ValidateVerdictInput() = %v, want %v (the unconditional FactCheck check runs before the deep-path-only block)", err, reviewpost.ErrInvalidFactCheck)
	}
}

// TestValidateVerdictInput_FactCheckCheckedOnLightPathToo proves §26.6's
// own "schema-required UNCONDITIONALLY (both paths, not just deep)" --
// unlike CounterReview (which the light path never validates at all,
// TestValidateVerdictInput's own "counterReview is unchecked on the light
// path even when garbled" case), a light-path payload with a missing
// FactCheck must still be rejected.
func TestValidateVerdictInput_FactCheckCheckedOnLightPathToo(t *testing.T) {
	in := validInput()
	in.ReviewDepth = reviewtriage.DepthLight
	in.FactCheck = ""

	err := reviewpost.ValidateVerdictInput(in)
	if !errors.Is(err, reviewpost.ErrInvalidFactCheck) {
		t.Errorf("ValidateVerdictInput() = %v, want %v (FactCheck is required on the light path too)", err, reviewpost.ErrInvalidFactCheck)
	}
}
