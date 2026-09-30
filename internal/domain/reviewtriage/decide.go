package reviewtriage

import (
	"strings"

	"github.com/narvidev/narvi/internal/domain/review"
)

// maxChangedLinesLight and minDistinctRootsForDeep are §26.3's own v1
// thresholds, verbatim ("initial thresholds... >600 changed lines or >=3
// distinct top-level path roots -> deep"). Fixed package constants, not a
// Config field -- see doc.go's own "Per-repo config" section for why.
const (
	maxChangedLinesLight    = 600
	minDistinctRootsForDeep = 3
)

// Reason is Decide's own short, human-readable explanation for why it
// picked the depth it did -- one fixed string per v1 rule, mirroring
// internal/domain/autoapproval.Reason's own identical "a normal domain
// OUTCOME, never a Go error" shape. Suitable for a structured log field
// and for the §18.4-precedent routing-decision record's own reasoning
// column.
type Reason string

// The Reason values Decide can return -- one per v1 rule, checked in the
// fixed order Decide's own doc comment documents.
const (
	ReasonAlwaysLightConfig Reason = "repo config: mode=always_light"
	ReasonAlwaysDeepConfig  Reason = "repo config: mode=always_deep"
	ReasonSensitiveGlob     Reason = "sensitive glob touched"
	ReasonDeepPathConfig    Reason = "repo-configured deep path touched"
	ReasonChangedLinesOver  Reason = "changed lines exceed threshold"
	ReasonRootDispersion    Reason = "distinct top-level path roots at or above threshold"
	ReasonPriorHighVerdict  Reason = "prior verdict for this PR was high risk"
	ReasonNeedsHumanLabel   Reason = "review:needs-human label present"
	ReasonLightDefault      Reason = "no deep-routing signal"
	// ReasonInputUnreadable: the pull request or its diff could not be
	// read in full (Signals.InputRead), so the size and the changed paths
	// the other rules read are missing or partial. §26.3: a missing size
	// costs a thorough review, a missing scope costs a finding. A depth
	// chosen for this reason is never the next review's floor
	// (NonFloorReasons, depth.go).
	ReasonInputUnreadable Reason = "review input could not be read in full"
)

// Decision is Decide's own output -- recorded verbatim on the §18.4-
// precedent routing-decision record (internal/app/reviewtriage) and, via
// its Depth field alone, persisted as review_verdicts.review_path
// (§21).
type Decision struct {
	Depth ReviewDepth
	// Reason is the FIRST rule that fired, in the fixed order Decide
	// checks them (this function's own doc comment) -- never a
	// combination of every rule that WOULD have matched, mirroring
	// internal/domain/autoapproval.ComputeEligible's own "first check
	// that fails wins" precedent.
	Reason Reason
	// MatchedSensitiveTags is non-nil only when Reason ==
	// ReasonSensitiveGlob -- the specific review.Tag(s) the sensitive-
	// glob check matched, carried through for the routing-decision
	// record's own audit trail.
	MatchedSensitiveTags []review.Tag
	// ChangedLines is Signals.Additions+Signals.Deletions, the pull
	// request's own reported total, carried through verbatim for the
	// routing-decision record.
	ChangedLines int
	// SourceLines is the size the line threshold is compared against:
	// ChangedLines less the lines the diff attributes to files the
	// deployment's size exclusions match (sourceChangedLines,
	// sizeexclusion.go).
	SourceLines int
	// DistinctRoots is the number of distinct top-level path roots
	// Signals.ChangedPaths touches, carried through verbatim for the
	// routing-decision record.
	DistinctRoots int
	// InputRead is Signals.InputRead, carried through so the record names
	// how the input was read whatever rule decided -- an always_light
	// override included.
	InputRead review.InputRead
}

// topLevelRoot returns p's own first path segment -- "internal/domain/
// review/context.go" -> "internal"; a path with no "/" at all (a
// repo-root file, e.g. "README.md") returns itself, its own root.
func topLevelRoot(p string) string {
	if idx := strings.Index(p, "/"); idx >= 0 {
		return p[:idx]
	}
	return p
}

// distinctRoots counts the number of distinct topLevelRoot values across
// paths -- §26.3's own "cross-cutting dispersion: number of distinct
// top-level path roots touched".
func distinctRoots(paths []string) int {
	roots := make(map[string]bool, len(paths))
	for _, p := range paths {
		if p == "" {
			continue
		}
		roots[topLevelRoot(p)] = true
	}
	return len(roots)
}

// resolveMode normalizes cfg.Mode -- the zero value or any other
// unrecognized string reads as ModeAuto (Mode's own doc comment: a
// garbled config value must never itself silently force always_deep or
// always_light).
func resolveMode(m Mode) Mode {
	switch m {
	case ModeAlwaysLight, ModeAlwaysDeep:
		return m
	default:
		return ModeAuto
	}
}

// Decide computes what sig's own signals alone route to, under cfg --
// §26.3's own v1 rule cascade, deterministic-first, no LLM tie-break.
// Pure per CLAUDE.md/§11.
//
// Checked in this fixed order, first match wins (mirrors internal/domain/
// autoapproval.ComputeEligible's own "first check that fails wins"
// discipline, and internal/domain/reviewpost.ValidateVerdictInput's own
// "fixed order... deterministic first error" discipline):
//
//  1. cfg.Mode == always_light / always_deep: an explicit admin override,
//     checked before any signal at all -- an unreadable input included.
//  2. Any changed path matches the fixed sensitive-glob set (migrations/
//     auth/infra-as-code+CI-workflows) OR any repo-configured
//     cfg.DeepPaths entry -> deep. Every changed path is read, test and
//     documentation files included, and a partial list is still read:
//     a sensitive path that WAS seen is a real signal.
//  3. Signals.InputRead is not readable (the pull request or its diff
//     could not be read in full) -> deep, ReasonInputUnreadable. Checked
//     before every count, since the counts are what is missing.
//  4. The source line count (ChangedLines less the lines of files the
//     deployment's cfg.SizeExclusions match) > 600, OR distinct top-level
//     path roots >= 3 -> deep.
//  5. Signals.PriorVerdictRiskHigh -> deep (§26.3's own explicit fourth
//     rule; see doc.go's own "v1 rules -- five, not three" section).
//  6. Signals.NeedsHumanLabelPresent -> deep (this package's own fifth
//     rule, same section).
//  7. Otherwise: light.
//
// This function has no error return at all. An input that could not be
// read is not a router error (§26.3): it reaches rule 3 and routes deep
// under a reason of its own. The fail-open-to-light rule covers only the
// caller's own reads (config, verdict history) -- see internal/app/
// reviewtriage.ComputeDecision's own doc comment for how those degrade.
func Decide(sig Signals, cfg Config) Decision {
	base := Decision{
		ChangedLines:  sig.Additions + sig.Deletions,
		DistinctRoots: distinctRoots(sig.ChangedPaths),
		InputRead:     sig.InputRead,
	}
	base.SourceLines = sourceChangedLines(base.ChangedLines, sig.FileLines, cfg.SizeExclusions)
	decide := func(depth ReviewDepth, reason Reason) Decision {
		d := base
		d.Depth = depth
		d.Reason = reason
		return d
	}

	switch resolveMode(cfg.Mode) {
	case ModeAlwaysLight:
		return decide(DepthLight, ReasonAlwaysLightConfig)
	case ModeAlwaysDeep:
		return decide(DepthDeep, ReasonAlwaysDeepConfig)
	}

	if tags := classifySensitivePaths(sig.ChangedPaths); len(tags) > 0 {
		d := decide(DepthDeep, ReasonSensitiveGlob)
		d.MatchedSensitiveTags = tags
		return d
	}
	if anyDeepPathMatch(sig.ChangedPaths, cfg.DeepPaths) {
		return decide(DepthDeep, ReasonDeepPathConfig)
	}

	if !sig.InputRead.Readable() {
		return decide(DepthDeep, ReasonInputUnreadable)
	}

	if base.SourceLines > maxChangedLinesLight {
		return decide(DepthDeep, ReasonChangedLinesOver)
	}
	if base.DistinctRoots >= minDistinctRootsForDeep {
		return decide(DepthDeep, ReasonRootDispersion)
	}

	if sig.PriorVerdictRiskHigh {
		return decide(DepthDeep, ReasonPriorHighVerdict)
	}
	if sig.NeedsHumanLabelPresent {
		return decide(DepthDeep, ReasonNeedsHumanLabel)
	}

	return decide(DepthLight, ReasonLightDefault)
}
