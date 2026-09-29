package review

// Shippable is the automated-approval engine's own gating field (§21.2, a
// later Step): auto/needs_human/block. This is the AUTHORITATIVE,
// server-computed classification — the ONLY sanctioned way to produce a
// legitimate value is the Class of ComputeShippable's result. See doc.go's
// "server-computed Shippable" section and ProposedShippable's own doc
// comment below for why this is a distinct type from the model's own
// self-report, never interchangeable with it without an explicit
// conversion.
type Shippable string

// The three Shippable values, ordered auto < needs_human < block (most to
// least permissive) — see shippableRank below for the explicit total
// order, and doc.go's "ranking is an explicit table" section for why that
// order is never inferred from these consts' declaration position.
const (
	// ShippableAuto is eligible for the automated-approval eligibility
	// engine (§21.2) to consider — NOT itself sufficient for auto-merge;
	// §21.2 layers CI-green/diff-size/sensitive-path/verdict-freshness
	// criteria on top of Shippable == auto.
	ShippableAuto Shippable = "auto"
	// ShippableNeedsHuman requires a person to review before the PR can
	// proceed; excluded from auto-approval outright.
	ShippableNeedsHuman Shippable = "needs_human"
	// ShippableBlock is this package's strictest classification —
	// reserved for premise-level illegitimacy (PremiseStateNotAPR) or any
	// unrecognized enum value anywhere in this package's fail-conservative
	// policy (doc.go). See RiskLevel's own doc comment for why RiskLevel
	// alone, even at its own highest defined tier, never reaches this
	// value on its own.
	ShippableBlock Shippable = "block"
)

// ProposedShippable is the model's OWN self-report of what it believes
// Shippable should be — carried on Verdict purely for audit/transparency
// (§21.2: "the LLM's verdict only ever *proposes* Shippable"). It is a
// DISTINCT Go type from Shippable, not merely a different field name: a
// ProposedShippable value cannot be assigned into a Shippable-typed field
// or parameter (Verdict.Shippable, any argument of ComputeShippable)
// without an explicit type conversion, so accidentally laundering a
// model's guess into the authoritative field is a visible, grep-able act,
// never a silent one. ComputeShippable's own signature does not accept a
// ProposedShippable parameter at all — the model's opinion is not merely
// distrusted, it is structurally unable to influence the computation.
//
// The three ProposedShippable values share Shippable's own underlying
// strings (auto/needs_human/block) deliberately — the model is proposing
// the same three-way classification, just via a type the compiler will
// never treat as equivalent to the authoritative one. Nothing in this
// package validates, ranks, or otherwise interprets a ProposedShippable
// value; it is opaque data as far as ComputeShippable is concerned.
type ProposedShippable string

// The three ProposedShippable values, mirroring Shippable's own three
// strings (see the type's own doc comment for why the values match but the
// type does not).
const (
	ProposedShippableAuto       ProposedShippable = "auto"
	ProposedShippableNeedsHuman ProposedShippable = "needs_human"
	ProposedShippableBlock      ProposedShippable = "block"
)

// shippableRank is the explicit total order over Shippable, from most to
// least permissive: auto(0) < needs_human(1) < block(2). Deliberately NOT
// derived from iota/declaration order on the Shippable consts above (see
// doc.go's "ranking is an explicit table" section) — a reorder of those
// consts (e.g. an alphabetizing pass, or a new value inserted between two
// existing ones) can never silently invert the policy this table encodes,
// because nothing in this package ever compares the consts' own
// declaration position; every comparison goes through rank() below.
var shippableRank = map[Shippable]int{
	ShippableAuto:       0,
	ShippableNeedsHuman: 1,
	ShippableBlock:      2,
}

// rank returns s's position in shippableRank's explicit total order. Every
// producer in this package (baselineFromRisk, CoverageFloor, PremiseFloor,
// AdequacyFloor) only ever returns one of the three legal Shippable
// consts, so this branch is never exercised by a legitimate call within
// this package today — it exists purely as defense in depth, so that IF a
// future change ever pipes an unvalidated Shippable value through rank()
// (rather than through one of the producer functions above), the result
// fails conservative: ranked one above the strictest legal value (ShippableBlock
// itself), never silently read as rank 0 ("auto") via a bare map-lookup
// miss. maxShippable below never reconstructs a NEW Shippable value from a
// rank number — it always returns one of its two INPUT values verbatim —
// so this sentinel rank can never itself leak out as a fabricated
// Shippable string.
func rank(s Shippable) int {
	if r, ok := shippableRank[s]; ok {
		return r
	}
	return len(shippableRank)
}

// maxShippable returns whichever of a, b ranks least permissive (highest
// rank), preferring b on a tie (a bare `>` on a's rank, not `>=`) — the
// specific tie-break is immaterial here since ShippableAssessment.Class
// only ever calls this with two values it expects MAY be equal, never
// relying on which one "wins" a tie to distinguish behavior.
func maxShippable(a, b Shippable) Shippable {
	if rank(a) > rank(b) {
		return a
	}
	return b
}

// baselineFromRisk maps the reviewer's own overall RiskLevel to the
// Shippable value that would apply BEFORE any floor is considered —
// i.e. what Shippable would be if coverage, premise, description
// adequacy, and counter-review were all perfectly clean. RiskLevel is not
// itself one of the four named raise-only floors (coverage and premise,
// per §8.2; description adequacy, §26.2; counter-review,
// §26.4 — see ComputeShippable's own doc comment below for the
// full, current list) — see doc.go's design call #2 for why it is instead
// treated as the baseline those four floors can only ever raise, never
// lower, keeping "raise-only" uniform across all five inputs to
// ComputeShippable.
//
//   - RiskLevelLow: ShippableAuto — the reviewer found nothing that
//     itself warrants human gating; only a floor can raise this further.
//   - RiskLevelMedium, RiskLevelHigh: ShippableNeedsHuman — a person
//     should look, but nothing here alone means the PR must be blocked
//     outright (see RiskLevel's own doc comment for why a three-tier
//     scale never reaches Block on its own).
//   - the zero value or any other unrecognized RiskLevel:
//     ShippableNeedsHuman, FAILING CONSERVATIVE — ranked identically to
//     RiskLevelHigh, this enum's own worst known legitimate value (doc.go's
//     uniform fail-conservative policy), never as permissive as
//     RiskLevelLow's ShippableAuto.
func baselineFromRisk(r RiskLevel) Shippable {
	switch r {
	case RiskLevelLow:
		return ShippableAuto
	case RiskLevelMedium, RiskLevelHigh:
		return ShippableNeedsHuman
	default:
		return ShippableNeedsHuman
	}
}

// ShippableInput names one of the five inputs ComputeShippable composes:
// the risk baseline or one of the four raise-only floors. It is how a
// Blocker says which input it is.
type ShippableInput string

// The five ShippableInput values, one per ComputeShippable parameter, in
// the order ComputeShippable composes them, which is also the order a
// ShippableAssessment lists its blockers in.
const (
	ShippableInputRiskLevel           ShippableInput = "risk_level"
	ShippableInputTestsCoverage       ShippableInput = "tests_coverage"
	ShippableInputPremise             ShippableInput = "premise"
	ShippableInputDescriptionAdequacy ShippableInput = "description_adequacy"
	ShippableInputCounterReview       ShippableInput = "counter_review"
)

// Blocker is one input that, on its own, keeps Shippable above auto
// (§26.1): the risk baseline when it is above auto, or a floor that is.
// Only ComputeShippable puts a Blocker into a ShippableAssessment.
type Blocker struct {
	// Input is which of ComputeShippable's inputs this is.
	Input ShippableInput
	// Value is that input's value exactly as ComputeShippable received it:
	// "medium" for a medium risk level, "uncorroborated" for a
	// counter-review reported done that the server could not corroborate.
	// An empty or unrecognized value keeps the class above auto under the
	// fail-conservative policy (doc.go), so it is a blocker too, carried
	// verbatim.
	Value string
	// Level is the class this input alone forces: needs_human or block,
	// never auto.
	Level Shippable
}

// ShippableAssessment is ComputeShippable's result: the blockers, and the
// class computed from them. It holds the blockers and nothing else, so
// the two cannot disagree: the class is auto when there are no blockers
// and otherwise the least permissive Level among them. Only
// ComputeShippable builds one; the zero value is not an assessment, and
// its Class is the empty Shippable, never auto (the same property
// Verdict's own zero value has, TestVerdict_ZeroValueIsNotAuto).
type ShippableAssessment struct {
	assessed bool
	blockers []Blocker
}

// Class is the authoritative Shippable. It is computed here, from the
// blockers, and nowhere else.
func (a ShippableAssessment) Class() Shippable {
	if !a.assessed {
		return ""
	}
	class := ShippableAuto
	for _, b := range a.blockers {
		class = maxShippable(class, b.Level)
	}
	return class
}

// Blockers returns every input that on its own keeps Class above auto, in
// ComputeShippable's parameter order. It is nil exactly when Class is
// auto, and two inputs at the same level are both listed. The slice is a
// copy.
func (a ShippableAssessment) Blockers() []Blocker {
	return append([]Blocker(nil), a.blockers...)
}

// floor is one of ComputeShippable's inputs as a single value: which input
// it is, its value, and the class it alone forces. assess turns every
// floor above auto into a Blocker and keeps nothing else, and Class is
// computed from those blockers alone, so no input can raise the class
// without being named among them.
type floor struct {
	input ShippableInput
	value string
	level Shippable
}

// assess builds the ShippableAssessment for floors: one Blocker per floor
// whose level ranks above auto, in the order given.
func assess(floors ...floor) ShippableAssessment {
	a := ShippableAssessment{assessed: true}
	for _, f := range floors {
		if rank(f.level) > rank(ShippableAuto) {
			a.blockers = append(a.blockers, Blocker{Input: f.input, Value: f.value, Level: f.level})
		}
	}
	return a
}

// ComputeShippable is domain/review's single exported pure function for
// deriving Shippable (§8.2, extended by §26.2 and again by
// §26.4) — the ONLY sanctioned way any caller computes an
// authoritative Shippable value. It is a pure function of the reviewer's
// own RiskLevel plus the FOUR independent raise-only floors (coverage,
// premise, description adequacy, counter-review), composed via max(rank):
//
//	class = max(rank(baselineFromRisk(risk)),
//	            rank(CoverageFloor(coverage)),
//	            rank(PremiseFloor(premise)),
//	            rank(AdequacyFloor(adequacy)),
//	            rank(CounterReviewFloor(counterReview)))
//
// The class comes back with its blockers (§26.1): every input that on
// its own would keep the class above auto. Each input enters as one floor
// value carrying both its level and what names it, and the class is
// computed from the blockers those values produce (ShippableAssessment),
// so there is no second computation for the two to drift apart in: no
// blockers exactly when the class is auto, and two inputs at the same
// level are both named. TestComputeShippable_EveryRaisingInputIsABlocker
// (shippable_test.go) checks this over the baseline and every floor.
//
// adequacy (§26.2) is this function's own THIRD floor input,
// added alongside the original two (coverage, premise) §8.2 already
// established — see AdequacyFloor's own doc comment (adequacy.go) for its
// full policy. counterReview (§26.4) is the FOURTH, most recent
// addition — see CounterReviewFloor's own doc comment (counterreview.go)
// for its full policy, and that type's own doc comment for why a caller on
// the LIGHT path (where counter-review never runs at all) must pass
// CounterReviewDone rather than the type's own zero value — see this
// function's own closing paragraph below for why RiskLevel (untouched by
// any floor) is unaffected by either addition.
//
// Deliberately NOT a parameter here: any model-proposed value
// (ProposedShippable). This is not an oversight — it is the whole point of
// §8.2's "Shippable is server-computed, never the model's
// self-report" rule (doc.go's own top-level section): the model's own
// guess is structurally incapable of influencing this function's result,
// because it is not a parameter this signature even accepts. A caller
// wiring up a Verdict (a later Step) is expected to populate
// Verdict.Shippable with EXACTLY this function's result's Class and never
// with Verdict's own ProposedShippable field, converted or otherwise — see
// Verdict's own doc comment (verdict.go).
//
// RAISE-ONLY property: for any (risk, coverage, premise, adequacy,
// counterReview) input, this function never returns a class ranked
// BELOW baselineFromRisk(risk) alone, nor below CoverageFloor(coverage)
// alone, nor below PremiseFloor(premise) alone, nor below
// AdequacyFloor(adequacy) alone, nor below CounterReviewFloor(counterReview)
// alone — max() is monotonic in each argument by construction, and every
// producer above returns one of exactly three legal Shippable values, so
// this composition can never observe (let alone propagate) an out-of-band
// rank. See TestComputeShippable_RaiseOnly (shippable_test.go) for this
// property proved exhaustively across the full input matrix.
//
// RiskLevel is NEVER touched by this function, or by AdequacyFloor, or by
// CounterReviewFloor, or by any other floor — §26.2 states this asymmetry
// explicitly ("deliberately never inflating RiskLevel... the server
// computes Shippable, but never fabricates risk the model did not
// report"): RiskLevel is carried on Verdict verbatim from the reviewer's
// own self-reported assessment, upstream of and structurally unrelated to
// this function's own Shippable computation — see
// TestBuildVerdict_AdequacyNeverAffectsRiskLevel
// (internal/domain/reviewpost/validate_test.go) for the pin, at
// BuildVerdict's own real construction site.
func ComputeShippable(risk RiskLevel, coverage TestsCoverageState, premise PremiseState, adequacy DescriptionAdequacy, counterReview CounterReviewStatus) ShippableAssessment {
	return assess(
		floor{input: ShippableInputRiskLevel, value: string(risk), level: baselineFromRisk(risk)},
		floor{input: ShippableInputTestsCoverage, value: string(coverage), level: CoverageFloor(coverage)},
		floor{input: ShippableInputPremise, value: string(premise), level: PremiseFloor(premise)},
		floor{input: ShippableInputDescriptionAdequacy, value: string(adequacy), level: AdequacyFloor(adequacy)},
		floor{input: ShippableInputCounterReview, value: string(counterReview), level: CounterReviewFloor(counterReview)},
	)
}
