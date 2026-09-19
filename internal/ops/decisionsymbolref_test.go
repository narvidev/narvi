package ops

import (
	"os"
	"path/filepath"
	"testing"
)

// TestDecisionSymbolRefsAreFound guards the scanner itself, mirroring
// TestDeferredDecisionsTableIsFound's own reasoning (deferreddecisions_test.go):
// a pattern that silently matches nothing reads as "every citation
// resolved" for the identical reason an unfound heading would. D-06/D-07
// were written specifically to carry this citation shape, so today's real
// docs/DECISIONS.md must yield at least one.
func TestDecisionSymbolRefsAreFound(t *testing.T) {
	t.Parallel()

	refs, err := ScanDecisionSymbolRefs(repoRoot(t))
	if err != nil {
		t.Fatalf("ScanDecisionSymbolRefs: %v", err)
	}
	if len(refs) == 0 {
		t.Fatal("parsed zero `Symbol` (path) citations from docs/DECISIONS.md -- either the pattern " +
			"stopped matching (a formatting change?) or D-06/D-07 lost their symbol citations; either " +
			"way this check is now verifying nothing")
	}
}

// TestDecisionSymbolRefsResolve is W8's own required guard: every symbol
// D-06 (or any other decision using this citation shape) points at must
// still exist, in the file it names, today.
func TestDecisionSymbolRefsResolve(t *testing.T) {
	t.Parallel()

	root := repoRoot(t)
	refs, err := ScanDecisionSymbolRefs(root)
	if err != nil {
		t.Fatalf("ScanDecisionSymbolRefs: %v", err)
	}
	if bad := CheckDecisionSymbolRefs(root, refs); len(bad) > 0 {
		t.Errorf("docs/DECISIONS.md cites a symbol that does not resolve:\n%s\n\nA decision record "+
			"that points at code must point at code that still exists -- fix the citation (the symbol "+
			"was renamed/moved) or fix the decision (it no longer describes what shipped).",
			joinLines(bad))
	}
}

func joinLines(lines []string) string {
	out := ""
	for i, l := range lines {
		if i > 0 {
			out += "\n"
		}
		out += "  " + l
	}
	return out
}

// TestCheckDecisionSymbolRefs_Table is a synthetic, table-driven unit test
// over CheckDecisionSymbolRefs directly -- isolated from the real
// docs/DECISIONS.md content (TestDecisionSymbolRefsResolve above already
// covers that), proving the resolve/does-not-resolve verdict for each
// individually-named degenerate input: a missing file, a missing symbol,
// and a genuinely resolving pair.
func TestCheckDecisionSymbolRefs_Table(t *testing.T) {
	t.Parallel()

	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, "pkg"), 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.WriteFile(filepath.Join(root, "pkg", "real.go"), []byte("package pkg\n\nfunc RealSymbol() {}\n"), 0o644); err != nil {
		t.Fatalf("write fixture file: %v", err)
	}

	tests := []struct {
		name    string
		ref     DecisionSymbolRef
		wantBad bool
	}{
		{"resolves", DecisionSymbolRef{Symbol: "RealSymbol", Path: "pkg/real.go", Line: 1}, false},
		{"missing file", DecisionSymbolRef{Symbol: "RealSymbol", Path: "pkg/nonexistent.go", Line: 2}, true},
		{"missing symbol in a real file", DecisionSymbolRef{Symbol: "NoSuchSymbol", Path: "pkg/real.go", Line: 3}, true},
		{"symbol as a substring only, not a whole word", DecisionSymbolRef{Symbol: "Real", Path: "pkg/real.go", Line: 4}, true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			bad := CheckDecisionSymbolRefs(root, []DecisionSymbolRef{tt.ref})
			if got := len(bad) > 0; got != tt.wantBad {
				t.Errorf("CheckDecisionSymbolRefs(%+v) bad=%v (%v), want bad=%v", tt.ref, got, bad, tt.wantBad)
			}
		})
	}
}

// TestScanDecisionSymbolRefs_ToleratesLineWrap proves the whitespace
// between the backtick-quoted symbol and its parenthesized path may
// include a markdown line-wrap (a literal newline) -- docs/DECISIONS.md
// hand-wraps prose, and a citation written on one logical line routinely
// has its own two halves pushed across a real line break by a later edit.
func TestScanDecisionSymbolRefs_ToleratesLineWrap(t *testing.T) {
	t.Parallel()

	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, "docs"), 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	content := "# Decisions\n\nSee `WrappedSymbol`\n(pkg/wrapped.go) for the full story.\n"
	if err := os.WriteFile(filepath.Join(root, "docs", "DECISIONS.md"), []byte(content), 0o644); err != nil {
		t.Fatalf("write fixture DECISIONS.md: %v", err)
	}

	refs, err := ScanDecisionSymbolRefs(root)
	if err != nil {
		t.Fatalf("ScanDecisionSymbolRefs: %v", err)
	}
	if len(refs) != 1 {
		t.Fatalf("ScanDecisionSymbolRefs found %d refs, want 1 (a citation split across a line wrap must still be found): %+v", len(refs), refs)
	}
	if refs[0].Symbol != "WrappedSymbol" || refs[0].Path != "pkg/wrapped.go" {
		t.Errorf("ScanDecisionSymbolRefs = %+v, want Symbol=WrappedSymbol Path=pkg/wrapped.go", refs[0])
	}
}
