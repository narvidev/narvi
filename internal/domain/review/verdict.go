package review

// Verdict is code review's first-class structured verdict (§8.2)
// — the type a review session (a later Step) produces and the
// server-side verdict-posting tool (§8.2) persists, never a
// free-text comment re-parsed after the fact (doc.go). It carries exactly
// the seven fields §8.2 names, plus
// ProposedShippable (required by the server-computed-Shippable property
// below) — see doc.go's design call #4 for why no Finding/rebuttal-content
// shape is included here.
//
// CONTRACT for the caller that constructs a Verdict (a later Step, never
// this package): Shippable must be populated with EXACTLY the Class of
// ComputeShippable(RiskLevel, TestsCoverage, Premise, DescriptionAdequacy,
// CounterReviewStatus)'s result — never a hand-set value, and never
// ProposedShippable converted to Shippable. This package cannot enforce
// that at the type-system level
// (Go has no way to make a struct field "write-once, only via this
// function") — the enforcement is ProposedShippable's own distinct type
// (verdict.go's sibling doc comment on shippable.go), which makes the one
// mistake this contract exists to prevent — assigning the model's own
// guess directly into Shippable — require a visible, explicit conversion
// rather than a silent one. This mirrors internal/domain/sandbox's own
// CircuitBreakerState.FailureCount contract note: a documented caller
// obligation this package's own functions do not themselves police,
// because policing it would require I/O or state this package does not
// have.
type Verdict struct {
	// RiskLevel is the reviewer's own overall risk assessment.
	RiskLevel RiskLevel
	// Premise is the reviewer's own assessment of the PR's stated premise.
	Premise PremiseState
	// BlastRadius is the set of fixed-vocabulary Tag values (tag.go)
	// describing which system-surface areas this PR's diff touches. A nil
	// or empty slice is a legitimate value (the reviewer found no tagged
	// area touched); this package places no floor on it — see doc.go's
	// design call #3.
	BlastRadius []Tag
	// FilesChanged is the number of files the diff touches. Purely
	// descriptive data in this package — a later Step's auto-approval
	// eligibility engine (§21.2) is the one that gates on a diff-size
	// threshold, not this package.
	FilesChanged int
	// TestsCoverage is the test-coverage sentinel's own assessed state.
	TestsCoverage TestsCoverageState
	// DocsDrift is the doc-drift sentinel's own assessed state. Carried
	// as data; not wired into Shippable in this Step (doc.go design call
	// #5).
	DocsDrift DocsDriftState
	// ProposedShippable is the model's own self-report — advisory only,
	// of a type distinct from Shippable below, and never an input to
	// ComputeShippable. See its own doc comment (shippable.go) and this
	// struct's own top-level CONTRACT.
	ProposedShippable ProposedShippable
	// Shippable is the AUTHORITATIVE, server-computed classification.
	// CONTRACT: populate only via the Class of ComputeShippable's result — see
	// this struct's own top-level doc comment.
	Shippable Shippable
}
