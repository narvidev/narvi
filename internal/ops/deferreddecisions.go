package ops

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// DeferredDecision is one row of docs/DECISIONS.md's own "Deferred -- and
// what reopens each" table: a decision taken as a deferral, which leaves
// the open list without becoming settled.
//
// The field that matters is Trigger. A deferral whose reopen condition is
// empty is indistinguishable from an item nobody got round to, and that
// is the exact state the register exists to make impossible -- so this
// package refuses it at CI time rather than trusting an editor to
// remember.
type DeferredDecision struct {
	Subject  string
	Deferred string
	Trigger  string
}

// deferredTableHeading is the exact heading the table lives under. Keyed
// on the heading rather than on position so reordering the file cannot
// silently make this check scan nothing -- a scanner that finds no rows
// and reports success is the failure mode this whole package exists
// against.
const deferredTableHeading = "## Deferred — and what reopens each"

// LoadDeferredDecisions parses that one table. It returns an error when
// the heading is absent or the table under it holds no data row: both
// mean this check has stopped checking, which must fail loudly rather
// than pass quietly.
func LoadDeferredDecisions(root string) ([]DeferredDecision, error) {
	path := filepath.Join(root, "docs", "DECISIONS.md")
	raw, err := os.ReadFile(path) //nolint:gosec // repo-relative, fixed name
	if err != nil {
		return nil, fmt.Errorf("read %s: %w", path, err)
	}
	lines := strings.Split(string(raw), "\n")

	start := -1
	for i, line := range lines {
		if strings.TrimSpace(line) == deferredTableHeading {
			start = i
			break
		}
	}
	if start == -1 {
		return nil, fmt.Errorf("docs/DECISIONS.md: heading %q not found -- either it was renamed "+
			"(update deferredTableHeading with it) or the deferred table is gone, and this check "+
			"must not silently scan nothing", deferredTableHeading)
	}

	var out []DeferredDecision
	for _, line := range lines[start+1:] {
		trimmed := strings.TrimSpace(line)
		// A second "###" or "##" heading ends this table's own region.
		if strings.HasPrefix(trimmed, "##") {
			break
		}
		if !strings.HasPrefix(trimmed, "|") {
			continue
		}
		cells := splitDeferredRow(trimmed)
		if len(cells) != 3 {
			continue
		}
		if cells[0] == "Decision" || strings.HasPrefix(cells[0], "---") {
			continue
		}
		out = append(out, DeferredDecision{Subject: cells[0], Deferred: cells[1], Trigger: cells[2]})
	}
	if len(out) == 0 {
		return nil, fmt.Errorf("docs/DECISIONS.md: the deferred table under %q has no data row -- "+
			"an empty result here reads as \"nothing deferred\" and is far more likely to mean the "+
			"table's shape changed", deferredTableHeading)
	}
	return out, nil
}

// splitDeferredRow turns "| a | b | c |" into its three cells.
func splitDeferredRow(row string) []string {
	parts := strings.Split(strings.Trim(row, "|"), "|")
	out := make([]string, 0, len(parts))
	for _, p := range parts {
		out = append(out, strings.TrimSpace(p))
	}
	return out
}

// CheckDeferredTriggers returns the subjects whose reopen condition is
// missing or too short to be a condition at all.
//
// The length floor is deliberate and crude: it exists to refuse "TBD",
// "later" and "-" without pretending to judge whether a real sentence
// describes an evaluable condition. THIS CHECK CANNOT TELL WHETHER A
// CONDITION HAS FIRED -- it stops a deferral being recorded without one,
// and does not watch the world. Saying so here keeps the guard from
// being read as more than it is.
func CheckDeferredTriggers(decisions []DeferredDecision) []string {
	const minTriggerLen = 24
	var bad []string
	for _, d := range decisions {
		if len([]rune(d.Trigger)) < minTriggerLen {
			bad = append(bad, d.Subject)
		}
	}
	return bad
}
