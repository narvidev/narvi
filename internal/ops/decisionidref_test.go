package ops

import (
	"os"
	"path/filepath"
	"testing"
)

// TestNoDecisionIDRefDrift is Y4 audit fix's own considered addition
// (decisionidref.go's own top doc comment explains what this can and
// cannot catch): fails when a real Go source comment cites a "D-NN"
// docs/DECISIONS.md id that does not exist as a real heading there --
// a typo, or a citation left behind after an entry is renumbered or
// removed. It runs as a plain `go test`, alongside TestNoSectionRefDrift
// and this package's other citation guards.
func TestNoDecisionIDRefDrift(t *testing.T) {
	t.Parallel()

	root := repoRoot(t)

	headings, err := ScanDecisionHeadings(root)
	if err != nil {
		t.Fatalf("ScanDecisionHeadings: %v", err)
	}

	refs, err := ScanDecisionIDRefs(root, []string{"internal", "cmd", "controlplane", "extension", "contracts"})
	if err != nil {
		t.Fatalf("ScanDecisionIDRefs: %v", err)
	}

	if bad := CheckDecisionIDRefs(refs, headings); len(bad) > 0 {
		for _, ref := range bad {
			t.Errorf("%s cites %q (%d time(s)), which docs/DECISIONS.md does not define as a heading. "+
				"This check cannot tell whether a citation of a REAL, existing D-NN nonetheless names "+
				"the wrong entry (see decisionidref.go's own doc comment) -- but a citation of an id "+
				"that does not exist at all is always wrong.", ref.File, ref.Cited, ref.Count)
		}
	}
}

// TestScanDecisionIDRefsAreFound guards the scanner itself, mirroring
// TestDecisionSymbolRefsAreFound's own reasoning (decisionsymbolref_test.go):
// a pattern that silently matches nothing would make the check above pass
// on an empty slice, indistinguishable from "every citation resolved".
// D-06/D-07 are cited from real Go source today (see automationdispatch.go,
// lineardispatch.go, dispatch.go), so this must find at least one.
func TestScanDecisionIDRefsAreFound(t *testing.T) {
	t.Parallel()

	refs, err := ScanDecisionIDRefs(repoRoot(t), []string{"internal", "cmd", "controlplane", "extension", "contracts"})
	if err != nil {
		t.Fatalf("ScanDecisionIDRefs: %v", err)
	}
	if len(refs) == 0 {
		t.Fatal("parsed zero \"D-NN\" citations from real Go source -- either the pattern stopped " +
			"matching (a citation style change?) or every D-NN citation was removed from source; " +
			"either way this check is now verifying nothing")
	}
}

// TestCheckDecisionIDRefs_Table is a synthetic, table-driven unit test over
// CheckDecisionIDRefs directly, isolated from the real repository content
// (TestNoDecisionIDRefDrift above already covers that).
func TestCheckDecisionIDRefs_Table(t *testing.T) {
	t.Parallel()

	headings := []int{1, 2, 3, 4, 5, 6, 7, 8}

	tests := []struct {
		name    string
		ref     DecisionIDRef
		wantBad bool
	}{
		// The two invalid Cited values below are ASSEMBLED (string
		// concatenation) rather than written as one contiguous literal --
		// this file lives under internal/, which TestNoDecisionIDRefDrift
		// above scans over the real tree, and a nonexistent id spelled out
		// contiguously here would itself be a real, unresolvable citation in
		// real source, making the fixture for the failing case also fail
		// the passing case (mirrors sectionref_test.go's own identical,
		// established convention and its own doc comment on why,
		// TestCheckSectionRefs_ReportsACitationTheSpecDoesNotDefine).
		{"resolves", DecisionIDRef{Number: 7, Cited: "D-07", File: "pkg/real.go", Count: 1}, false},
		{"resolves, another existing id", DecisionIDRef{Number: 6, Cited: "D-06", File: "pkg/real.go", Count: 1}, false},
		{"does not resolve: no such heading", DecisionIDRef{Number: 99, Cited: "D-" + "99", File: "pkg/typo.go", Count: 1}, true},
		{"does not resolve: zero", DecisionIDRef{Number: 0, Cited: "D-" + "0", File: "pkg/zero.go", Count: 1}, true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			bad := CheckDecisionIDRefs([]DecisionIDRef{tt.ref}, headings)
			if got := len(bad) > 0; got != tt.wantBad {
				t.Errorf("CheckDecisionIDRefs(%+v) bad=%v (%v), want bad=%v", tt.ref, got, bad, tt.wantBad)
			}
		})
	}
}

// TestScanDecisionIDRefs_WordBoundaryExcludesUnrelatedHyphenatedTokens
// proves the false-positive case this pattern was specifically chosen
// against (decisionIDRefPattern's own doc comment): an unrelated all-caps
// word immediately followed by a hyphen and a number must never be misread
// as a decision-id citation merely because it ends in a hyphen and a digit
// run -- this shape genuinely appears twice in this repository's own real
// source today (that doc comment names where), which is exactly why this
// pattern needed the word-boundary anchor in the first place.
func TestScanDecisionIDRefs_WordBoundaryExcludesUnrelatedHyphenatedTokens(t *testing.T) {
	t.Parallel()

	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, "internal", "pkg"), 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	content := "package pkg\n\n// ROUND-11 changed one side of this comparison, and D-07 (docs/DECISIONS.md) documents the actual decision.\nfunc f() {}\n"
	if err := os.WriteFile(filepath.Join(root, "internal", "pkg", "real.go"), []byte(content), 0o644); err != nil {
		t.Fatalf("write fixture file: %v", err)
	}

	refs, err := ScanDecisionIDRefs(root, []string{"internal"})
	if err != nil {
		t.Fatalf("ScanDecisionIDRefs: %v", err)
	}
	if len(refs) != 1 {
		t.Fatalf("ScanDecisionIDRefs found %d refs, want exactly 1 (D-07 only -- the fixture's own leading \"ROUND\" counter must not be misread as a second decision-id citation): %+v", len(refs), refs)
	}
	if refs[0].Number != 7 || refs[0].Cited != "D-07" {
		t.Errorf("ScanDecisionIDRefs = %+v, want Number=7 Cited=D-07", refs[0])
	}
}
