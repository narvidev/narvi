package ops

import (
	"os"
	"path/filepath"
	"testing"
)

// TestDecisionHeadingsAreFound guards the scanner itself, mirroring
// TestDeferredDecisionsTableIsFound's own reasoning (deferreddecisions_test.go)
// and TestDecisionSymbolRefsAreFound's (decisionsymbolref_test.go): a
// pattern that silently matches nothing would make the contiguity check
// below pass on an empty slice, which is indistinguishable from "every
// heading intact" for exactly the reason this whole package exists to
// refuse.
func TestDecisionHeadingsAreFound(t *testing.T) {
	t.Parallel()

	headings, err := ScanDecisionHeadings(repoRoot(t))
	if err != nil {
		t.Fatalf("ScanDecisionHeadings: %v", err)
	}
	if len(headings) == 0 {
		t.Fatal("parsed zero \"### D-NN\" headings from docs/DECISIONS.md -- either the pattern " +
			"stopped matching (a heading format change?) or every decision entry lost its heading; " +
			"either way this check is now verifying nothing")
	}
}

// TestDecisionHeadingsContiguous is the guard Y1 asked for: the real
// docs/DECISIONS.md's own "### D-NN" headings, in file order, must be
// present, unique, and contiguous starting at D-01. A merge (or any other
// edit) that keeps an entry's body paragraphs but drops its own heading
// line -- the exact defect this check was written against -- makes the
// following heading's position mismatch its number, and fails here.
func TestDecisionHeadingsContiguous(t *testing.T) {
	t.Parallel()

	root := repoRoot(t)
	headings, err := ScanDecisionHeadings(root)
	if err != nil {
		t.Fatalf("ScanDecisionHeadings: %v", err)
	}
	if bad := CheckDecisionHeadingsContiguous(headings); len(bad) > 0 {
		t.Errorf("docs/DECISIONS.md: the \"### D-NN\" headings are not present/unique/contiguous:\n%s\n\n"+
			"A citation like \"see D-07\" only means something if exactly one entry is headed D-07 -- "+
			"restore the missing heading, or renumber, so the sequence runs 1..N with no gap or "+
			"duplicate.", joinLines(bad))
	}
}

// TestCheckDecisionHeadingsContiguous_Table is a synthetic, table-driven
// unit test over CheckDecisionHeadingsContiguous directly, isolated from
// the real docs/DECISIONS.md content (TestDecisionHeadingsContiguous above
// already covers that) -- proving the verdict for each individually-named
// degenerate input, including the exact shape the round-5 merge produced
// (a heading dropped from the middle of the sequence).
func TestCheckDecisionHeadingsContiguous_Table(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name     string
		headings []int
		wantBad  bool
	}{
		{"empty", nil, false},
		{"single, valid", []int{1}, false},
		{"contiguous run", []int{1, 2, 3, 4, 5}, false},
		{"gap in the middle -- the round-5 merge's own defect (D-05 lost between D-04 and D-06)",
			[]int{1, 2, 3, 4, 6, 7, 8}, true},
		{"duplicate heading", []int{1, 2, 3, 3, 5}, true},
		{"out of order", []int{1, 2, 4, 3, 5}, true},
		{"does not start at 1", []int{2, 3, 4}, true},
		{"gap at the very end", []int{1, 2, 3, 5}, true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			bad := CheckDecisionHeadingsContiguous(tt.headings)
			if got := len(bad) > 0; got != tt.wantBad {
				t.Errorf("CheckDecisionHeadingsContiguous(%v) bad=%v (%v), want bad=%v",
					tt.headings, got, bad, tt.wantBad)
			}
		})
	}
}

// TestScanDecisionHeadings_OrderAndParse proves ScanDecisionHeadings reads
// numbers in file order (not sorted) and tolerates the real heading's own
// trailing " -- **ADOPTED/DEFERRED ...** -- ..." suffix, so the contiguity
// check above is exercising the actual parse, not a simplified fixture.
func TestScanDecisionHeadings_OrderAndParse(t *testing.T) {
	t.Parallel()

	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, "docs"), 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	content := "# Open decisions\n\n" +
		"### D-01 — First — **ADOPTED 2026-01-01**\n\nbody\n\n" +
		"### D-03 — Third, out of order in this fixture on purpose — **DEFERRED 2026-01-01**\n\nbody\n\n" +
		"### D-02 — Second — **ADOPTED 2026-01-01**\n\nbody\n"
	if err := os.WriteFile(filepath.Join(root, "docs", "DECISIONS.md"), []byte(content), 0o644); err != nil {
		t.Fatalf("write fixture DECISIONS.md: %v", err)
	}

	headings, err := ScanDecisionHeadings(root)
	if err != nil {
		t.Fatalf("ScanDecisionHeadings: %v", err)
	}
	want := []int{1, 3, 2}
	if len(headings) != len(want) {
		t.Fatalf("ScanDecisionHeadings = %v, want %v", headings, want)
	}
	for i := range want {
		if headings[i] != want[i] {
			t.Errorf("ScanDecisionHeadings[%d] = %d, want %d (headings must be returned in FILE order)", i, headings[i], want[i])
		}
	}

	// This fixture's own out-of-file-order 1,3,2 sequence must itself be
	// caught as non-contiguous, proving the two functions compose the way
	// the real guard test above relies on.
	if bad := CheckDecisionHeadingsContiguous(headings); len(bad) == 0 {
		t.Error("CheckDecisionHeadingsContiguous(1,3,2) reported no problem, want at least one")
	}
}
