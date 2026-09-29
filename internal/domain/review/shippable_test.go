package review_test

import (
	"fmt"
	"reflect"
	"testing"

	"github.com/narvidev/narvi/internal/domain/review"
)

// rank is this test's own independent mirror of shippable.go's unexported
// total order (auto=0 < needs_human=1 < block=2), used ONLY to build
// expected values and inequality assertions below — it does not reach
// into the production package's own unexported rank()/shippableRank at
// all (this is an external _test package, matching every other domain
// package's own convention: internal/domain/gitstate_test's allTriggers,
// internal/domain/authz_test's TestAllRoles_MatchesRoleConstants). An
// unrecognized Shippable ranks one above Block here too, mirroring
// production's own defense-in-depth default — never exercised by a
// legitimate ComputeShippable/CoverageFloor/PremiseFloor result, since all
// three only ever return one of the three legal consts.
func rank(s review.Shippable) int {
	switch s {
	case review.ShippableAuto:
		return 0
	case review.ShippableNeedsHuman:
		return 1
	case review.ShippableBlock:
		return 2
	default:
		return 3
	}
}

// composeExpected mirrors ComputeShippable's own documented max(rank)
// composition, independently, purely to build this test's "want" values —
// see rank's own doc comment for why this independence is meaningful
// rather than circular.
func composeExpected(values ...review.Shippable) review.Shippable {
	best := review.ShippableAuto
	for _, v := range values {
		if rank(v) > rank(best) {
			best = v
		}
	}
	return best
}

// TestShippableRank_TotalOrder proves the three-level total order
// (auto < needs_human < block) holds behaviorally through the ONLY seam
// that exposes it, ComputeShippable's own max(rank) composition —
// directly exercising doc.go's "ranking is an explicit table" property:
// whichever of two conflicting inputs is objectively MORE conservative
// always wins, regardless of which argument position (baseline, coverage
// floor, or premise floor) it arrived through.
func TestShippableRank_TotalOrder(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name     string
		risk     review.RiskLevel
		coverage review.TestsCoverageState
		premise  review.PremiseState
		want     review.Shippable
	}{
		// needs_human (from the coverage floor) beats auto (from a clean
		// baseline and a clean premise floor).
		{"coverage floor beats a clean baseline and clean premise", review.RiskLevelLow, review.TestsCoverageStateInsufficient, review.PremiseStateOK, review.ShippableNeedsHuman},
		// block (from the premise floor) beats auto from everything else.
		{"premise floor beats a clean baseline and clean coverage", review.RiskLevelLow, review.TestsCoverageStateAdequate, review.PremiseStateNotAPR, review.ShippableBlock},
		// block (from the premise floor) beats needs_human (from the risk
		// baseline) too -- proving block outranks needs_human, not just
		// auto.
		{"premise floor (block) beats a needs_human baseline", review.RiskLevelHigh, review.TestsCoverageStateAdequate, review.PremiseStateNotAPR, review.ShippableBlock},
		// needs_human (from the risk baseline) beats auto from both
		// clean floors -- proving the baseline genuinely participates in
		// the max, not just the two floors.
		{"risk baseline (needs_human) beats two clean floors", review.RiskLevelHigh, review.TestsCoverageStateAdequate, review.PremiseStateOK, review.ShippableNeedsHuman},
		// all three clean: auto survives untouched.
		{"all three clean: auto", review.RiskLevelLow, review.TestsCoverageStateAdequate, review.PremiseStateOK, review.ShippableAuto},
	}

	for _, tc := range tests {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			// review.DescriptionAdequacyOK/review.CounterReviewDone impose
			// no floor of their own (AdequacyFloor/CounterReviewFloor's own
			// doc comments) -- held fixed here so this table's own
			// pre-existing risk/coverage/premise interplay assertions are
			// unaffected by the §26.2 and §26.4 additions;
			// see TestComputeShippable_AdequacyRaiseOnly,
			// TestComputeShippable_CounterReviewRaiseOnly, and
			// TestComputeShippable_FourFloorCompositionMatrix below for
			// those floors' own dedicated coverage.
			got := review.ComputeShippable(tc.risk, tc.coverage, tc.premise, review.DescriptionAdequacyOK, review.CounterReviewDone).Class()
			if got != tc.want {
				t.Errorf("ComputeShippable(%s, %s, %s, ok, done) = %s, want %s", tc.risk, tc.coverage, tc.premise, got, tc.want)
			}
		})
	}
}

// TestComputeShippable_RiskBaseline is exhaustive over every RiskLevel
// value (the three legal ones, the zero value, and an unrecognized one),
// with both floors held perfectly clean (TestsCoverageStateAdequate,
// PremiseStateOK) so the result is exactly baselineFromRisk(risk) with
// nothing else influencing it -- proving RiskLevel's own baseline mapping
// (doc.go design call #2), including its fail-conservative zero-value
// handling.
func TestComputeShippable_RiskBaseline(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		risk review.RiskLevel
		want review.Shippable
	}{
		{"low", review.RiskLevelLow, review.ShippableAuto},
		{"medium", review.RiskLevelMedium, review.ShippableNeedsHuman},
		{"high", review.RiskLevelHigh, review.ShippableNeedsHuman},
		{"zero value fails conservative (needs_human, matching high)", review.RiskLevel(""), review.ShippableNeedsHuman},
		{"unrecognized value fails conservative (needs_human, matching high)", review.RiskLevel("bogus"), review.ShippableNeedsHuman},
	}

	for _, tc := range tests {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got := review.ComputeShippable(tc.risk, review.TestsCoverageStateAdequate, review.PremiseStateOK, review.DescriptionAdequacyOK, review.CounterReviewDone).Class()
			if got != tc.want {
				t.Errorf("ComputeShippable(%s, adequate, ok, ok, done) = %s, want %s", tc.risk, got, tc.want)
			}
		})
	}
}

// TestComputeShippable_FourFloorCompositionMatrix is the exhaustive
// coverage-floor-state × premise-floor-state × description-adequacy-
// floor-state × counter-review-floor-state matrix (extended by §26.4
// from its own prior three-floor version, itself extended once already
// by §26.2 from an original two-floor version): every one of the
// five TestsCoverageState values under test
// (adequate/insufficient/skipped/zero/unrecognized) crossed with every one
// of the five PremiseState values under test
// (ok/questionable/not_a_pr/zero/unrecognized) crossed with every one of
// the five DescriptionAdequacy values under test
// (ok/drift/misleading/zero/unrecognized) crossed with every one of the
// three CounterReviewStatus values under test (done/skipped/unrecognized)
// = 375 rows, RiskLevel fixed at RiskLevelLow (baseline ShippableAuto, the
// most permissive) so each cell isolates exactly what the four floors
// alone compose to.
func TestComputeShippable_FourFloorCompositionMatrix(t *testing.T) {
	t.Parallel()

	coverageCases := []struct {
		name  string
		state review.TestsCoverageState
	}{
		{"adequate", review.TestsCoverageStateAdequate},
		{"insufficient", review.TestsCoverageStateInsufficient},
		{"skipped", review.TestsCoverageStateSkipped},
		{"zero", review.TestsCoverageState("")},
		{"unrecognized", review.TestsCoverageState("bogus")},
	}

	premiseCases := []struct {
		name  string
		state review.PremiseState
	}{
		{"ok", review.PremiseStateOK},
		{"questionable", review.PremiseStateQuestionable},
		{"not_a_pr", review.PremiseStateNotAPR},
		{"zero", review.PremiseState("")},
		{"unrecognized", review.PremiseState("bogus")},
	}

	adequacyCases := []struct {
		name  string
		state review.DescriptionAdequacy
	}{
		{"ok", review.DescriptionAdequacyOK},
		{"drift", review.DescriptionAdequacyDrift},
		{"misleading", review.DescriptionAdequacyMisleading},
		{"zero", review.DescriptionAdequacy("")},
		{"unrecognized", review.DescriptionAdequacy("bogus")},
	}

	counterReviewCases := []struct {
		name  string
		state review.CounterReviewStatus
	}{
		{"done", review.CounterReviewDone},
		{"skipped", review.CounterReviewSkipped},
		{"unrecognized", review.CounterReviewStatus("bogus")},
	}

	for _, cov := range coverageCases {
		cov := cov
		for _, prem := range premiseCases {
			prem := prem
			for _, adeq := range adequacyCases {
				adeq := adeq
				for _, cr := range counterReviewCases {
					cr := cr
					t.Run(cov.name+"_x_"+prem.name+"_x_"+adeq.name+"_x_"+cr.name, func(t *testing.T) {
						t.Parallel()

						covFloor := review.CoverageFloor(cov.state)
						premFloor := review.PremiseFloor(prem.state)
						adeqFloor := review.AdequacyFloor(adeq.state)
						crFloor := review.CounterReviewFloor(cr.state)
						want := composeExpected(review.ShippableAuto, covFloor, premFloor, adeqFloor, crFloor)

						got := review.ComputeShippable(review.RiskLevelLow, cov.state, prem.state, adeq.state, cr.state).Class()
						if got != want {
							t.Errorf("ComputeShippable(low, %s, %s, %s, %s) = %s, want %s (coverageFloor=%s, premiseFloor=%s, adequacyFloor=%s, counterReviewFloor=%s)",
								cov.name, prem.name, adeq.name, cr.name, got, want, covFloor, premFloor, adeqFloor, crFloor)
						}
					})
				}
			}
		}
	}
}

// TestComputeShippable_RaiseOnly proves the raise-only property directly,
// exhaustively across the full (risk × coverage × premise) input matrix
// (5×5×5 = 125 combinations, including every zero-value and unrecognized
// variant): ComputeShippable's result is NEVER ranked below what
// RiskLevel's own baseline alone implies, NEVER below CoverageFloor's own
// result alone, and NEVER below PremiseFloor's own result alone. This is
// the property the plan calls "raise-only" -- there is no input in this
// matrix for which applying a floor produces a LESS conservative outcome
// than not applying it. adequacy/counterReview are held at their own
// clean values here (ok/done) -- TestComputeShippable_AdequacyRaiseOnly
// and TestComputeShippable_CounterReviewRaiseOnly cover those two floors'
// own raise-only property in isolation.
func TestComputeShippable_RaiseOnly(t *testing.T) {
	t.Parallel()

	risks := []struct {
		name         string
		level        review.RiskLevel
		wantBaseline review.Shippable
	}{
		{"low", review.RiskLevelLow, review.ShippableAuto},
		{"medium", review.RiskLevelMedium, review.ShippableNeedsHuman},
		{"high", review.RiskLevelHigh, review.ShippableNeedsHuman},
		{"zero", review.RiskLevel(""), review.ShippableNeedsHuman},
		{"unrecognized", review.RiskLevel("bogus"), review.ShippableNeedsHuman},
	}

	coverages := []review.TestsCoverageState{
		review.TestsCoverageStateAdequate,
		review.TestsCoverageStateInsufficient,
		review.TestsCoverageStateSkipped,
		review.TestsCoverageState(""),
		review.TestsCoverageState("bogus"),
	}

	premises := []review.PremiseState{
		review.PremiseStateOK,
		review.PremiseStateQuestionable,
		review.PremiseStateNotAPR,
		review.PremiseState(""),
		review.PremiseState("bogus"),
	}

	for _, r := range risks {
		r := r
		for _, cov := range coverages {
			cov := cov
			for _, prem := range premises {
				prem := prem
				name := r.name + "_" + string(cov) + "_" + string(prem)
				t.Run(name, func(t *testing.T) {
					t.Parallel()

					covFloor := review.CoverageFloor(cov)
					premFloor := review.PremiseFloor(prem)
					got := review.ComputeShippable(r.level, cov, prem, review.DescriptionAdequacyOK, review.CounterReviewDone).Class()

					if rank(got) < rank(r.wantBaseline) {
						t.Errorf("ComputeShippable(%s, %s, %s, ok, done) = %s ranks BELOW the risk baseline %s alone -- raise-only violated",
							r.name, cov, prem, got, r.wantBaseline)
					}
					if rank(got) < rank(covFloor) {
						t.Errorf("ComputeShippable(%s, %s, %s, ok, done) = %s ranks BELOW CoverageFloor(%s)=%s alone -- raise-only violated",
							r.name, cov, prem, got, cov, covFloor)
					}
					if rank(got) < rank(premFloor) {
						t.Errorf("ComputeShippable(%s, %s, %s, ok, done) = %s ranks BELOW PremiseFloor(%s)=%s alone -- raise-only violated",
							r.name, cov, prem, got, prem, premFloor)
					}

					want := composeExpected(r.wantBaseline, covFloor, premFloor)
					if got != want {
						t.Errorf("ComputeShippable(%s, %s, %s, ok, done) = %s, want %s", r.name, cov, prem, got, want)
					}
				})
			}
		}
	}
}

// TestComputeShippable_AdequacyRaiseOnly is AdequacyFloor's own dedicated
// raise-only proof (§26.2), isolated from every other input:
// risk/coverage/premise are held at their cleanest legal values (low/
// adequate/ok, baseline ShippableAuto) so each row isolates exactly what
// the description-adequacy floor alone contributes, exhaustively over
// every DescriptionAdequacy value under test
// (ok/drift/misleading/zero/unrecognized) -- mirrors
// TestComputeShippable_RiskBaseline's own identical "isolate one input,
// hold the others clean" shape.
func TestComputeShippable_AdequacyRaiseOnly(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name     string
		adequacy review.DescriptionAdequacy
		want     review.Shippable
	}{
		{"ok", review.DescriptionAdequacyOK, review.ShippableAuto},
		{"drift", review.DescriptionAdequacyDrift, review.ShippableAuto},
		{"misleading raises to needs_human", review.DescriptionAdequacyMisleading, review.ShippableNeedsHuman},
		{"zero value fails conservative (needs_human, matching misleading)", review.DescriptionAdequacy(""), review.ShippableNeedsHuman},
		{"unrecognized value fails conservative (needs_human, matching misleading)", review.DescriptionAdequacy("bogus"), review.ShippableNeedsHuman},
	}

	for _, tc := range tests {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			adeqFloor := review.AdequacyFloor(tc.adequacy)
			got := review.ComputeShippable(review.RiskLevelLow, review.TestsCoverageStateAdequate, review.PremiseStateOK, tc.adequacy, review.CounterReviewDone).Class()

			if rank(got) < rank(adeqFloor) {
				t.Errorf("ComputeShippable(low, adequate, ok, %s, done) = %s ranks BELOW AdequacyFloor(%s)=%s alone -- raise-only violated",
					tc.adequacy, got, tc.adequacy, adeqFloor)
			}
			if got != tc.want {
				t.Errorf("ComputeShippable(low, adequate, ok, %s, done) = %s, want %s", tc.adequacy, got, tc.want)
			}
		})
	}
}

// TestComputeShippable_MisleadingRaisesShippable is the single, explicit
// pin this Step's own process requirements name by phrase: "the floor
// actually raising Shippable on misleading". A misleading description
// alone (everything else at its cleanest: low risk, adequate coverage, ok
// premise, done counter-review -- which alone would compute
// ShippableAuto) raises the RESULT to ShippableNeedsHuman, never leaving
// it at the clean baseline.
func TestComputeShippable_MisleadingRaisesShippable(t *testing.T) {
	t.Parallel()

	clean := review.ComputeShippable(review.RiskLevelLow, review.TestsCoverageStateAdequate, review.PremiseStateOK, review.DescriptionAdequacyOK, review.CounterReviewDone).Class()
	if clean != review.ShippableAuto {
		t.Fatalf("sanity check failed: an all-clean verdict computed %s, want %s (test setup is broken, not the property under test)", clean, review.ShippableAuto)
	}

	misleading := review.ComputeShippable(review.RiskLevelLow, review.TestsCoverageStateAdequate, review.PremiseStateOK, review.DescriptionAdequacyMisleading, review.CounterReviewDone).Class()
	if misleading != review.ShippableNeedsHuman {
		t.Errorf("ComputeShippable(low, adequate, ok, misleading, done) = %s, want %s (misleading must raise Shippable off an otherwise-clean auto baseline)", misleading, review.ShippableNeedsHuman)
	}
}

// TestComputeShippable_CounterReviewRaiseOnly is CounterReviewFloor's own
// dedicated raise-only proof (§26.4), isolated from every other
// input: risk/coverage/premise/adequacy are held at their cleanest legal
// values (low/adequate/ok/ok, baseline ShippableAuto) so each row isolates
// exactly what the counter-review floor alone contributes, exhaustively
// over every CounterReviewStatus value under test
// (done/skipped/uncorroborated/zero/unrecognized) -- mirrors
// TestComputeShippable_AdequacyRaiseOnly's own identical "isolate one
// input, hold the others clean" shape.
func TestComputeShippable_CounterReviewRaiseOnly(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name          string
		counterReview review.CounterReviewStatus
		want          review.Shippable
	}{
		{"done", review.CounterReviewDone, review.ShippableAuto},
		{"skipped raises to needs_human", review.CounterReviewSkipped, review.ShippableNeedsHuman},
		{"uncorroborated raises to needs_human, exactly as skipped", review.CounterReviewUncorroborated, review.ShippableNeedsHuman},
		{"zero value fails conservative (needs_human, matching skipped)", review.CounterReviewStatus(""), review.ShippableNeedsHuman},
		{"unrecognized value fails conservative (needs_human, matching skipped)", review.CounterReviewStatus("bogus"), review.ShippableNeedsHuman},
	}

	for _, tc := range tests {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			crFloor := review.CounterReviewFloor(tc.counterReview)
			got := review.ComputeShippable(review.RiskLevelLow, review.TestsCoverageStateAdequate, review.PremiseStateOK, review.DescriptionAdequacyOK, tc.counterReview).Class()

			if rank(got) < rank(crFloor) {
				t.Errorf("ComputeShippable(low, adequate, ok, ok, %s) = %s ranks BELOW CounterReviewFloor(%s)=%s alone -- raise-only violated",
					tc.counterReview, got, tc.counterReview, crFloor)
			}
			if got != tc.want {
				t.Errorf("ComputeShippable(low, adequate, ok, ok, %s) = %s, want %s", tc.counterReview, got, tc.want)
			}
		})
	}
}

// TestComputeShippable_CounterReviewSkippedRaisesShippable is the single,
// explicit pin this Step's own process requirements name by phrase, and
// call out as "the single most important property in this whole Step":
// CounterReview:skipped raises Shippable off an otherwise-clean auto
// baseline to needs_human -- the mirror image of
// TestComputeShippable_MisleadingRaisesShippable above, for the FOURTH
// floor rather than the third. See
// TestComputeShippable_FactCheckSkippedNeverRaisesShippable
// (internal/domain/reviewpost/validate_test.go) for this property's own
// deliberate, load-bearing OPPOSITE: FactCheck:skipped, unlike this floor,
// never raises anything at all.
func TestComputeShippable_CounterReviewSkippedRaisesShippable(t *testing.T) {
	t.Parallel()

	clean := review.ComputeShippable(review.RiskLevelLow, review.TestsCoverageStateAdequate, review.PremiseStateOK, review.DescriptionAdequacyOK, review.CounterReviewDone).Class()
	if clean != review.ShippableAuto {
		t.Fatalf("sanity check failed: an all-clean verdict computed %s, want %s (test setup is broken, not the property under test)", clean, review.ShippableAuto)
	}

	skipped := review.ComputeShippable(review.RiskLevelLow, review.TestsCoverageStateAdequate, review.PremiseStateOK, review.DescriptionAdequacyOK, review.CounterReviewSkipped).Class()
	if skipped != review.ShippableNeedsHuman {
		t.Errorf("ComputeShippable(low, adequate, ok, ok, skipped) = %s, want %s (a skipped counter-review must raise Shippable off an otherwise-clean auto baseline)", skipped, review.ShippableNeedsHuman)
	}
}

// riskBaseline is this test file's own independent mirror of
// shippable.go's unexported baselineFromRisk, for the same reason rank
// above mirrors the rank table: expected values are built without calling
// the code under test.
func riskBaseline(r review.RiskLevel) review.Shippable {
	if r == review.RiskLevelLow {
		return review.ShippableAuto
	}
	return review.ShippableNeedsHuman
}

// TestComputeShippable_NamesItsBlockers is §26.1's table of named cases:
// each input that keeps the class above auto is returned beside the class
// as a blocker naming the input, its value and the level it alone forces,
// and an auto verdict names none.
func TestComputeShippable_NamesItsBlockers(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name          string
		risk          review.RiskLevel
		coverage      review.TestsCoverageState
		premise       review.PremiseState
		adequacy      review.DescriptionAdequacy
		counterReview review.CounterReviewStatus
		wantClass     review.Shippable
		wantBlockers  []review.Blocker
	}{
		{
			name: "a medium-risk verdict with every floor clean names its risk level",
			risk: review.RiskLevelMedium, coverage: review.TestsCoverageStateAdequate, premise: review.PremiseStateOK, adequacy: review.DescriptionAdequacyOK, counterReview: review.CounterReviewDone,
			wantClass:    review.ShippableNeedsHuman,
			wantBlockers: []review.Blocker{{Input: review.ShippableInputRiskLevel, Value: "medium", Level: review.ShippableNeedsHuman}},
		},
		{
			name: "a verdict floored by a coverage gap names the coverage gap",
			risk: review.RiskLevelLow, coverage: review.TestsCoverageStateInsufficient, premise: review.PremiseStateOK, adequacy: review.DescriptionAdequacyOK, counterReview: review.CounterReviewDone,
			wantClass:    review.ShippableNeedsHuman,
			wantBlockers: []review.Blocker{{Input: review.ShippableInputTestsCoverage, Value: "insufficient", Level: review.ShippableNeedsHuman}},
		},
		{
			name: "an auto verdict names none",
			risk: review.RiskLevelLow, coverage: review.TestsCoverageStateAdequate, premise: review.PremiseStateOK, adequacy: review.DescriptionAdequacyDrift, counterReview: review.CounterReviewDone,
			wantClass:    review.ShippableAuto,
			wantBlockers: nil,
		},
		{
			name: "an uncorroborated counter-review is named as such",
			risk: review.RiskLevelLow, coverage: review.TestsCoverageStateSkipped, premise: review.PremiseStateOK, adequacy: review.DescriptionAdequacyOK, counterReview: review.CounterReviewUncorroborated,
			wantClass:    review.ShippableNeedsHuman,
			wantBlockers: []review.Blocker{{Input: review.ShippableInputCounterReview, Value: "uncorroborated", Level: review.ShippableNeedsHuman}},
		},
		{
			name: "a skipped counter-review is named as skipped",
			risk: review.RiskLevelLow, coverage: review.TestsCoverageStateAdequate, premise: review.PremiseStateOK, adequacy: review.DescriptionAdequacyOK, counterReview: review.CounterReviewSkipped,
			wantClass:    review.ShippableNeedsHuman,
			wantBlockers: []review.Blocker{{Input: review.ShippableInputCounterReview, Value: "skipped", Level: review.ShippableNeedsHuman}},
		},
		{
			name: "two inputs at the same level are both named",
			risk: review.RiskLevelHigh, coverage: review.TestsCoverageStateAdequate, premise: review.PremiseStateQuestionable, adequacy: review.DescriptionAdequacyOK, counterReview: review.CounterReviewDone,
			wantClass: review.ShippableNeedsHuman,
			wantBlockers: []review.Blocker{
				{Input: review.ShippableInputRiskLevel, Value: "high", Level: review.ShippableNeedsHuman},
				{Input: review.ShippableInputPremise, Value: "questionable", Level: review.ShippableNeedsHuman},
			},
		},
		{
			name: "a block floor is named beside a needs_human baseline, each at its own level",
			risk: review.RiskLevelMedium, coverage: review.TestsCoverageStateAdequate, premise: review.PremiseStateNotAPR, adequacy: review.DescriptionAdequacyMisleading, counterReview: review.CounterReviewDone,
			wantClass: review.ShippableBlock,
			wantBlockers: []review.Blocker{
				{Input: review.ShippableInputRiskLevel, Value: "medium", Level: review.ShippableNeedsHuman},
				{Input: review.ShippableInputPremise, Value: "not_a_pr", Level: review.ShippableBlock},
				{Input: review.ShippableInputDescriptionAdequacy, Value: "misleading", Level: review.ShippableNeedsHuman},
			},
		},
		{
			name: "an unset input is a blocker, carried verbatim",
			risk: review.RiskLevel(""), coverage: review.TestsCoverageStateAdequate, premise: review.PremiseStateOK, adequacy: review.DescriptionAdequacyOK, counterReview: review.CounterReviewDone,
			wantClass:    review.ShippableNeedsHuman,
			wantBlockers: []review.Blocker{{Input: review.ShippableInputRiskLevel, Value: "", Level: review.ShippableNeedsHuman}},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			got := review.ComputeShippable(tc.risk, tc.coverage, tc.premise, tc.adequacy, tc.counterReview)
			if got.Class() != tc.wantClass {
				t.Errorf("Class() = %q, want %q", got.Class(), tc.wantClass)
			}
			if blockers := got.Blockers(); !reflect.DeepEqual(blockers, tc.wantBlockers) {
				t.Errorf("Blockers() = %+v, want %+v", blockers, tc.wantBlockers)
			}
		})
	}
}

// TestComputeShippable_EveryRaisingInputIsABlocker is the property that
// makes a floor unable to raise the class without being named, checked
// over the risk baseline and every floor ComputeShippable composes: every
// value under test of each of the five inputs (the legal ones, the zero
// value and an unrecognized one) crossed with every other, 5^5 = 3125
// combinations. For each one, the expected level of each input on its own
// comes from the exported floor functions and this file's own risk mirror,
// never from ComputeShippable, and is confirmed against ComputeShippable
// with every other input clean. Then:
//
//   - the blockers are exactly the inputs whose own level is above auto,
//     in parameter order, each carrying its value and that level;
//   - the class is the least permissive of those levels, auto when none;
//   - there are no blockers exactly when the class is auto.
//
// An input wired into the class without a blocker, a blocker list
// computed apart from the class and missing a case, or a class that
// ignores a blocker each fail here.
func TestComputeShippable_EveryRaisingInputIsABlocker(t *testing.T) {
	t.Parallel()

	risks := []review.RiskLevel{review.RiskLevelLow, review.RiskLevelMedium, review.RiskLevelHigh, "", "bogus"}
	coverages := []review.TestsCoverageState{review.TestsCoverageStateAdequate, review.TestsCoverageStateInsufficient, review.TestsCoverageStateSkipped, "", "bogus"}
	premises := []review.PremiseState{review.PremiseStateOK, review.PremiseStateQuestionable, review.PremiseStateNotAPR, "", "bogus"}
	adequacies := []review.DescriptionAdequacy{review.DescriptionAdequacyOK, review.DescriptionAdequacyDrift, review.DescriptionAdequacyMisleading, "", "bogus"}
	counterReviews := []review.CounterReviewStatus{review.CounterReviewDone, review.CounterReviewSkipped, review.CounterReviewUncorroborated, "", "bogus"}

	const (
		cleanRisk          = review.RiskLevelLow
		cleanCoverage      = review.TestsCoverageStateAdequate
		cleanPremise       = review.PremiseStateOK
		cleanAdequacy      = review.DescriptionAdequacyOK
		cleanCounterReview = review.CounterReviewDone
	)

	combinations := 0
	for _, r := range risks {
		for _, c := range coverages {
			for _, p := range premises {
				for _, d := range adequacies {
					for _, cr := range counterReviews {
						combinations++
						name := fmt.Sprintf("(%q, %q, %q, %q, %q)", r, c, p, d, cr)

						type input struct {
							input review.ShippableInput
							value string
							level review.Shippable
							alone review.Shippable
						}
						inputs := []input{
							{review.ShippableInputRiskLevel, string(r), riskBaseline(r), review.ComputeShippable(r, cleanCoverage, cleanPremise, cleanAdequacy, cleanCounterReview).Class()},
							{review.ShippableInputTestsCoverage, string(c), review.CoverageFloor(c), review.ComputeShippable(cleanRisk, c, cleanPremise, cleanAdequacy, cleanCounterReview).Class()},
							{review.ShippableInputPremise, string(p), review.PremiseFloor(p), review.ComputeShippable(cleanRisk, cleanCoverage, p, cleanAdequacy, cleanCounterReview).Class()},
							{review.ShippableInputDescriptionAdequacy, string(d), review.AdequacyFloor(d), review.ComputeShippable(cleanRisk, cleanCoverage, cleanPremise, d, cleanCounterReview).Class()},
							{review.ShippableInputCounterReview, string(cr), review.CounterReviewFloor(cr), review.ComputeShippable(cleanRisk, cleanCoverage, cleanPremise, cleanAdequacy, cr).Class()},
						}

						var wantBlockers []review.Blocker
						levels := make([]review.Shippable, 0, len(inputs))
						for _, in := range inputs {
							if in.alone != in.level {
								t.Errorf("%s: %s on its own computes %q, want its own level %q", name, in.input, in.alone, in.level)
							}
							levels = append(levels, in.level)
							if rank(in.level) > rank(review.ShippableAuto) {
								wantBlockers = append(wantBlockers, review.Blocker{Input: in.input, Value: in.value, Level: in.level})
							}
						}

						got := review.ComputeShippable(r, c, p, d, cr)
						blockers := got.Blockers()
						if !reflect.DeepEqual(blockers, wantBlockers) {
							t.Errorf("%s: Blockers() = %+v, want %+v", name, blockers, wantBlockers)
						}
						if want := composeExpected(levels...); got.Class() != want {
							t.Errorf("%s: Class() = %q, want %q", name, got.Class(), want)
						}
						if (len(blockers) == 0) != (got.Class() == review.ShippableAuto) {
							t.Errorf("%s: Class() = %q with %d blockers; want no blockers exactly when the class is auto", name, got.Class(), len(blockers))
						}
					}
				}
			}
		}
	}
	if combinations != 3125 {
		t.Fatalf("checked %d combinations, want 3125", combinations)
	}
}

// TestShippableAssessment_ZeroValueIsNotAuto proves an assessment
// ComputeShippable did not produce never reads as auto, the same
// fail-conservative property TestVerdict_ZeroValueIsNotAuto pins for
// Verdict: its class is the empty Shippable, which no gate reads as
// eligible.
func TestShippableAssessment_ZeroValueIsNotAuto(t *testing.T) {
	t.Parallel()

	var a review.ShippableAssessment
	if a.Class() == review.ShippableAuto {
		t.Error("a zero-value ShippableAssessment must not have class auto")
	}
	if a.Class() == review.ShippableNeedsHuman || a.Class() == review.ShippableBlock {
		t.Errorf("a zero-value ShippableAssessment has class %q, want no legal Shippable value", a.Class())
	}
}

// TestShippableAssessment_BlockersIsACopy proves a caller cannot change an
// assessment's blockers, and with them its class, through the slice
// Blockers returns.
func TestShippableAssessment_BlockersIsACopy(t *testing.T) {
	t.Parallel()

	a := review.ComputeShippable(review.RiskLevelLow, review.TestsCoverageStateAdequate, review.PremiseStateNotAPR, review.DescriptionAdequacyOK, review.CounterReviewDone)
	blockers := a.Blockers()
	if len(blockers) != 1 {
		t.Fatalf("test setup: Blockers() = %+v, want one premise blocker", blockers)
	}
	blockers[0].Level = review.ShippableAuto

	if a.Class() != review.ShippableBlock {
		t.Errorf("Class() = %q after editing the returned slice, want %q", a.Class(), review.ShippableBlock)
	}
	if got := a.Blockers()[0].Level; got != review.ShippableBlock {
		t.Errorf("Blockers()[0].Level = %q after editing the returned slice, want %q", got, review.ShippableBlock)
	}
}
