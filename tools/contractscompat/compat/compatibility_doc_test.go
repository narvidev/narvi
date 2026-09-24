package compat

import (
	"os"
	"regexp"
	"strings"
	"testing"
)

// TestCompatibilityDocRuleIDsMatchCode is C22's doc guard: contracts/
// COMPATIBILITY.md's own "## The rule table" markdown table must list
// EXACTLY the same rule-id set this package's ruleTable (meta_test.go)
// enforces in code -- neither a doc-only row (never implemented) nor a
// code-only row (never documented) can land without this test noticing.
// Mirrors this repo's internal/ops doc-guard convention (sectionref.go
// and friends: bind a document's own claims to the code that has to keep
// them true, rather than trusting them to stay in sync by review alone).
func TestCompatibilityDocRuleIDsMatchCode(t *testing.T) {
	data, err := os.ReadFile("../../../contracts/COMPATIBILITY.md")
	if err != nil {
		t.Fatalf("read COMPATIBILITY.md: %v", err)
	}

	docIDs := extractRuleTableIDs(t, string(data))
	if len(docIDs) == 0 {
		t.Fatal("found no rule-table rows in COMPATIBILITY.md -- did its own \"## The rule table\" markdown table move or get reformatted?")
	}

	codeIDs := map[string]bool{}
	for id := range ruleTable {
		codeIDs[id] = true
	}

	for id := range docIDs {
		if !codeIDs[id] {
			t.Errorf("COMPATIBILITY.md documents rule %s, but tools/contractscompat/compat's ruleTable (meta_test.go) has no entry for it", id)
		}
	}
	for id := range codeIDs {
		if !docIDs[id] {
			t.Errorf("tools/contractscompat/compat's ruleTable has rule %s, but COMPATIBILITY.md's rule table does not document it", id)
		}
	}
}

// ruleTableRowRE matches one markdown table data row under "## The rule
// table": "| 12 | ... |" -- the leading "|" plus a bare integer in the
// first cell. Header/separator rows ("| # | Change | ..." and
// "|---|---|...") never match (their first cell is not a bare integer),
// so this needs no separate header-skipping logic.
var ruleTableRowRE = regexp.MustCompile(`(?m)^\|\s*(\d+)\s*\|`)

func extractRuleTableIDs(t *testing.T, doc string) map[string]bool {
	t.Helper()
	ids := map[string]bool{}
	for _, m := range ruleTableRowRE.FindAllStringSubmatch(doc, -1) {
		ids[m[1]] = true
	}
	return ids
}

// TestCompatibilityDocAllowlistMentionsGoJSONSchema pins C22's other
// concrete complaint: the allowlist prose in COMPATIBILITY.md must not
// silently drop back out of sync with keywords.go's own allowedKeywords
// (it did, for goJSONSchema, before this rewrite).
func TestCompatibilityDocAllowlistMentionsGoJSONSchema(t *testing.T) {
	data, err := os.ReadFile("../../../contracts/COMPATIBILITY.md")
	if err != nil {
		t.Fatalf("read COMPATIBILITY.md: %v", err)
	}
	doc := string(data)
	for _, kw := range AllowedKeywordNames() {
		if !strings.Contains(doc, kw) {
			t.Errorf("COMPATIBILITY.md's allowlist prose does not appear to mention keyword %q", kw)
		}
	}
}
