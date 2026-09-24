package compat

import (
	"sort"
	"testing"
)

// ruleKind classifies how a rule-table row's severity behaves across the
// P2C/C2P columns, which is what a coherent coverage requirement has to
// be based ON: the design spec's guard 7 asks for "a MAJOR and a
// non-MAJOR case" per rule id, but roughly half the table's rows assign
// the SAME severity to both columns (row 1, "property removed", is
// MAJOR|MAJOR -- there is no non-MAJOR reading of row 1 to construct
// without misrepresenting the table). This test applies the guard's own
// intent -- catch a checker that can only ever emit one severity,
// direction-blind -- in the form the table actually supports:
//
//   - "dual" rows (the two columns disagree): require both a MAJOR-class
//     and a non-MAJOR-class corpus case.
//   - "fixed" rows (both columns agree on one severity): require a case
//     at exactly that severity (there is nothing else to require).
//   - "variable" rows (26, 27, 30: a wrapper whose severity is whatever
//     the recursive comparison finds): require at least one case, any
//     severity -- these three already get both a MAJOR and a low/PATCH
//     example in the corpus above for extra rigor, but the table itself
//     does not fix their severity the way it does the other rows.
type ruleKind int

const (
	kindDual ruleKind = iota
	kindFixed
	kindVariable
)

type ruleExpectation struct {
	kind ruleKind
	// fixedSeverity is only meaningful for kindFixed.
	fixedSeverity Severity
}

// ruleTable mirrors the design spec's own rule table (§6.3 design spec §2,
// plus this PR's four appended rows 42-45) exactly -- see COMPATIBILITY.md
// for the prose version. TestCompatibilityDocRuleIDsMatchCode (in
// compatibility_doc_test.go) separately asserts COMPATIBILITY.md's own
// rule table lists exactly this same id set, so the two can never drift
// apart silently.
var ruleTable = map[string]ruleExpectation{
	"1":  {kind: kindFixed, fixedSeverity: SeverityMajor},
	"2":  {kind: kindFixed, fixedSeverity: SeverityMinor},
	"3":  {kind: kindDual},
	"4":  {kind: kindDual},
	"5":  {kind: kindDual},
	"6":  {kind: kindFixed, fixedSeverity: SeverityMajor},
	"7":  {kind: kindDual},
	"8":  {kind: kindDual},
	"9":  {kind: kindDual},
	"10": {kind: kindDual},
	"11": {kind: kindDual},
	"12": {kind: kindDual},
	"13": {kind: kindDual},
	"14": {kind: kindFixed, fixedSeverity: SeverityMajor},
	"15": {kind: kindDual},
	"16": {kind: kindDual},
	"17": {kind: kindFixed, fixedSeverity: SeverityMajor},
	"18": {kind: kindDual},
	"19": {kind: kindDual},
	"20": {kind: kindDual},
	"21": {kind: kindDual},
	"22": {kind: kindFixed, fixedSeverity: SeverityMajor},
	"23": {kind: kindFixed, fixedSeverity: SeverityMinor},
	"24": {kind: kindFixed, fixedSeverity: SeverityMajor},
	"25": {kind: kindFixed, fixedSeverity: SeverityMajor},
	"26": {kind: kindVariable},
	"27": {kind: kindVariable},
	"28": {kind: kindFixed, fixedSeverity: SeverityMinor},
	"29": {kind: kindFixed, fixedSeverity: SeverityMajor},
	"30": {kind: kindVariable},
	"31": {kind: kindFixed, fixedSeverity: SeverityMajor},
	"32": {kind: kindFixed, fixedSeverity: SeverityMinor},
	"33": {kind: kindFixed, fixedSeverity: SeverityMajor},
	"34": {kind: kindFixed, fixedSeverity: SeverityMajor},
	"35": {kind: kindFixed, fixedSeverity: SeverityPatch},
	"36": {kind: kindFixed, fixedSeverity: SeverityFailClosed},
	"37": {kind: kindFixed, fixedSeverity: SeverityMajor},
	"38": {kind: kindFixed, fixedSeverity: SeverityMinor},
	"39": {kind: kindFixed, fixedSeverity: SeverityMajor},
	"40": {kind: kindFixed, fixedSeverity: SeverityMajor},
	"41": {kind: kindFixed, fixedSeverity: SeverityMinor},
	"42": {kind: kindDual},                                // C13
	"43": {kind: kindFixed, fixedSeverity: SeverityMajor}, // C15
	"44": {kind: kindFixed, fixedSeverity: SeverityMajor}, // C1
	"45": {kind: kindFixed, fixedSeverity: SeverityMajor}, // C19
}

// TestCorpusCoverage is guard 7's meta-test: every rule id in the closed
// table has at least one corpus case, of the severity/severities its own
// row actually supports (see ruleKind's doc comment), and every
// allowlisted keyword is exercised by at least one corpus case.
//
// C17: every data point this test checks against is derived by ACTUALLY
// RUNNING corpus/wholeSurfaceCorpus's own producer functions here, not by
// a parallel hard-coded list -- the previous version's record("31",
// SeverityMajor) et al. calls were pure decoration that could not notice
// their own backing fixture disappearing (a mutant that guts
// TestWholeSurfaceCorpus, or one of its cases, left this test's
// hard-coded records untouched and still green). Recomputing "seen" from
// the shared tables' own run() calls means a case that stops actually
// producing the finding it claims -- whether because the fixture broke or
// because the underlying rule broke -- fails HERE too, independent of
// whatever else is or isn't asserting on it.
func TestCorpusCoverage(t *testing.T) {
	type severitySet map[Severity]bool
	seen := map[string]severitySet{}
	record := func(ruleID string, sev Severity) {
		if seen[ruleID] == nil {
			seen[ruleID] = severitySet{}
		}
		seen[ruleID][sev] = true
	}

	for _, tc := range corpus {
		for _, want := range []*wantFinding{tc.p2cWant, tc.c2pWant, tc.fixed} {
			if want != nil {
				record(want.ruleID, want.severity)
			}
		}
	}

	// Rows exercised via whole-surface/manifest/routes comparisons rather
	// than DiffDef: run each wholeSurfaceCorpus case for real (the exact
	// same table TestWholeSurfaceCorpus asserts against) and record
	// whatever it actually produced -- not what it CLAIMS to produce.
	for _, tc := range wholeSurfaceCorpus {
		findings, err := tc.run()
		if err != nil {
			t.Errorf("wholeSurfaceCorpus %q: unexpected error re-running for coverage: %v", tc.name, err)
			continue
		}
		if !containsFinding(findings, tc.ruleID, tc.severity) {
			t.Errorf("wholeSurfaceCorpus %q: re-running it did not reproduce rule %s severity %s (got %v) -- coverage for this rule/severity is not real", tc.name, tc.ruleID, tc.severity, findings)
			continue
		}
		record(tc.ruleID, tc.severity)
	}

	var ids []string
	for id := range ruleTable {
		ids = append(ids, id)
	}
	sort.Strings(ids)

	for _, id := range ids {
		exp := ruleTable[id]
		got := seen[id]
		switch exp.kind {
		case kindFixed:
			if !got[exp.fixedSeverity] {
				t.Errorf("rule %s: no corpus case at its fixed severity %s (got %v)", id, exp.fixedSeverity, severitiesOf(got))
			}
		case kindDual:
			if !got[SeverityMajor] {
				t.Errorf("rule %s: no MAJOR corpus case (got %v)", id, severitiesOf(got))
			}
			hasNonMajor := got[SeverityMinor] || got[SeverityPatch]
			if !hasNonMajor {
				t.Errorf("rule %s: no non-MAJOR corpus case (got %v)", id, severitiesOf(got))
			}
		case kindVariable:
			if len(got) == 0 {
				t.Errorf("rule %s: no corpus case at all", id)
			}
		}
	}

	assertAllKeywordsCovered(t)
}

func severitiesOf(m map[Severity]bool) []string {
	var out []string
	for s := range m {
		out = append(out, s.String())
	}
	sort.Strings(out)
	return out
}

// assertAllKeywordsCovered walks every corpus fixture's base/head defs,
// PLUS every wholeSurfaceCorpus case's own declared keywordsUsed (derived
// from that fixture's own construction, not hard-coded separately from
// it -- C17 again: the old version force-set found["$schema"] etc. to
// true unconditionally, so those five keywords could never be reported
// as uncovered even if every fixture using them were deleted).
func assertAllKeywordsCovered(t *testing.T) {
	found := map[string]bool{}
	var walk func(node any)
	walk = func(node any) {
		obj, ok := node.(map[string]any)
		if !ok {
			if arr, ok := node.([]any); ok {
				for _, el := range arr {
					walk(el)
				}
			}
			return
		}
		for k, v := range obj {
			if allowedKeywords[k] {
				found[k] = true
			}
			walk(v)
		}
	}

	for _, tc := range corpus {
		walk(tc.baseDefs)
		walk(tc.headDefs)
	}
	for _, tc := range wholeSurfaceCorpus {
		for _, kw := range tc.keywordsUsed {
			found[kw] = true
		}
	}

	for _, kw := range AllowedKeywordNames() {
		if !found[kw] {
			t.Errorf("keyword %q is never exercised by any corpus fixture", kw)
		}
	}
}
