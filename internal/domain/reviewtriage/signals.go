package reviewtriage

import "github.com/narvidev/narvi/internal/domain/review"

// Signals is Decide's own input -- every field already resolved by the
// caller before this pure function ever runs (CLAUDE.md/§11: no I/O in
// domain). internal/app/reviewtriage is the one real assembler: most
// fields come straight off review.PreFetchedContext (Additions/
// Deletions/ChangedPaths/Labels, already fetched alongside the inline
// diff at review-session creation, per §26.3's own framing), and
// PriorVerdictRiskHigh comes from a fresh internal/app/reviewverdict.
// GetLatest read.
type Signals struct {
	// Additions/Deletions are this PR's own server-reported diff-size
	// facts (review.PreFetchedContext.Additions/Deletions) -- recorded as
	// Decision.ChangedLines, never compared against maxChangedLinesLight:
	// the size that routes comes from the diff alone (FileLines).
	Additions int
	Deletions int

	// ChangedPaths is the PR's own changed-file-path listing
	// (review.PreFetchedContext.ChangedPaths, ExtractChangedPaths' own
	// output) -- feeds BOTH the sensitive-glob check (sensitiveglob.go)
	// and the top-level-root-dispersion count (decide.go), always every
	// changed path, test and documentation files included: the size
	// exclusions (sizeexclusion.go) narrow the COUNT, never the paths. An
	// empty list is only a real "no paths" when InputRead says the input
	// was readable; otherwise Decide routes it as unreadable.
	ChangedPaths []string

	// FileLines is the diff's own per-file added/deleted lines
	// (ExtractFileLines over review.PreFetchedContext.Diff) -- the size
	// rule's only input (diffSize, sizeexclusion.go): Additions/Deletions
	// above are GitHub's separate count, recorded, never routed on.
	FileLines []FileLines

	// InputRead is how the reads behind this review's context ended
	// (review.InputRead) -- set by the context's producer and passed
	// through unchanged, never inferred from empty fields here. Anything
	// but complete or empty is an unreadable input: Decide routes it deep
	// under ReasonInputUnreadable, and records it in every case.
	InputRead review.InputRead

	// NeedsHumanLabelPresent reports whether this PR currently carries
	// reviewpost.LabelNeedsHuman (§8.2's own maintainer escape hatch)
	// -- see doc.go's own "v1 rules -- five, not three" section for why
	// this is one of Decide's five triggers.
	NeedsHumanLabelPresent bool

	// PriorVerdictRiskHigh reports whether the LATEST posted verdict for
	// this exact PR (internal/app/reviewverdict.GetLatest) carried
	// review.RiskLevelHigh -- §26.3's own explicit fourth rule ("a prior
	// high verdict routes deep"). false for a PR with no prior verdict
	// at all, indistinguishable from "the prior verdict was not high
	// risk" -- both are the same safe, non-triggering reading.
	PriorVerdictRiskHigh bool
}
