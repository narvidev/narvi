package ops

import (
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
)

// This file guards the mirror-image mistake sectionref.go's own
// TestNoSectionRefDrift teaches how to fix: a Go comment citing "Step N" at
// all, instead of the technical-plan section that actually governs the
// behavior. A Step number is the implementation plan's own row order -- it
// says WHEN something was built, never WHAT it must do -- and this plan has
// been renumbered before (sectionref.go's own history), so a Step-N
// citation frozen into source is a citation waiting to go stale the next
// time it is. §N.M is the durable one; this repo's own convention is that
// code cites only that, never the Step.
//
// # Telling a plan citation apart from local test narration
//
// A numbered step inside a test's own scenario or procedure comment
// ("// Step 1: the timer fires", "// --- Step 3: kill pod A") is not a
// citation of the plan's Step table at all -- it is narration of what THIS
// test does, at THIS step of ITS OWN sequence, and flagging it would just
// teach every future test author to stop writing readable scenario
// comments. The rule stepRefPattern/narrativeStepLine apply to separate the
// two: a narrative step names an action, always at the very start of a
// comment line (after only whitespace, "//", and an optional "---"
// separator) and always followed immediately by a colon introducing that
// action -- "Step 1: the timer fires", never "Step 1's own timer". A plan
// citation, by contrast, almost always names the Step's own content via a
// possessive ("Step 48's own X") or a parenthetical alongside other
// citation material ("(Step 59, §29.5)") -- shapes a colon-led narrative
// line never takes. stepref_test.go's own mutation tests pin both
// directions of this boundary: a real plan-style citation added to a
// source comment must fail this check, and a narrative "// Step 1:" line
// must not.
//
// # Three citation shapes, one pattern
//
// A full-text audit of this repo found real citations in three shapes:
// the plain "Step 21", the plural "Steps 32-34"/"Steps 32/33/34" (several
// ingress Steps cited together), and the parenthesis form "Step (17)"/
// "Steps (47, 58)". stepRefPattern below matches all three at once. The
// ONE known false-positive risk this widening accepts: a local, in-file
// numbered list that happens to use capitalized "Steps N" for its own
// items (e.g. "Steps 8-10 below" meaning list items 8 through 10, not
// plan Steps) would also match -- found exactly once in this repo
// (scmcredentials.go, fixed by lowercasing to "steps 8-10", which this
// pattern's case sensitivity already treats as ordinary English). That is
// judged rare and cheap enough to fix on sight that it does not clear the
// "fights its users" bar the singular narrative-colon case would have.
//
// The separator after "Step(s)" is REQUIRED (one or more of space or
// hyphen, never zero): an identifier like builtInPlanStep1ID has no
// separator at all and must never be mistaken for a citation. The hyphen
// is in the class because "pre-Step-90" shipped past this check on a
// branch that had just been swept -- the fourth distinct way a citation
// has evaded it (after the file extension, the root list, and the line
// wrap), and the reason the negative cases below are pinned as carefully
// as the positive ones.
//
// # The same citation wearing a different word
//
// The implementation plan's Step table is a table, so its rows also get
// cited as rows: "docs/IMPLEMENTATION_PLAN.md row 87", "row 90's own
// screen". That is the identical non-durable reference -- a position in a
// table this project has renumbered before -- and stepRefPattern cannot see
// any of it. Seven such citations were found in the SPA after every
// Step-worded one had been swept, one of them rendered on screen to
// operators ("Not part of this Step. ... is docs/IMPLEMENTATION_PLAN.md
// row 90's own screen"), which is how the blind spot surfaced.
//
// Widening stepRefPattern to `row\s+\d+` is the obvious fix and it is the
// wrong one: it matched 222 lines, and almost every one of them cites a row
// of the TECHNICAL plan's own §13.3 RBAC table ("§13.3 row 1", "row 6,
// admin-only") -- exactly the durable citation this convention asks for.
// The rule that actually separates the two is simpler and needs no counting
// of rows: source code has no business citing the implementation plan AT
// ALL, by Step, by row, or by name. §N.M is the durable reference; the
// implementation plan is a schedule. planDocRefPattern below enforces that
// directly.
var stepRefPattern = regexp.MustCompile(`Steps?[\s-]+\(?\d+`)

// narrativeStepLine matches a numbered step at the start of a comment line
// that immediately introduces a local action with a colon -- see this
// file's own top doc comment for why that shape is exempt.
var narrativeStepLine = regexp.MustCompile(`^\s*//\s*(?:-{2,}\s*)?Step\s+\d+[a-z]?:\s`)

// wrappedStepRefHead/wrappedStepRefTail catch a citation the line-based
// stepRefPattern above cannot see, because a comment wrapped between the word
// and its number. The head must END with Step/Steps (nothing after it on that
// line); the tail must BEGIN with an optional comment marker and then the
// number. Deliberately narrow: a line merely ending in the English word "step"
// followed by an unrelated numbered line is the false positive to avoid, so the
// head requires the capitalised form the citation convention actually uses.
var (
	wrappedStepRefHead = regexp.MustCompile(`\bSteps?\s*$`)
	wrappedStepRefTail = regexp.MustCompile(`^\s*(?://+|\*)?\s*\(?\d`)
)

// planDocRefPattern matches any mention of the implementation plan's own
// filename in source. Deliberately the whole name and nothing cleverer:
// there is no legitimate reason for code to point at the schedule, so the
// check does not have to distinguish a row citation from a bare reference.
var planDocRefPattern = regexp.MustCompile(`IMPLEMENTATION_PLAN`)

// stepRefCheckExemptFiles names the files that document this very
// convention (and the historical incident that motivated it,
// sectionref.go's own doc comment) in prose, rather than citing the plan
// for the behavior of the code around them. They legitimately say "Step N"
// while explaining why nothing else in the tree should.
var stepRefCheckExemptFiles = map[string]bool{
	"internal/ops/sectionref.go":       true,
	"internal/ops/sectionref_test.go":  true,
	"internal/ops/stepref.go":          true,
	"internal/ops/stepref_test.go":     true,
	"internal/ops/stepref_pin.go":      true,
	"internal/ops/stepref_pin_test.go": true,
}

// stepRefScanExtensions are the source extensions CheckStepRefs walks.
// The convention is about what the SOURCE says, not what language it is
// written in: a "Step 62" citation rendered into a settings screen misleads
// an operator exactly as much as one in a Go comment, and .ts/.tsx use the
// same "//" comment marker narrativeStepLine already keys on.
var stepRefScanExtensions = map[string]bool{
	".go":  true,
	".ts":  true,
	".tsx": true,
}

// StepRef is one disallowed "Step N" citation this check found.
type StepRef struct {
	File string
	Line int
	Text string
}

// CheckStepRefs scans every source file under the given roots for a
// "Step N" citation that is not local test-scenario narration
// (narrativeStepLine) and not inside one of stepRefCheckExemptFiles,
// returning one StepRef per occurrence, sorted for a stable failure
// message. A nil/empty result means the scanned roots cite only
// technical-plan sections, never implementation Steps -- the CI-passing
// state.
//
// "Source file" means the extensions in stepRefScanExtensions, and the
// scanned roots are whatever the caller passes -- both matter, and both
// have been wrong here. This check shipped scanning only ".go", under
// roots that did not include the SPA at all, while its own doc comment
// claimed a clean result meant "the tree" cited only sections. It did not:
// web/src had accumulated dozens of Step citations, one of them rendered
// on screen to operators, all of them invisible to CI and all of them
// green. A check validates exactly what it scans, so the claim next to it
// must never describe more than that.
func CheckStepRefs(root string, scanDirs []string) ([]StepRef, error) {
	var out []StepRef
	for _, dir := range scanDirs {
		base := filepath.Join(root, dir)
		if _, err := os.Stat(base); os.IsNotExist(err) {
			continue
		}
		err := filepath.WalkDir(base, func(path string, d fs.DirEntry, err error) error {
			if err != nil {
				return err
			}
			if d.IsDir() || !stepRefScanExtensions[filepath.Ext(path)] {
				return nil
			}
			rel, relErr := filepath.Rel(root, path)
			if relErr != nil {
				rel = path
			}
			rel = filepath.ToSlash(rel)
			if stepRefCheckExemptFiles[rel] {
				return nil
			}
			raw, readErr := os.ReadFile(path) //nolint:gosec // walking a repo-relative tree
			if readErr != nil {
				return readErr
			}
			lines := strings.Split(string(raw), "\n")
			for i, line := range lines {
				if stepRefPattern.MatchString(line) {
					if narrativeStepLine.MatchString(line) {
						continue
					}
					out = append(out, StepRef{File: rel, Line: i + 1, Text: strings.TrimSpace(line)})
					continue
				}
				// A citation split across a line break: "Step" ends one line
				// and "88)" begins the next. Matching line by line sees
				// neither half, and comments in this repo wrap at 80 columns
				// constantly, so this is the common way a citation survives
				// rather than an exotic one -- ten of them were found repo-wide
				// after every single-line citation had been swept, including
				// three the SPA sweep had reported as complete. Reported at the
				// line carrying the word, which is where a reader looks.
				if i+1 < len(lines) && wrappedStepRefHead.MatchString(line) && wrappedStepRefTail.MatchString(lines[i+1]) {
					out = append(out, StepRef{File: rel, Line: i + 1, Text: strings.TrimSpace(line) + " " + strings.TrimSpace(lines[i+1])})
				}
			}
			for i, line := range strings.Split(string(raw), "\n") {
				if !planDocRefPattern.MatchString(line) {
					continue
				}
				out = append(out, StepRef{File: rel, Line: i + 1, Text: strings.TrimSpace(line)})
			}
			return nil
		})
		if err != nil {
			return nil, fmt.Errorf("walk %s: %w", dir, err)
		}
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].File != out[j].File {
			return out[i].File < out[j].File
		}
		return out[i].Line < out[j].Line
	})
	return out, nil
}

//
// # The colon form is this check's real blind spot, with evidence
//
// Narrative comments number their own local procedure -- "// Step 1: the
// timer fires", "// --- Step 3: kill pod A" -- and a plan citation can wear
// the same colon: "// Step 48: is the sentinel-auto-fix flow even a
// CANDIDATE". Excluding the colon form keeps the narrative ones quiet at the
// cost of missing citations shaped like them, and three genuine ones survived
// the sweep that way (Step 48 in httpapi/reviewverdict.go, Step 59 in
// domain/authz/authorize.go, Step 53 in opencodeproc/spawn_test.go -- all
// verified against the plan's own rows and rewritten to §17.6, §29 and §25.1).
// Separating the two needs the citation's TOPIC, not its shape, which is
// semantics and outside what this check can decide -- the same boundary
// docs/guides/README.md draws for its siblings. Stated here so the next reader
// knows the check is a floor, not a proof.
