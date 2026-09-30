// Package reviewtriage implements §26.3's own light/deep review-depth
// routing decision (§26.3): "the depth decision is made in the single
// funnel (§8.2's creation/dispatch path), deterministic-first -- the
// same posture as everywhere else in this system (the server does not
// trust agent judgment for routing; deterministic fallbacks throughout,
// §18)." Every function here is pure per CLAUDE.md/§11: no I/O, no
// time.Now(), no randomness. Unlike internal/domain/review, this package
// does import the standard library (path/strings/strconv) and sibling
// domain packages (internal/domain/autoapproval, for its already-shipped
// path->BlastRadius-tag classifier; internal/domain/codeowners, for the
// one gitignore-style glob matcher the size patterns reuse) -- both real,
// existing precedents:
// internal/domain/autoapproval itself already imports "path"/"strings"
// (blastradius.go), and cross-domain-package imports are already
// established (internal/domain/autoapproval imports internal/domain/
// review, internal/domain/reviewverdict imports internal/domain/
// reviewpost, and so on).
//
// # Two pure functions, deliberately separate
//
// Decide computes what TODAY's diff alone would route to -- it has no
// awareness of any PAST review of the same PR. Floor composes that fresh
// result with the PR's own previously-recorded depth (§24's re-review
// floor: "once deep, a PR stays deep"), mirroring internal/domain/
// review's own "one pure function per floor, composed by the caller"
// discipline (CoverageFloor/PremiseFloor/AdequacyFloor, composed by
// ComputeShippable) rather than folding history into Decide itself. This
// keeps each function independently testable (and independently
// mutation-testable, CLAUDE.md's own process requirement) and keeps
// "what would a fresh diff alone route to" a legible, standalone
// question a re-review path is free to answer differently (§24: "depth
// re-evaluated on the delta").
//
// # Do not invent a second BlastRadius vocabulary
//
// internal/domain/autoapproval/blastradius.go's own top doc comment
// explicitly anticipates this: "§26.3's own 'sensitive globs ...
// mapped deterministically onto the same BlastRadius tags' is a LATER,
// broader design ... A straightforward, additive rewrite once §26.3
// actually lands, never a migration this file needs to anticipate." This package follows that
// note literally: sensitiveGlobHit (decide.go) calls
// autoapproval.ClassifyChangedPaths directly -- the SAME eight-value
// review.Tag vocabulary, the SAME path-classification heuristics
// (deliberately over-inclusive, never under-inclusive, that file's own
// design principle) -- rather than hand-rolling a second, parallel
// classifier that could silently drift from the first.
//
// # v1 rules -- five, not three, and why
//
// §26.3's own compact recap sentence names exactly three conditions
// ("any sensitive-glob hit -> always deep; >600 changed lines or >=3
// distinct top-level path roots -> deep; otherwise light"), worded with
// a seemingly-exhaustive "otherwise". But the SAME section's own,
// fuller "Signals" paragraph, one paragraph earlier, states a fourth
// rule explicitly and unambiguously: "the PR's own verdict history (a
// prior high verdict routes deep)". This package treats that
// clause as binding (ignoring an explicit "routes deep" statement would
// be a worse plan-fidelity failure than the recap sentence's own
// "otherwise" wording tension) and additionally treats the PR's existing
// review:needs-human label (internal/domain/reviewpost.LabelNeedsHuman,
// §8.2's own maintainer escape hatch) as a fifth trigger: routing an
// already-flagged-needs-human PR through the MORE rigorous deep path is
// strictly the safer direction, consistent with §26.9's own invariant
// ("the router may only ever add depth, never subtract rigor"). This
// inconsistency between §26.3's own recap sentence and its fuller prose
// is flagged in this Step's own PR description, not silently resolved.
//
// review:low-risk/review:medium-risk/review:high-risk (§8.2's own
// bot-synced risk labels, reviewpost.RiskLabel) are deliberately NOT
// used as an independent sixth trigger here: they are themselves derived
// from the SAME verdict history the fourth rule above already consults
// (reviewpost.ComputeLabelSync mirrors a verdict's own RiskLevel onto
// exactly one of those three labels), so treating them as a second,
// separate signal would be redundant with, and could lag behind, the
// database read the fourth rule already performs.
//
// # An unreadable input is not a small change
//
// The rules above read a size and a set of changed paths. When the pull
// request or its diff could not be read, those are missing or partial,
// and an empty list looks exactly like a one-line fix. So the reads' own
// outcome reaches Decide as a typed fact (Signals.InputRead, set by the
// review context's producer): anything but a complete or genuinely empty
// read routes deep under ReasonInputUnreadable. That rule is the LAST deep
// rule: a path, a line or a root seen in a partial read is still there in
// the whole one (a truncated diff holds whole lines only -- the code host
// adapter cuts it back to its last line boundary), and the verdict history
// and the labels do not come from the diff, so each earlier rule fires only
// on a real signal and records its own reason. The unreadable reason is recorded only when nothing else
// routes deep, which is why a depth chosen for it is never a later
// review's floor (NonFloorReasons, depth.go) while the others still are.
// An always_light override still wins, being an admin's decision, and the
// record names the cause either way.
//
// # The size counts source changes only
//
// The line threshold is compared against the diff's own lines less those
// of files this deployment's size patterns match (DefaultSizeExclusions,
// sizeexclusion.go, which lists each default and why it cannot match
// hand-written production code). The size comes from the one diff read
// pinned to the head under review, never from GitHub's reported counts
// less something, which could describe another head. The patterns are the
// deployment's, matched on file names alone, never on a directory name or
// on markers the pull request carries about its own files. The path rules
// still read every changed path, so a file excluded from the count still
// counts toward the sensitive-path and root rules.
//
// # Per-repo config: mode + deepPaths only, thresholds stay fixed
//
// §26.3 states thresholds are "per-repo-tunable" in prose, but the ONE
// concrete config shape it names is `reviewDepth: {mode, deepPaths}` --
// no threshold field at all. Config (config.go) follows the concrete,
// named shape literally: DeepPaths is a real per-repo override surface,
// but the 600-line/3-root thresholds are fixed package constants
// (maxChangedLinesLight/minDistinctRootsForDeep, decide.go), not exposed
// via any config column -- mirroring ReviewAutoRetriggerBudget's own
// identical precedent (internal/app/sessionactor/reviewretrigger.go:
// "a plain package constant... §24.6 itself only asks for a documented,
// reasoned default, not a per-repo override surface"). This resolution
// of the "per-repo-tunable thresholds" vs. "{mode, deepPaths}" tension
// is also flagged in this Step's own PR description.
package reviewtriage
