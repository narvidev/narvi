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
//
// D18: the id-set check alone is NOT what C22 actually needed -- C22's own
// failure mode was a SEVERITY drifting out of sync while the id stayed
// documented (goJSONSchema written up as PATCH in the doc while the code
// scored it MAJOR). So this test also parses each row's own P→C/C→P
// severity cells and cross-checks them against ruleTable's docP2C/docC2P
// (for kindDual rows) or fixedSeverity (kindFixed, both columns).
// kindVariable rows (26, 27, 30) are exempt from the severity check: their
// doc cells ("recurse", "same") never reduce to a fixed severity, which is
// exactly why the code models them as "variable" too.
func TestCompatibilityDocRuleIDsMatchCode(t *testing.T) {
	data, err := os.ReadFile("../../../contracts/COMPATIBILITY.md")
	if err != nil {
		t.Fatalf("read COMPATIBILITY.md: %v", err)
	}
	doc := string(data)

	rows := extractRuleTableRows(t, doc)
	if len(rows) == 0 {
		t.Fatal("found no rule-table rows in COMPATIBILITY.md -- did its own \"## The rule table\" markdown table move or get reformatted?")
	}

	codeIDs := map[string]bool{}
	for id := range ruleTable {
		codeIDs[id] = true
	}

	for id := range rows {
		if !codeIDs[id] {
			t.Errorf("COMPATIBILITY.md documents rule %s, but tools/contractscompat/compat's ruleTable (meta_test.go) has no entry for it", id)
		}
	}
	for id := range codeIDs {
		if _, ok := rows[id]; !ok {
			t.Errorf("tools/contractscompat/compat's ruleTable has rule %s, but COMPATIBILITY.md's rule table does not document it", id)
		}
	}

	for id, row := range rows {
		exp, ok := ruleTable[id]
		if !ok {
			continue // already reported above
		}
		switch exp.kind {
		case kindFixed:
			checkColumn(t, id, "P→C", row.p2c, exp.fixedSeverity)
			checkColumn(t, id, "C→P", row.c2p, exp.fixedSeverity)
		case kindDual:
			checkColumn(t, id, "P→C", row.p2c, exp.docP2C)
			checkColumn(t, id, "C→P", row.c2p, exp.docC2P)
		case kindVariable:
			// No fixed severity to check -- see this test's own doc
			// comment.
		}
	}
}

// checkColumn asserts that the severity parsed out of a doc cell
// (docSeverity, zero value meaning "no severity word found at all," e.g.
// an "n/a" cell) matches what ruleTable records for that rule/column.
func checkColumn(t *testing.T, ruleID, column string, docSeverity severityOrAbsent, want Severity) {
	t.Helper()
	if !docSeverity.present {
		return // "n/a," or otherwise no severity word in this cell
	}
	if docSeverity.value != want {
		t.Errorf("rule %s %s: COMPATIBILITY.md documents severity %s, but ruleTable (meta_test.go) records %s -- the doc and the code have drifted apart (this is C22's own failure mode)", ruleID, column, docSeverity.value, want)
	}
}

// docRuleRow is one parsed "## The rule table" data row: the P→C and C→P
// cell text, each reduced to the FIRST severity word
// (MAJOR/MINOR/PATCH/FAIL-CLOSED) it contains, if any.
type docRuleRow struct {
	p2c, c2p severityOrAbsent
}

type severityOrAbsent struct {
	value   Severity
	present bool
}

// ruleTableRowRE matches one markdown table data row under "## The rule
// table": "| 12 | Change text | P→C cell | C→P cell |" -- the leading "|"
// plus a bare integer in the first cell, then the remaining three cells.
// Header/separator rows ("| # | Change | ..." and "|---|---|...") never
// match (their first cell is not a bare integer), so this needs no
// separate header-skipping logic.
var ruleTableRowRE = regexp.MustCompile(`(?m)^\|\s*(\d+)\s*\|([^|]*)\|([^|]*)\|([^|]*)\|\s*$`)

// severityWordRE finds a severity token this checker's own Severity type
// can render (Severity.String() in types.go).
var severityWordRE = regexp.MustCompile(`MAJOR|MINOR|PATCH|FAIL-CLOSED`)

// parseSeverityCell reduces one P→C/C→P cell to its FIRST severity word,
// not every severity word the cell's prose happens to contain: several
// cells mention a SECOND severity that belongs to a different concept
// entirely -- row 38's P→C cell is "MINOR (also forces a MAJOR version
// bump, §4 -- ...)," where the "MAJOR" describes the forced VERSION BUMP
// discipline (§4), not this row's own compatibility severity (which is,
// and stays, MINOR). Every cell in the real table (verified by hand) puts
// its OWN severity first, so "first word" is the reliable signal; a
// literal "same" (rows 27, 37, 39 or similar shorthand for "same as the
// other column") is handled by extractRuleTableRows below, not here.
func parseSeverityCell(cell string) severityOrAbsent {
	w := severityWordRE.FindString(cell)
	switch w {
	case "MAJOR":
		return severityOrAbsent{SeverityMajor, true}
	case "MINOR":
		return severityOrAbsent{SeverityMinor, true}
	case "PATCH":
		return severityOrAbsent{SeverityPatch, true}
	case "FAIL-CLOSED":
		return severityOrAbsent{SeverityFailClosed, true}
	default:
		return severityOrAbsent{}
	}
}

func extractRuleTableRows(t *testing.T, doc string) map[string]docRuleRow {
	t.Helper()
	rows := map[string]docRuleRow{}
	for _, m := range ruleTableRowRE.FindAllStringSubmatch(doc, -1) {
		id, p2cCell, c2pCell := m[1], m[3], m[4]
		row := docRuleRow{
			p2c: parseSeverityCell(p2cCell),
			c2p: parseSeverityCell(c2pCell),
		}
		// "same" is this doc's own shorthand for "identical to the other
		// column" (rows 27, 37, 39, ...) -- resolve it once both cells of
		// the row have been parsed, rather than trying to teach the
		// per-cell parser about the OTHER cell.
		if strings.TrimSpace(c2pCell) == "same" {
			row.c2p = row.p2c
		}
		if strings.TrimSpace(p2cCell) == "same" {
			row.p2c = row.c2p
		}
		rows[id] = row
	}
	return rows
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
