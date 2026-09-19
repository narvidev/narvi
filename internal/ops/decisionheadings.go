package ops

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
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
