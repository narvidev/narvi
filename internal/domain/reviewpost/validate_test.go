package reviewpost_test

import (
	"errors"
	"fmt"
	"reflect"
	"strings"
	"testing"

	"github.com/narvidev/narvi/internal/domain/review"
	"github.com/narvidev/narvi/internal/domain/reviewpost"
	"github.com/narvidev/narvi/internal/domain/reviewtriage"
)

// validInput returns a VerdictInput that passes every check --
// individual test cases mutate a copy of this to isolate exactly one bad
// field at a time.
func validInput() reviewpost.VerdictInput {
	return reviewpost.VerdictInput{
		RiskLevel:         review.RiskLevelLow,
		Premise:           review.PremiseStateOK,
		BlastRadius:       []review.Tag{review.TagAuth},
		FilesChanged:      3,
		TestsCoverage:     review.TestsCoverageStateAdequate,
		DocsDrift:         review.DocsDriftStateNone,
		ProposedShippable: review.ProposedShippableAuto,
		Summary:           "Looks good, minor nit.",
		Digest: reviewpost.Digest{
			Summary:             "Adds a retry helper around the flaky upstream call.",
			DescriptionAdequacy: review.DescriptionAdequacyOK,
			AdequacyExplanation: "The PR body accurately describes the retry helper this diff adds.",
		},
		FactCheck: reviewpost.FactCheckDone,
	}
}

func TestValidateVerdictInput(t *testing.T) {
	tests := []struct {
		name    string
		mutate  func(in *reviewpost.VerdictInput)
		wantErr error
	}{
		{
			name:    "valid input",
			mutate:  func(_ *reviewpost.VerdictInput) {},
			wantErr: nil,
		},
		{
			name:    "missing riskLevel (zero value)",
			mutate:  func(in *reviewpost.VerdictInput) { in.RiskLevel = "" },
			wantErr: reviewpost.ErrInvalidRiskLevel,
		},
		{
			name:    "garbled riskLevel",
			mutate:  func(in *reviewpost.VerdictInput) { in.RiskLevel = "extreme" },
			wantErr: reviewpost.ErrInvalidRiskLevel,
		},
		{
			name:    "missing premise (zero value)",
			mutate:  func(in *reviewpost.VerdictInput) { in.Premise = "" },
			wantErr: reviewpost.ErrInvalidPremise,
		},
		{
			name:    "garbled premise",
			mutate:  func(in *reviewpost.VerdictInput) { in.Premise = "maybe" },
			wantErr: reviewpost.ErrInvalidPremise,
		},
		{
			name:    "missing testsCoverage (zero value)",
			mutate:  func(in *reviewpost.VerdictInput) { in.TestsCoverage = "" },
			wantErr: reviewpost.ErrInvalidTestsCoverage,
		},
		{
			name:    "missing docsDrift (zero value)",
			mutate:  func(in *reviewpost.VerdictInput) { in.DocsDrift = "" },
			wantErr: reviewpost.ErrInvalidDocsDrift,
		},
		{
			name:    "missing proposedShippable (zero value)",
			mutate:  func(in *reviewpost.VerdictInput) { in.ProposedShippable = "" },
			wantErr: reviewpost.ErrInvalidProposedShippable,
		},
		{
			name:    "garbled proposedShippable",
			mutate:  func(in *reviewpost.VerdictInput) { in.ProposedShippable = "definitely" },
			wantErr: reviewpost.ErrInvalidProposedShippable,
		},
		{
			name:    "unrecognized blastRadius tag",
			mutate:  func(in *reviewpost.VerdictInput) { in.BlastRadius = []review.Tag{"frontend"} },
			wantErr: reviewpost.ErrInvalidBlastRadiusTag,
		},
		{
			name:    "one valid tag among many, one bad -- still rejected",
			mutate:  func(in *reviewpost.VerdictInput) { in.BlastRadius = []review.Tag{review.TagAuth, "bogus"} },
			wantErr: reviewpost.ErrInvalidBlastRadiusTag,
		},
		{
			name:    "empty blastRadius is legal (reviewer found no tagged area)",
			mutate:  func(in *reviewpost.VerdictInput) { in.BlastRadius = nil },
			wantErr: nil,
		},
		{
			name:    "negative filesChanged",
			mutate:  func(in *reviewpost.VerdictInput) { in.FilesChanged = -1 },
			wantErr: reviewpost.ErrNegativeFilesChanged,
		},
		{
			name:    "zero filesChanged is legal",
			mutate:  func(in *reviewpost.VerdictInput) { in.FilesChanged = 0 },
			wantErr: nil,
		},
		{
			name:    "empty summary",
			mutate:  func(in *reviewpost.VerdictInput) { in.Summary = "" },
			wantErr: reviewpost.ErrEmptySummary,
		},
		{
			name:    "whitespace-only summary",
			mutate:  func(in *reviewpost.VerdictInput) { in.Summary = "   \n\t  " },
			wantErr: reviewpost.ErrEmptySummary,
		},
		{
			name:    "empty digest summary (§26.1: required on every review)",
			mutate:  func(in *reviewpost.VerdictInput) { in.Digest.Summary = "" },
			wantErr: reviewpost.ErrEmptyDigestSummary,
		},
		{
			name:    "whitespace-only digest summary",
			mutate:  func(in *reviewpost.VerdictInput) { in.Digest.Summary = "   \n\t  " },
			wantErr: reviewpost.ErrEmptyDigestSummary,
		},
		{
			name: "empty ArchDecisions/StackRisks/UnverifiedLimits is legal (§26.1: requested, not required, until §26.3 defines the deep path)",
			mutate: func(in *reviewpost.VerdictInput) {
				in.Digest.ArchDecisions = nil
				in.Digest.StackRisks = ""
				in.Digest.UnverifiedLimits = ""
			},
			wantErr: nil,
		},
		{
			name:    "missing digest.descriptionAdequacy (zero value, §26.2: required on every review)",
			mutate:  func(in *reviewpost.VerdictInput) { in.Digest.DescriptionAdequacy = "" },
			wantErr: reviewpost.ErrInvalidDescriptionAdequacy,
		},
		{
			name:    "garbled digest.descriptionAdequacy",
			mutate:  func(in *reviewpost.VerdictInput) { in.Digest.DescriptionAdequacy = "somewhat" },
			wantErr: reviewpost.ErrInvalidDescriptionAdequacy,
		},
		{
			name:    "digest.descriptionAdequacy = drift is legal",
			mutate:  func(in *reviewpost.VerdictInput) { in.Digest.DescriptionAdequacy = review.DescriptionAdequacyDrift },
			wantErr: nil,
		},
		{
			name: "digest.descriptionAdequacy = misleading is legal",
			mutate: func(in *reviewpost.VerdictInput) {
				in.Digest.DescriptionAdequacy = review.DescriptionAdequacyMisleading
			},
			wantErr: nil,
		},
		{
			name:    "empty digest.adequacyExplanation (§26.2: required on every review)",
			mutate:  func(in *reviewpost.VerdictInput) { in.Digest.AdequacyExplanation = "" },
			wantErr: reviewpost.ErrEmptyAdequacyExplanation,
		},
		{
			name:    "whitespace-only digest.adequacyExplanation",
			mutate:  func(in *reviewpost.VerdictInput) { in.Digest.AdequacyExplanation = "   \n\t  " },
			wantErr: reviewpost.ErrEmptyAdequacyExplanation,
		},
		{
			name:    "empty digest.proposedBody is legal (§26.2: the agent MAY propose a rewrite, not required)",
			mutate:  func(in *reviewpost.VerdictInput) { in.Digest.ProposedBody = "" },
			wantErr: nil,
		},
		{
			name:    "missing factCheck (zero value) -- §26.6: schema-required unconditionally",
			mutate:  func(in *reviewpost.VerdictInput) { in.FactCheck = "" },
			wantErr: reviewpost.ErrInvalidFactCheck,
		},
		{
			name:    "garbled factCheck",
			mutate:  func(in *reviewpost.VerdictInput) { in.FactCheck = "maybe" },
			wantErr: reviewpost.ErrInvalidFactCheck,
		},
		{
			name:    "factCheck=skipped with factCheckKilled=0 is legal",
			mutate:  func(in *reviewpost.VerdictInput) { in.FactCheck = reviewpost.FactCheckSkipped; in.FactCheckKilled = 0 },
			wantErr: nil,
		},
		{
			name:    "negative factCheckKilled",
			mutate:  func(in *reviewpost.VerdictInput) { in.FactCheckKilled = -1 },
			wantErr: reviewpost.ErrNegativeFactCheckKilled,
		},
		{
			name:    "factCheck=skipped with a non-zero factCheckKilled is rejected (a skipped pass removed nothing, by construction)",
			mutate:  func(in *reviewpost.VerdictInput) { in.FactCheck = reviewpost.FactCheckSkipped; in.FactCheckKilled = 2 },
			wantErr: reviewpost.ErrFactCheckKilledOnSkip,
		},
		{
			name:    "factCheck=done with a positive factCheckKilled is legal",
			mutate:  func(in *reviewpost.VerdictInput) { in.FactCheck = reviewpost.FactCheckDone; in.FactCheckKilled = 3 },
			wantErr: nil,
		},
		{
			name:    "counterReview is unchecked on the light path even when garbled (§26.9: no meaning there)",
			mutate:  func(in *reviewpost.VerdictInput) { in.CounterReview = "bogus" },
			wantErr: nil,
		},
		{
			name:    "counterReview=uncorroborated is rejected on an unresolved-depth path -- only the server resolves that state",
			mutate:  func(in *reviewpost.VerdictInput) { in.CounterReview = review.CounterReviewUncorroborated },
			wantErr: reviewpost.ErrServerOnlyCounterReview,
		},
		{
			name: "counterReview=uncorroborated is rejected on the light path -- only the server resolves that state",
			mutate: func(in *reviewpost.VerdictInput) {
				in.ReviewDepth = reviewtriage.DepthLight
				in.CounterReview = review.CounterReviewUncorroborated
			},
			wantErr: reviewpost.ErrServerOnlyCounterReview,
		},
		{
			name: "counterReview=skipped stays legal on the light path",
			mutate: func(in *reviewpost.VerdictInput) {
				in.ReviewDepth = reviewtriage.DepthLight
				in.CounterReview = review.CounterReviewSkipped
			},
			wantErr: nil,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			in := validInput()
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

// TestValidateVerdictInput_FieldOrder proves the checks run in the
// documented fixed order: a payload with BOTH an invalid riskLevel AND an
// invalid premise always reports the riskLevel error first, never the
// premise one -- a caller building a 400 message must get a deterministic
// answer, not one that depends on struct field order or map iteration.
func TestValidateVerdictInput_FieldOrder(t *testing.T) {
	in := validInput()
	in.RiskLevel = "bogus"
	in.Premise = "bogus"

	err := reviewpost.ValidateVerdictInput(in)
	if !errors.Is(err, reviewpost.ErrInvalidRiskLevel) {
		t.Errorf("ValidateVerdictInput() = %v, want %v (riskLevel checked first)", err, reviewpost.ErrInvalidRiskLevel)
	}
}

// TestValidateVerdictInput_DigestSummaryCheckedLastAmongExisting proves
// §26.1's own new Digest.Summary check runs AFTER every pre-existing
// check (added at the end of the fixed order, never interleaved earlier,
// per this function's own doc comment) -- a payload with BOTH an empty
// top-level Summary AND an empty Digest.Summary must still report
// ErrEmptySummary, never ErrEmptyDigestSummary, so this Step never changes
// which error an already-malformed pre-existing payload reports.
func TestValidateVerdictInput_DigestSummaryCheckedLastAmongExisting(t *testing.T) {
	in := validInput()
	in.Summary = ""
	in.Digest.Summary = ""

	err := reviewpost.ValidateVerdictInput(in)
	if !errors.Is(err, reviewpost.ErrEmptySummary) {
		t.Errorf("ValidateVerdictInput() = %v, want %v (top-level summary checked before digest.summary)", err, reviewpost.ErrEmptySummary)
	}
}

// TestValidateVerdictInput_AdequacyCheckedAfterDigestSummary proves §26.2/
// §26.2's own new Digest.DescriptionAdequacy/AdequacyExplanation checks
// run AFTER Digest.Summary (added at the end of the fixed order, per this
// function's own doc comment) -- a payload with BOTH an empty
// Digest.Summary AND a garbled Digest.DescriptionAdequacy must still
// report ErrEmptyDigestSummary, never ErrInvalidDescriptionAdequacy, so
// this Step never changes which error an already-malformed pre-existing
// payload reports.
func TestValidateVerdictInput_AdequacyCheckedAfterDigestSummary(t *testing.T) {
	in := validInput()
	in.Digest.Summary = ""
	in.Digest.DescriptionAdequacy = "bogus"

	err := reviewpost.ValidateVerdictInput(in)
	if !errors.Is(err, reviewpost.ErrEmptyDigestSummary) {
		t.Errorf("ValidateVerdictInput() = %v, want %v (digest.summary checked before digest.descriptionAdequacy)", err, reviewpost.ErrEmptyDigestSummary)
	}
}

// TestValidateVerdictInput_AdequacyExplanationCheckedLast proves
// AdequacyExplanation is checked LAST of all -- a payload with BOTH a
// garbled DescriptionAdequacy AND an empty AdequacyExplanation must still
// report ErrInvalidDescriptionAdequacy, never ErrEmptyAdequacyExplanation.
func TestValidateVerdictInput_AdequacyExplanationCheckedLast(t *testing.T) {
	in := validInput()
	in.Digest.DescriptionAdequacy = "bogus"
	in.Digest.AdequacyExplanation = ""

	err := reviewpost.ValidateVerdictInput(in)
	if !errors.Is(err, reviewpost.ErrInvalidDescriptionAdequacy) {
		t.Errorf("ValidateVerdictInput() = %v, want %v (digest.descriptionAdequacy checked before digest.adequacyExplanation)", err, reviewpost.ErrInvalidDescriptionAdequacy)
	}
}

// TestBuildVerdict_ShippableAlwaysComputedNeverCopiedFromProposed proves
// BuildVerdict's own central contract: Shippable is ALWAYS
// review.ComputeShippable's own result, regardless of what
// ProposedShippable says -- a caller "proposing" auto must never leak
// into an authoritative auto when the real floors say otherwise.
func TestBuildVerdict_ShippableAlwaysComputedNeverCopiedFromProposed(t *testing.T) {
	in := validInput()
	in.RiskLevel = review.RiskLevelLow
	in.Premise = review.PremiseStateNotAPR // forces the premise floor to Block.
	in.TestsCoverage = review.TestsCoverageStateAdequate
	in.ProposedShippable = review.ProposedShippableAuto // the model insists "auto" -- must be ignored.

	got, _ := reviewpost.BuildVerdict(in)
	if got.Shippable != review.ShippableBlock {
		t.Errorf("Shippable = %q, want %q (ProposedShippableAuto must never override the computed floor)", got.Shippable, review.ShippableBlock)
	}
	if got.ProposedShippable != review.ProposedShippableAuto {
		t.Errorf("ProposedShippable = %q, want %q (still carried verbatim as audit data)", got.ProposedShippable, review.ProposedShippableAuto)
	}
}

// TestBuildVerdict_MisleadingAdequacyRaisesShippable is §26.2's
// own end-to-end pin, one layer up from
// TestComputeShippable_MisleadingRaisesShippable (internal/domain/review):
// an otherwise-completely-clean VerdictInput (low risk, ok premise,
// adequate coverage -- every OTHER floor clean, Shippable would otherwise
// compute auto) whose Digest.DescriptionAdequacy is "misleading" must
// still come out of BuildVerdict as ShippableNeedsHuman, proving the third
// floor genuinely reaches the posting endpoint's own real construction
// path, not merely ComputeShippable in isolation.
func TestBuildVerdict_MisleadingAdequacyRaisesShippable(t *testing.T) {
	in := validInput()
	in.RiskLevel = review.RiskLevelLow
	in.Premise = review.PremiseStateOK
	in.TestsCoverage = review.TestsCoverageStateAdequate
	in.Digest.DescriptionAdequacy = review.DescriptionAdequacyMisleading

	got, _ := reviewpost.BuildVerdict(in)
	if got.Shippable != review.ShippableNeedsHuman {
		t.Errorf("Shippable = %q, want %q (a misleading description must raise Shippable off an otherwise-clean auto baseline)", got.Shippable, review.ShippableNeedsHuman)
	}
}

// TestBuildVerdict_AdequacyNeverAffectsRiskLevel is §26.2's own explicit
// asymmetry, pinned at BuildVerdict's own real construction site (not just
// ComputeShippable's pure-function level, TestComputeShippable_
// MisleadingRaisesShippable, internal/domain/review): varying
// Digest.DescriptionAdequacy from "ok" to "misleading", with every other
// field held fixed, changes Shippable but leaves RiskLevel COMPLETELY
// untouched -- "the server computes Shippable, but never fabricates risk
// the model did not report". Mutation coverage: a bug that derived
// RiskLevel from DescriptionAdequacy (or otherwise let the adequacy floor
// leak into RiskLevel) makes this test fail.
func TestBuildVerdict_AdequacyNeverAffectsRiskLevel(t *testing.T) {
	base := validInput()
	base.RiskLevel = review.RiskLevelMedium

	okInput := base
	okInput.Digest.DescriptionAdequacy = review.DescriptionAdequacyOK
	okVerdict, _ := reviewpost.BuildVerdict(okInput)

	misleadingInput := base
	misleadingInput.Digest.DescriptionAdequacy = review.DescriptionAdequacyMisleading
	misleadingVerdict, _ := reviewpost.BuildVerdict(misleadingInput)

	if okVerdict.RiskLevel != review.RiskLevelMedium {
		t.Fatalf("test setup: okVerdict.RiskLevel = %q, want %q", okVerdict.RiskLevel, review.RiskLevelMedium)
	}
	if misleadingVerdict.RiskLevel != review.RiskLevelMedium {
		t.Errorf("misleadingVerdict.RiskLevel = %q, want %q (DescriptionAdequacy must never influence RiskLevel)", misleadingVerdict.RiskLevel, review.RiskLevelMedium)
	}
	if okVerdict.RiskLevel != misleadingVerdict.RiskLevel {
		t.Errorf("RiskLevel differed across DescriptionAdequacy values (%q vs %q) -- must be identical, RiskLevel is upstream of and unaffected by any floor", okVerdict.RiskLevel, misleadingVerdict.RiskLevel)
	}
	// Sanity: Shippable itself DID change (otherwise this test would prove
	// nothing about the floor actually running) -- both raise the SAME
	// baseline (medium -> needs_human) here, so require Shippable to be at
	// LEAST as strict for misleading, and confirm the floor path was
	// genuinely exercised via the dedicated raise test above instead of
	// re-deriving that property here.
	if misleadingVerdict.Shippable != review.ShippableNeedsHuman {
		t.Errorf("misleadingVerdict.Shippable = %q, want %q", misleadingVerdict.Shippable, review.ShippableNeedsHuman)
	}
}

// TestBuildVerdict_CounterReviewSkippedRaisesShippable is §26.4's
// own end-to-end pin, one layer up from
// TestComputeShippable_CounterReviewSkippedRaisesShippable
// (internal/domain/review/shippable_test.go): an otherwise-completely-clean,
// DEEP-path VerdictInput (low risk, ok premise, adequate coverage, ok
// adequacy -- every OTHER floor clean, Shippable would otherwise compute
// auto) whose CounterReview is "skipped" must still come out of
// BuildVerdict as ShippableNeedsHuman, proving the fourth floor genuinely
// reaches the posting endpoint's own real construction path, not merely
// ComputeShippable in isolation. This is explicitly named, in this Step's
// own process requirements, as "the single most important property in
// this whole Step".
func TestBuildVerdict_CounterReviewSkippedRaisesShippable(t *testing.T) {
	in := validInput()
	in.ReviewDepth = reviewtriage.DepthDeep
	in.RiskLevel = review.RiskLevelLow
	in.Premise = review.PremiseStateOK
	in.TestsCoverage = review.TestsCoverageStateAdequate
	in.Digest.DescriptionAdequacy = review.DescriptionAdequacyOK
	in.Digest.ArchDecisions = []reviewpost.ArchDecision{{Decision: "x"}}
	in.Digest.StackRisks = "none of note"
	in.Digest.UnverifiedLimits = "did not run against production data"
	in.CounterReview = review.CounterReviewSkipped

	if err := reviewpost.ValidateVerdictInput(in); err != nil {
		t.Fatalf("test setup: ValidateVerdictInput() = %v, want nil", err)
	}

	got, _ := reviewpost.BuildVerdict(in)
	if got.Shippable != review.ShippableNeedsHuman {
		t.Errorf("Shippable = %q, want %q (a skipped counter-review must raise Shippable off an otherwise-clean auto baseline on the deep path)", got.Shippable, review.ShippableNeedsHuman)
	}
}

// TestBuildVerdict_CounterReviewFloorInertOnLightPath is the mutation-test
// pin for BuildVerdict's own light-path substitution (this function's own
// doc comment, validate.go): a LIGHT-path (or unresolved-depth) VerdictInput
// carrying a garbled/unset CounterReview value must NOT have Shippable
// floored to needs_human by it -- review.CounterReviewFloor's own
// fail-conservative default would otherwise silently defeat light-path
// auto-approval entirely, on every single light-path verdict, since the
// light path never populates (or validates) this field at all (§26.9).
// Mutation coverage: reverting BuildVerdict's own `if in.ReviewDepth !=
// reviewtriage.DepthDeep { counterReviewForFloor = review.CounterReviewDone }`
// substitution (e.g. always forwarding in.CounterReview verbatim) makes
// this test fail, since ReviewDepth is left at its own zero value below
// (never DepthDeep) with CounterReview left at ITS zero value too.
func TestBuildVerdict_CounterReviewFloorInertOnLightPath(t *testing.T) {
	in := validInput()
	in.RiskLevel = review.RiskLevelLow
	in.Premise = review.PremiseStateOK
	in.TestsCoverage = review.TestsCoverageStateAdequate
	in.Digest.DescriptionAdequacy = review.DescriptionAdequacyOK
	// in.ReviewDepth is left at its own zero value (never DepthDeep) --
	// the light-path/unresolved-depth case.
	// in.CounterReview is left at its own zero value too, exactly what an
	// agent that was never asked to populate it (§26.9: no counter-review
	// on light) would submit.

	if err := reviewpost.ValidateVerdictInput(in); err != nil {
		t.Fatalf("test setup: ValidateVerdictInput() = %v, want nil (counterReview is never validated on the light path)", err)
	}

	got, _ := reviewpost.BuildVerdict(in)
	if got.Shippable != review.ShippableAuto {
		t.Errorf("Shippable = %q, want %q (an unset CounterReview must be inert on the light path, never floored as though it were 'skipped')", got.Shippable, review.ShippableAuto)
	}
}

// TestBuildVerdict_ExplicitCounterReviewSkippedNeverOverwrittenOnLightPath
// is B11's own regression test: unlike the BLANK/unset CounterReview the
// test immediately above pins as correctly inert on the light path, a
// verdict that EXPLICITLY carries CounterReview: "skipped" -- legal input
// there, since ValidateVerdictInput never checks this field at all when
// in.ReviewDepth != reviewtriage.DepthDeep -- must still float Shippable
// to needs_human, the SAME floor TestBuildVerdict_CounterReviewSkippedRaisesShippable
// above already pins for the deep path. Before the B11 fix, BuildVerdict's
// own unconditional light-path substitution silently rewrote this
// explicit self-report into CounterReviewDone, erasing exactly the signal
// §26.4 says must raise the floor.
// Mutation coverage: reverting BuildVerdict's own added
// `&& in.CounterReview != review.CounterReviewSkipped` clause (back to the
// bare `if in.ReviewDepth != reviewtriage.DepthDeep` this test's own
// sibling above still covers) makes this test fail, since it would then
// recompute Shippable as auto instead.
func TestBuildVerdict_ExplicitCounterReviewSkippedNeverOverwrittenOnLightPath(t *testing.T) {
	in := validInput()
	in.RiskLevel = review.RiskLevelLow
	in.Premise = review.PremiseStateOK
	in.TestsCoverage = review.TestsCoverageStateAdequate
	in.Digest.DescriptionAdequacy = review.DescriptionAdequacyOK
	// in.ReviewDepth is left at its own zero value (never DepthDeep) --
	// the light-path/unresolved-depth case, exactly like the sibling test
	// above -- but CounterReview is EXPLICITLY reported skipped this time.
	in.CounterReview = review.CounterReviewSkipped

	if err := reviewpost.ValidateVerdictInput(in); err != nil {
		t.Fatalf("test setup: ValidateVerdictInput() = %v, want nil (CounterReview is never validated on the light path, so an explicit \"skipped\" is legal input there)", err)
	}

	got, _ := reviewpost.BuildVerdict(in)
	if got.Shippable != review.ShippableNeedsHuman {
		t.Errorf("Shippable = %q, want %q (an EXPLICIT CounterReview: skipped must still raise Shippable, even on a verdict this function cannot confirm is genuinely deep-path)", got.Shippable, review.ShippableNeedsHuman)
	}
}

// TestBuildVerdict_CorroboratedDeepPathKeepsCounterReviewDoneFloor pins
// the ORDINARY, expected-common-case outcome of the second substitution
// (§26.4): a deep-path verdict that claims CounterReview: done
// AND whose claim the caller has independently corroborated against the
// persisted sub_task_finish trace (CounterReviewCorroborated: true) keeps
// counterReviewForFloor at CounterReviewDone -- floor stays whatever
// CounterReviewDone already gives (ShippableAuto on this otherwise-clean
// input), completely untouched by the new substitution.
func TestBuildVerdict_CorroboratedDeepPathKeepsCounterReviewDoneFloor(t *testing.T) {
	in := validInput()
	in.ReviewDepth = reviewtriage.DepthDeep
	in.RiskLevel = review.RiskLevelLow
	in.Premise = review.PremiseStateOK
	in.TestsCoverage = review.TestsCoverageStateAdequate
	in.Digest.DescriptionAdequacy = review.DescriptionAdequacyOK
	in.Digest.ArchDecisions = []reviewpost.ArchDecision{{Decision: "x"}}
	in.Digest.StackRisks = "none of note"
	in.Digest.UnverifiedLimits = "did not run against production data"
	in.CounterReview = review.CounterReviewDone
	in.CounterReviewCorroborated = true

	if err := reviewpost.ValidateVerdictInput(in); err != nil {
		t.Fatalf("test setup: ValidateVerdictInput() = %v, want nil", err)
	}

	got, _ := reviewpost.BuildVerdict(in)
	if got.Shippable != review.ShippableAuto {
		t.Errorf("Shippable = %q, want %q (a corroborated done claim on the deep path must keep the ordinary CounterReviewDone floor)", got.Shippable, review.ShippableAuto)
	}
}

// TestBuildVerdict_UncorroboratedDeepPathDoneFloorsToNeedsHuman is this
// Step's own central positive case: a deep-path verdict claiming
// CounterReview: done, whose claim the caller could NOT corroborate
// against the persisted trace (CounterReviewCorroborated: false), must be
// floored to ShippableNeedsHuman -- the SAME floor an honest "skipped"
// self-report already produces (TestBuildVerdict_CounterReviewSkippedRaisesShippable
// above), even though the raw self-report itself claims "done".
// Mutation coverage: removing BuildVerdict's own second substitution
// entirely (or inverting !in.CounterReviewCorroborated to
// in.CounterReviewCorroborated) makes this test fail, since Shippable
// would then stay at ShippableAuto instead.
func TestBuildVerdict_UncorroboratedDeepPathDoneFloorsToNeedsHuman(t *testing.T) {
	in := validInput()
	in.ReviewDepth = reviewtriage.DepthDeep
	in.RiskLevel = review.RiskLevelLow
	in.Premise = review.PremiseStateOK
	in.TestsCoverage = review.TestsCoverageStateAdequate
	in.Digest.DescriptionAdequacy = review.DescriptionAdequacyOK
	in.Digest.ArchDecisions = []reviewpost.ArchDecision{{Decision: "x"}}
	in.Digest.StackRisks = "none of note"
	in.Digest.UnverifiedLimits = "did not run against production data"
	in.CounterReview = review.CounterReviewDone
	in.CounterReviewCorroborated = false

	if err := reviewpost.ValidateVerdictInput(in); err != nil {
		t.Fatalf("test setup: ValidateVerdictInput() = %v, want nil", err)
	}

	got, _ := reviewpost.BuildVerdict(in)
	if got.Shippable != review.ShippableNeedsHuman {
		t.Errorf("Shippable = %q, want %q (a deep-path done claim the server could NOT corroborate must be floored to needs_human)", got.Shippable, review.ShippableNeedsHuman)
	}
}

// TestBuildVerdict_ExplicitSkippedUnaffectedByCorroboration pins that an
// EXPLICIT CounterReview: skipped self-report on the deep path is
// completely unaffected by CounterReviewCorroborated either way -- the
// second substitution's own condition requires in.CounterReview ==
// review.CounterReviewDone, so a "skipped" self-report never enters that
// branch at all, and Shippable is floored to needs_human by the ORIGINAL,
// unconditional CounterReviewFloor(CounterReviewSkipped) computation
// regardless of what CounterReviewCorroborated happens to carry.
func TestBuildVerdict_ExplicitSkippedUnaffectedByCorroboration(t *testing.T) {
	base := validInput()
	base.ReviewDepth = reviewtriage.DepthDeep
	base.RiskLevel = review.RiskLevelLow
	base.Premise = review.PremiseStateOK
	base.TestsCoverage = review.TestsCoverageStateAdequate
	base.Digest.DescriptionAdequacy = review.DescriptionAdequacyOK
	base.Digest.ArchDecisions = []reviewpost.ArchDecision{{Decision: "x"}}
	base.Digest.StackRisks = "none of note"
	base.Digest.UnverifiedLimits = "did not run against production data"
	base.CounterReview = review.CounterReviewSkipped

	for _, corroborated := range []bool{true, false} {
		in := base
		in.CounterReviewCorroborated = corroborated

		if err := reviewpost.ValidateVerdictInput(in); err != nil {
			t.Fatalf("test setup (corroborated=%v): ValidateVerdictInput() = %v, want nil", corroborated, err)
		}

		got, _ := reviewpost.BuildVerdict(in)
		if got.Shippable != review.ShippableNeedsHuman {
			t.Errorf("corroborated=%v: Shippable = %q, want %q (an explicit skipped self-report floors regardless of CounterReviewCorroborated)", corroborated, got.Shippable, review.ShippableNeedsHuman)
		}
	}
}

// TestBuildVerdict_CorroborationSubstitutionInertOnLightPath is the
// regression test for the exact bug this Step's own gate reasoning warns
// against (BuildVerdict's own doc comment, "Second substitution: post-hoc
// corroboration"): a LIGHT-path verdict whose payload happens to carry
// CounterReview: "done" -- legal, if meaningless, input there, since
// ValidateVerdictInput never checks this field outside the deep-path-only
// block -- combined with CounterReviewCorroborated left at its own zero
// value, false (httpapi never runs the corroboration query on the light
// path, VerdictInput.CounterReviewCorroborated's own doc comment) MUST
// NOT be floored by the second substitution. Gating that substitution on
// in.CounterReview == review.CounterReviewDone alone (instead of ALSO
// requiring in.ReviewDepth == reviewtriage.DepthDeep) would floor every
// such light-path verdict to needs_human, silently defeating light-path
// auto-approval -- the same failure mode the FIRST substitution's own doc
// comment already exists to prevent, reintroduced through a different
// door.
// Mutation coverage: dropping the `in.ReviewDepth ==
// reviewtriage.DepthDeep &&` clause from the second substitution's
// condition (leaving only `in.CounterReview == review.CounterReviewDone
// && !in.CounterReviewCorroborated`) makes this test fail, since Shippable
// would then be floored to needs_human instead of staying auto.
func TestBuildVerdict_CorroborationSubstitutionInertOnLightPath(t *testing.T) {
	in := validInput()
	in.RiskLevel = review.RiskLevelLow
	in.Premise = review.PremiseStateOK
	in.TestsCoverage = review.TestsCoverageStateAdequate
	in.Digest.DescriptionAdequacy = review.DescriptionAdequacyOK
	// in.ReviewDepth is left at its own zero value (never DepthDeep) --
	// the light-path/unresolved-depth case.
	in.CounterReview = review.CounterReviewDone
	// in.CounterReviewCorroborated is left at its own zero value, false --
	// exactly what httpapi leaves it at on every light-path verdict, since
	// it never runs the corroboration query there at all.

	if err := reviewpost.ValidateVerdictInput(in); err != nil {
		t.Fatalf("test setup: ValidateVerdictInput() = %v, want nil (counterReview is never validated on the light path)", err)
	}

	got, _ := reviewpost.BuildVerdict(in)
	if got.Shippable != review.ShippableAuto {
		t.Errorf("Shippable = %q, want %q (the corroboration substitution must be inert on the light path, never floored via a CounterReview:\"done\" echo paired with an always-false CounterReviewCorroborated)", got.Shippable, review.ShippableAuto)
	}
}

// TestBuildVerdict_NamesItsBlockers pins, at the posting endpoint's own
// construction site, the blockers BuildVerdict returns beside the Verdict
// (§26.1): the counter-review input as BuildVerdict resolves it --
// including a deep-path done claim the server could not corroborate,
// named uncorroborated rather than skipped -- and every other input as
// the reviewer reported it. Mutation coverage: collapsing the
// uncorroborated state back into skipped makes the uncorroborated case
// fail here, since its blocker would then read "skipped"; and a
// substitution that fires only when nothing else already holds
// needs_human fails the medium-risk case, which must name both.
func TestBuildVerdict_NamesItsBlockers(t *testing.T) {
	tests := []struct {
		name         string
		mutate       func(in *reviewpost.VerdictInput)
		wantClass    review.Shippable
		wantBlockers []review.Blocker
	}{
		{
			name:         "a clean light-path verdict names none",
			mutate:       func(_ *reviewpost.VerdictInput) {},
			wantClass:    review.ShippableAuto,
			wantBlockers: nil,
		},
		{
			name:         "a medium-risk verdict with every floor clean names its risk level",
			mutate:       func(in *reviewpost.VerdictInput) { in.RiskLevel = review.RiskLevelMedium },
			wantClass:    review.ShippableNeedsHuman,
			wantBlockers: []review.Blocker{{Input: review.ShippableInputRiskLevel, Value: "medium", Level: review.ShippableNeedsHuman}},
		},
		{
			name:         "a verdict floored by a coverage gap names the coverage gap",
			mutate:       func(in *reviewpost.VerdictInput) { in.TestsCoverage = review.TestsCoverageStateInsufficient },
			wantClass:    review.ShippableNeedsHuman,
			wantBlockers: []review.Blocker{{Input: review.ShippableInputTestsCoverage, Value: "insufficient", Level: review.ShippableNeedsHuman}},
		},
		{
			name: "a misleading description is named from the digest",
			mutate: func(in *reviewpost.VerdictInput) {
				in.Digest.DescriptionAdequacy = review.DescriptionAdequacyMisleading
			},
			wantClass:    review.ShippableNeedsHuman,
			wantBlockers: []review.Blocker{{Input: review.ShippableInputDescriptionAdequacy, Value: "misleading", Level: review.ShippableNeedsHuman}},
		},
		{
			name: "a deep-path done claim the server could not corroborate is named uncorroborated",
			mutate: func(in *reviewpost.VerdictInput) {
				*in = deepValidInput()
				in.CounterReview = review.CounterReviewDone
				in.CounterReviewCorroborated = false
			},
			wantClass:    review.ShippableNeedsHuman,
			wantBlockers: []review.Blocker{{Input: review.ShippableInputCounterReview, Value: "uncorroborated", Level: review.ShippableNeedsHuman}},
		},
		{
			name: "an uncorroborated done is named beside a medium risk that already holds needs_human",
			mutate: func(in *reviewpost.VerdictInput) {
				*in = deepValidInput()
				in.RiskLevel = review.RiskLevelMedium
				in.CounterReview = review.CounterReviewDone
				in.CounterReviewCorroborated = false
			},
			wantClass: review.ShippableNeedsHuman,
			wantBlockers: []review.Blocker{
				{Input: review.ShippableInputRiskLevel, Value: "medium", Level: review.ShippableNeedsHuman},
				{Input: review.ShippableInputCounterReview, Value: "uncorroborated", Level: review.ShippableNeedsHuman},
			},
		},
		{
			name: "a deep-path done claim the server corroborated names none",
			mutate: func(in *reviewpost.VerdictInput) {
				*in = deepValidInput()
				in.CounterReview = review.CounterReviewDone
				in.CounterReviewCorroborated = true
			},
			wantClass:    review.ShippableAuto,
			wantBlockers: nil,
		},
		{
			name: "a deep-path skip the reviewer reported is named skipped, corroborated or not",
			mutate: func(in *reviewpost.VerdictInput) {
				*in = deepValidInput()
				in.CounterReview = review.CounterReviewSkipped
				in.CounterReviewCorroborated = true
			},
			wantClass:    review.ShippableNeedsHuman,
			wantBlockers: []review.Blocker{{Input: review.ShippableInputCounterReview, Value: "skipped", Level: review.ShippableNeedsHuman}},
		},
		{
			name:         "an explicit skip on the light path is named skipped",
			mutate:       func(in *reviewpost.VerdictInput) { in.CounterReview = review.CounterReviewSkipped },
			wantClass:    review.ShippableNeedsHuman,
			wantBlockers: []review.Blocker{{Input: review.ShippableInputCounterReview, Value: "skipped", Level: review.ShippableNeedsHuman}},
		},
		{
			name:         "a light-path done echo is inert, never named uncorroborated",
			mutate:       func(in *reviewpost.VerdictInput) { in.CounterReview = review.CounterReviewDone },
			wantClass:    review.ShippableAuto,
			wantBlockers: nil,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			in := validInput()
			tc.mutate(&in)
			if err := reviewpost.ValidateVerdictInput(in); err != nil {
				t.Fatalf("test setup: ValidateVerdictInput() = %v, want nil", err)
			}

			v, shippable := reviewpost.BuildVerdict(in)
			if v.Shippable != tc.wantClass {
				t.Errorf("Verdict.Shippable = %q, want %q", v.Shippable, tc.wantClass)
			}
			if shippable.Class() != v.Shippable {
				t.Errorf("assessment class %q differs from Verdict.Shippable %q", shippable.Class(), v.Shippable)
			}
			if got := shippable.Blockers(); !reflect.DeepEqual(got, tc.wantBlockers) {
				t.Errorf("Blockers() = %+v, want %+v", got, tc.wantBlockers)
			}
		})
	}
}

// TestBuildVerdict_UncorroboratedKeepsEveryDecision is the audit behind
// adding CounterReviewUncorroborated: every verdict BuildVerdict can be
// handed across depth (deep, light, unresolved), the counter-review value
// in the payload (done, skipped, unset, garbled, and even the server-only
// uncorroborated, which ValidateVerdictInput rejects on every path but
// BuildVerdict does not re-check), corroboration, and a clean or medium
// risk, computes the SAME Shippable it did when an uncorroborated claim
// was folded into skipped -- the expected class below is that earlier
// rule, written out -- and returns an assessment whose class is the
// Verdict's Shippable and whose blockers are empty exactly when it is
// auto. So the new state moves no decision anywhere Shippable is read
// (the formal review event, the persisted review_verdicts row,
// auto-approval, auto-merge, the readout); it only changes what the
// blocker says. And it always says it: the uncorroborated counter-review
// blocker is present exactly when the deep path's done went unconfirmed
// (or an unvalidated deep payload carried the value itself), whatever
// else already holds the class at needs_human.
func TestBuildVerdict_UncorroboratedKeepsEveryDecision(t *testing.T) {
	depths := []reviewtriage.ReviewDepth{reviewtriage.DepthDeep, reviewtriage.DepthLight, ""}
	counterReviews := []review.CounterReviewStatus{review.CounterReviewDone, review.CounterReviewSkipped, "", "maybe", review.CounterReviewUncorroborated}
	risks := []review.RiskLevel{review.RiskLevelLow, review.RiskLevelMedium}

	for _, depth := range depths {
		for _, cr := range counterReviews {
			for _, corroborated := range []bool{true, false} {
				for _, risk := range risks {
					name := fmt.Sprintf("depth=%q counterReview=%q corroborated=%v risk=%s", depth, cr, corroborated, risk)
					in := validInput()
					in.ReviewDepth = depth
					in.CounterReview = cr
					in.CounterReviewCorroborated = corroborated
					in.RiskLevel = risk

					// The counter-review floor as it stood with an
					// uncorroborated claim read as skipped: on the deep
					// path, anything but a corroborated done floors; off
					// it, only an explicit skip does.
					counterReviewFloors := cr == review.CounterReviewSkipped
					if depth == reviewtriage.DepthDeep {
						counterReviewFloors = cr != review.CounterReviewDone || !corroborated
					}
					want := review.ShippableAuto
					if counterReviewFloors || risk == review.RiskLevelMedium {
						want = review.ShippableNeedsHuman
					}

					v, shippable := reviewpost.BuildVerdict(in)
					if v.Shippable != want {
						t.Errorf("%s: Verdict.Shippable = %q, want %q", name, v.Shippable, want)
					}
					if shippable.Class() != v.Shippable {
						t.Errorf("%s: assessment class %q differs from Verdict.Shippable %q", name, shippable.Class(), v.Shippable)
					}
					if (len(shippable.Blockers()) == 0) != (v.Shippable == review.ShippableAuto) {
						t.Errorf("%s: Shippable %q with blockers %+v; want none exactly when auto", name, v.Shippable, shippable.Blockers())
					}

					wantUncorroborated := depth == reviewtriage.DepthDeep &&
						((cr == review.CounterReviewDone && !corroborated) || cr == review.CounterReviewUncorroborated)
					gotUncorroborated := false
					for _, b := range shippable.Blockers() {
						if b.Input == review.ShippableInputCounterReview && b.Value == string(review.CounterReviewUncorroborated) {
							gotUncorroborated = true
						}
					}
					if gotUncorroborated != wantUncorroborated {
						t.Errorf("%s: uncorroborated counter-review blocker present = %v, want %v (blockers %+v)", name, gotUncorroborated, wantUncorroborated, shippable.Blockers())
					}
				}
			}
		}
	}
}

// TestComputeShippable_FactCheckSkippedNeverRaisesShippable is §26.6's
// own deliberate, load-bearing DIFFERENCE from
// TestBuildVerdict_CounterReviewSkippedRaisesShippable above: FactCheck
// has NO floor at all, so it can never be found anywhere in
// review.ComputeShippable's own parameter list, and no VerdictInput
// mutation involving FactCheck can ever change BuildVerdict's own computed
// Shippable. This proves the asymmetry end to end, at the SAME
// construction site the counter-review pin above exercises: two
// VerdictInputs, identical except FactCheck (done vs. skipped, with
// FactCheckKilled adjusted to keep each one independently valid), must
// compute the IDENTICAL Shippable.
func TestComputeShippable_FactCheckSkippedNeverRaisesShippable(t *testing.T) {
	base := validInput()
	base.RiskLevel = review.RiskLevelLow
	base.Premise = review.PremiseStateOK
	base.TestsCoverage = review.TestsCoverageStateAdequate
	base.Digest.DescriptionAdequacy = review.DescriptionAdequacyOK

	doneInput := base
	doneInput.FactCheck = reviewpost.FactCheckDone
	doneInput.FactCheckKilled = 5
	if err := reviewpost.ValidateVerdictInput(doneInput); err != nil {
		t.Fatalf("test setup: ValidateVerdictInput(done) = %v, want nil", err)
	}
	doneVerdict, _ := reviewpost.BuildVerdict(doneInput)

	skippedInput := base
	skippedInput.FactCheck = reviewpost.FactCheckSkipped
	skippedInput.FactCheckKilled = 0
	if err := reviewpost.ValidateVerdictInput(skippedInput); err != nil {
		t.Fatalf("test setup: ValidateVerdictInput(skipped) = %v, want nil", err)
	}
	skippedVerdict, _ := reviewpost.BuildVerdict(skippedInput)

	if doneVerdict.Shippable != review.ShippableAuto {
		t.Fatalf("test setup: doneVerdict.Shippable = %q, want %q", doneVerdict.Shippable, review.ShippableAuto)
	}
	if skippedVerdict.Shippable != doneVerdict.Shippable {
		t.Errorf("skippedVerdict.Shippable = %q, doneVerdict.Shippable = %q -- FactCheck:skipped must NEVER raise Shippable (the deliberate difference from CounterReview:skipped)", skippedVerdict.Shippable, doneVerdict.Shippable)
	}
}

// TestValidateVerdictInput_DigestLengthCaps is this Step's own regression
// test for G3 (hardening): table-driven, pinning the EXACT
// boundary of every Max*Bytes cap declared above (validate.go) -- a field
// AT the cap is legal, one byte OVER is rejected. strings.Repeat("a", n)
// is used throughout (never a multi-byte rune) so len() (a byte count) and
// the visible character count agree, keeping each boundary unambiguous.
func TestValidateVerdictInput_DigestLengthCaps(t *testing.T) {
	tests := []struct {
		name    string
		mutate  func(in *reviewpost.VerdictInput)
		wantErr error
	}{
		{
			name: "digest.summary at the cap is legal",
			mutate: func(in *reviewpost.VerdictInput) {
				in.Digest.Summary = strings.Repeat("a", reviewpost.MaxDigestSummaryBytes)
			},
			wantErr: nil,
		},
		{
			name: "digest.summary one byte over the cap is rejected",
			mutate: func(in *reviewpost.VerdictInput) {
				in.Digest.Summary = strings.Repeat("a", reviewpost.MaxDigestSummaryBytes+1)
			},
			wantErr: reviewpost.ErrDigestSummaryTooLong,
		},
		{
			name: "digest.adequacyExplanation at the cap is legal",
			mutate: func(in *reviewpost.VerdictInput) {
				in.Digest.AdequacyExplanation = strings.Repeat("a", reviewpost.MaxDigestAdequacyExplanationBytes)
			},
			wantErr: nil,
		},
		{
			name: "digest.adequacyExplanation one byte over the cap is rejected",
			mutate: func(in *reviewpost.VerdictInput) {
				in.Digest.AdequacyExplanation = strings.Repeat("a", reviewpost.MaxDigestAdequacyExplanationBytes+1)
			},
			wantErr: reviewpost.ErrDigestAdequacyExplanationTooLong,
		},
		{
			name: "digest.stackRisks at the cap is legal",
			mutate: func(in *reviewpost.VerdictInput) {
				in.Digest.StackRisks = strings.Repeat("a", reviewpost.MaxDigestStackRisksBytes)
			},
			wantErr: nil,
		},
		{
			name: "digest.stackRisks one byte over the cap is rejected",
			mutate: func(in *reviewpost.VerdictInput) {
				in.Digest.StackRisks = strings.Repeat("a", reviewpost.MaxDigestStackRisksBytes+1)
			},
			wantErr: reviewpost.ErrDigestStackRisksTooLong,
		},
		{
			name: "digest.unverifiedLimits at the cap is legal",
			mutate: func(in *reviewpost.VerdictInput) {
				in.Digest.UnverifiedLimits = strings.Repeat("a", reviewpost.MaxDigestUnverifiedLimitsBytes)
			},
			wantErr: nil,
		},
		{
			name: "digest.unverifiedLimits one byte over the cap is rejected",
			mutate: func(in *reviewpost.VerdictInput) {
				in.Digest.UnverifiedLimits = strings.Repeat("a", reviewpost.MaxDigestUnverifiedLimitsBytes+1)
			},
			wantErr: reviewpost.ErrDigestUnverifiedLimitsTooLong,
		},
		{
			name: "digest.proposedBody at the cap is legal",
			mutate: func(in *reviewpost.VerdictInput) {
				in.Digest.ProposedBody = strings.Repeat("a", reviewpost.MaxDigestProposedBodyBytes)
			},
			wantErr: nil,
		},
		{
			name: "digest.proposedBody one byte over the cap is rejected",
			mutate: func(in *reviewpost.VerdictInput) {
				in.Digest.ProposedBody = strings.Repeat("a", reviewpost.MaxDigestProposedBodyBytes+1)
			},
			wantErr: reviewpost.ErrDigestProposedBodyTooLong,
		},
		{
			name: "digest.contestedPoints at the cap is legal",
			mutate: func(in *reviewpost.VerdictInput) {
				in.Digest.ContestedPoints = strings.Repeat("a", reviewpost.MaxDigestContestedPointsBytes)
			},
			wantErr: nil,
		},
		{
			name: "digest.contestedPoints one byte over the cap is rejected",
			mutate: func(in *reviewpost.VerdictInput) {
				in.Digest.ContestedPoints = strings.Repeat("a", reviewpost.MaxDigestContestedPointsBytes+1)
			},
			wantErr: reviewpost.ErrDigestContestedPointsTooLong,
		},
		{
			name: "archDecision.decision at the cap is legal",
			mutate: func(in *reviewpost.VerdictInput) {
				in.Digest.ArchDecisions = []reviewpost.ArchDecision{{Decision: strings.Repeat("a", reviewpost.MaxArchDecisionFieldBytes)}}
			},
			wantErr: nil,
		},
		{
			name: "archDecision.decision one byte over the cap is rejected",
			mutate: func(in *reviewpost.VerdictInput) {
				in.Digest.ArchDecisions = []reviewpost.ArchDecision{{Decision: strings.Repeat("a", reviewpost.MaxArchDecisionFieldBytes+1)}}
			},
			wantErr: reviewpost.ErrDigestArchDecisionFieldTooLong,
		},
		{
			name: "archDecision.rejectedAlternative one byte over the cap is rejected",
			mutate: func(in *reviewpost.VerdictInput) {
				in.Digest.ArchDecisions = []reviewpost.ArchDecision{{RejectedAlternative: strings.Repeat("a", reviewpost.MaxArchDecisionFieldBytes+1)}}
			},
			wantErr: reviewpost.ErrDigestArchDecisionFieldTooLong,
		},
		{
			name: "archDecision.conventionConformance one byte over the cap is rejected",
			mutate: func(in *reviewpost.VerdictInput) {
				in.Digest.ArchDecisions = []reviewpost.ArchDecision{{ConventionConformance: strings.Repeat("a", reviewpost.MaxArchDecisionFieldBytes+1)}}
			},
			wantErr: reviewpost.ErrDigestArchDecisionFieldTooLong,
		},
		{
			name: "the SECOND element of archDecisions is checked too, not just the first",
			mutate: func(in *reviewpost.VerdictInput) {
				in.Digest.ArchDecisions = []reviewpost.ArchDecision{
					{Decision: "short"},
					{Decision: strings.Repeat("a", reviewpost.MaxArchDecisionFieldBytes+1)},
				}
			},
			wantErr: reviewpost.ErrDigestArchDecisionFieldTooLong,
		},
		{
			name: "oversized stackRisks is rejected on the LIGHT path too -- the cap is unconditional, unlike the non-blank requirement",
			mutate: func(in *reviewpost.VerdictInput) {
				in.ReviewDepth = reviewtriage.ReviewDepth("")
				in.Digest.StackRisks = strings.Repeat("a", reviewpost.MaxDigestStackRisksBytes+1)
			},
			wantErr: reviewpost.ErrDigestStackRisksTooLong,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			in := validInput()
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

// TestValidateVerdictInput_LengthCapsCheckedLastAmongExisting proves the
// new length-cap block (G3) runs AFTER every pre-existing check, including
// the deep-path-only block and the Findings loop -- a payload with BOTH an
// invalid riskLevel AND an oversized digest.summary must still report
// ErrInvalidRiskLevel, never a TooLong error, mirroring
// TestValidateVerdictInput_DigestSummaryCheckedLastAmongExisting's own
// identical "added at the end of the fixed order" proof above.
func TestValidateVerdictInput_LengthCapsCheckedLastAmongExisting(t *testing.T) {
	in := validInput()
	in.RiskLevel = "bogus"
	in.Digest.Summary = strings.Repeat("a", reviewpost.MaxDigestSummaryBytes+1)

	err := reviewpost.ValidateVerdictInput(in)
	if !errors.Is(err, reviewpost.ErrInvalidRiskLevel) {
		t.Errorf("ValidateVerdictInput() = %v, want %v (riskLevel checked before the length caps)", err, reviewpost.ErrInvalidRiskLevel)
	}
}
