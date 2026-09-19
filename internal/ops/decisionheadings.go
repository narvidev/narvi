package ops

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
)

// This file closes a gap a real merge exposed: two branches each appended
// new "### D-NN" entries to docs/DECISIONS.md, and the merge that combined
// them kept every body paragraph but dropped one heading line -- so the
// register's numbering silently jumped (D-04 straight to D-06) and one
// entry's three paragraphs read as an unheaded tail of the entry before it.
//
// This repository already had a merge-safety net for this file: a
// pipe-count discipline over its tables, adopted after an earlier bad merge
// produced seven-cell rows. That guard was run by hand on this exact merge
// and found nothing wrong, because it was right -- no table lost a cell.
// The casualty was a "### D-NN" PROSE heading, a shape a cell count
// structurally cannot see. LoadDeferredDecisions/ScanDecisionSymbolRefs
// (deferreddecisions.go, decisionsymbolref.go) already bind two other
// pieces of this same file to something CI can check instead of trusting
// an editor's own read-through; this is the third: the headings that make
// "D-07" a citation pointing at exactly one entry, rather than zero or two.
//
// It cannot tell whether a heading's OWN TITLE still matches its body --
// that is the citation-id-vs-content problem Y4 names, a different and
// harder claim. It can tell whether the numbering itself -- present,
// unique, contiguous, in order -- is intact, which is exactly the shape
// this merge broke.

// decisionHeadingPattern matches this file's own "### D-NN --- title"
// heading line and captures NN.
var decisionHeadingPattern = regexp.MustCompile(`(?m)^### D-(\d+)\s`)

// ScanDecisionHeadings returns every "### D-NN" heading number in
// docs/DECISIONS.md, in the order each appears in the file.
func ScanDecisionHeadings(root string) ([]int, error) {
	path := filepath.Join(root, "docs", "DECISIONS.md")
	raw, err := os.ReadFile(path) //nolint:gosec // repo-relative, fixed name
	if err != nil {
		return nil, fmt.Errorf("read %s: %w", path, err)
	}

	var out []int
	for _, m := range decisionHeadingPattern.FindAllStringSubmatch(string(raw), -1) {
		n, convErr := strconv.Atoi(m[1])
		if convErr != nil {
			// A regexp-captured run of digits that fails to parse as an int
			// would mean the pattern above and strconv disagree about what
			// a digit is -- not a real DECISIONS.md failure mode, but
			// surfaced rather than swallowed if it ever happens.
			return nil, fmt.Errorf("docs/DECISIONS.md: heading \"### D-%s\": %w", m[1], convErr)
		}
		out = append(out, n)
	}
	return out, nil
}

// CheckDecisionHeadingsContiguous verifies the D-NN headings, in the order
// they appear in the file, are exactly 1..len(headings) -- no gap, no
// duplicate, no entry out of order. Returns one human-readable message per
// position that breaks that promise, sorted by the order the break was
// found so the first divergence from the file's own top is always first.
//
// # Its own blind spot, and what closes it below
//
// This compares each heading's own POSITION against its number -- it has
// no notion of how many entries SHOULD exist, only that whatever IS
// present counts up from one with no gap. That is enough to catch a
// heading lost from the middle (every heading after the gap now
// mismatches its own position) but not the highest-numbered heading: lose
// it, and 1..N-1 remains, by itself, a perfectly contiguous run starting
// at 1 -- indistinguishable from "every heading intact" by a check that
// only ever looks at relative position. CheckDecisionHeadingsCoverReferencedIDs
// below closes exactly that gap, by cross-checking against a second place
// the register already names each entry from, rather than hard-coding how
// many headings there should be.
func CheckDecisionHeadingsContiguous(headings []int) []string {
	var bad []string
	seen := make(map[int]int, len(headings)) // value -> first 1-based position it appeared at
	for i, n := range headings {
		pos := i + 1
		want := pos
		if n != want {
			bad = append(bad, fmt.Sprintf(
				"position %d in file order is headed \"### D-%02d\", want \"### D-%02d\" -- "+
					"the D-NN headings must run 1..N with no gap, duplicate, or reordering",
				pos, n, want))
		}
		if firstPos, dup := seen[n]; dup {
			bad = append(bad, fmt.Sprintf(
				"\"### D-%02d\" appears twice, at position %d and position %d", n, firstPos, pos))
		} else {
			seen[n] = pos
		}
	}
	return bad
}

// ReferencedDecisionIDs extracts every "D-NN" id cited inside the Deferred
// table's own Subject cells (LoadDeferredDecisions, deferreddecisions.go)
// -- e.g. the trailing "(D-08)" in "A dispatch drop from a budget-exhausted
// delivery is visible, but not retried or reconciled (D-08)". Reuses
// decisionIDRefPattern (decisionidref.go) rather than a second pattern, so
// the two files agree on what a "D-NN" citation looks like. Not every
// deferred row names one -- a row recorded before a decision was ever
// numbered, or one that never gets one, cites nothing -- so callers must
// not treat this as exhaustive over every "### D-NN" heading in the file,
// only as a set that, whatever it contains, had better each still resolve
// to a real heading.
func ReferencedDecisionIDs(decisions []DeferredDecision) []int {
	seen := make(map[int]bool)
	var out []int
	for _, d := range decisions {
		for _, m := range decisionIDRefPattern.FindAllStringSubmatch(d.Subject, -1) {
			n, convErr := strconv.Atoi(m[1])
			if convErr != nil {
				continue // not reachable given \d+, mirrors ScanDecisionIDRefs' own identical guard
			}
			if !seen[n] {
				seen[n] = true
				out = append(out, n)
			}
		}
	}
	return out
}

// CheckDecisionHeadingsCoverReferencedIDs is CheckDecisionHeadingsContiguous's
// own closing half (see that function's own doc comment for the blind
// spot this exists against): every id in referenced (ReferencedDecisionIDs,
// ordinarily the Deferred table's own "(D-NN)" citations) must resolve to
// a real "### D-NN" heading among headings. A referenced id with no
// matching heading means the heading was lost -- or never written -- and
// this reports it BY NUMBER, which is exactly what a purely
// position-based comparison cannot do for the file's own highest-numbered
// entry. Deliberately does not attempt the reverse (that every heading is
// itself referenced back) -- most headings, adopted ones especially, are
// never cited from the Deferred table at all, and requiring one would be
// a fabricated obligation this file's own real content does not carry.
func CheckDecisionHeadingsCoverReferencedIDs(headings []int, referenced []int) []string {
	have := make(map[int]bool, len(headings))
	for _, n := range headings {
		have[n] = true
	}
	var bad []string
	for _, n := range referenced {
		if !have[n] {
			bad = append(bad, fmt.Sprintf(
				"the Deferred table cites \"(D-%02d)\" but docs/DECISIONS.md has no \"### D-%02d\" "+
					"heading -- the heading was lost (or never written)", n, n))
		}
	}
	sort.Strings(bad)
	return bad
}
