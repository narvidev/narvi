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

// This file is the OTHER half of Step 128 (docs/IMPLEMENTATION_PLAN.md's own
// row): TestNoStepRefInSource (stepref.go) BANS a "Step N" citation from
// internal/, cmd/, controlplane/, extension/, contracts/ and web/src, on the
// reasoning that a Step number is the plan's own row order and code should
// cite the technical-plan section (§N.M) that actually governs the behavior
// instead. That reasoning does not extend to this file's roots --
// migrations/, docs/guides/, docs/runbooks/, deploy/ and .github/. A
// migration's header comment recording "this table is Step 62's own review-
// verdict persistence work" is not a live reference standing in for a
// section citation the author should have written instead; it is a
// historical fact about when a schema change landed, the same thing a git
// blame or a changelog entry records, and it is exactly as legitimate there
// as a CHANGELOG entry citing a version number that later got yanked would
// remain legitimate after the yank. Banning it would mean deleting or
// rewriting Step 128's own count of these citations (213 verified by its
// sweep, up from the row's original 183/178 estimates once every wrapped-
// line and non-"//"-comment form was found) for no benefit: the citation was
// never standing in for a section, because there usually is no single
// section that names "this specific migration landed in this specific PR."
//
// What DOES carry forward from TestNoStepRefInSource's own motivating
// incident (docs/IMPLEMENTATION_PLAN.md has been renumbered before, more
// than once -- see stepref.go's own doc comment and this Step's own PR
// description for the citations Step 128's sweep found already broken by an
// earlier renumber) is the actual risk: a Step number is a row position, and
// a later renumber can silently repoint an old, once-correct citation at a
// completely different row. Banning cannot fix that (there would be nothing
// left to check), and ignoring it is how Step 128 found ~213 citations that
// nothing had verified since whenever each was written. So this file
// implements a different assertion than a ban: a PIN. Every (file, Step
// number) pair Step 128's sweep verified against the plan is recorded in
// stepRefPins (stepref_pin_test.go) together with the row title it named at
// verification time; TestStepRefsPinnedToPlanRows re-derives both sides
// fresh on every run -- the live plan's current title for that row number,
// and the live set of citations still present under these roots -- and
// fails the moment either drifts from the pin: a renumber that changes what
// a pinned number now names, or a citation added or repointed afterward
// that nobody pinned.
//
// The pin key is (file, number), not number alone. A global "Step 74 means
// this title" pin cannot catch a citation that gets edited to name a
// DIFFERENT, already-valid-elsewhere number by mistake -- e.g. a docker/
// egress migration's own "Step 74" typo'd to "Step 75" (a real row,
// "config/data seeding", already correctly pinned for a different file) --
// because the number still resolves and still matches ITS pin, just not the
// citation's own file. Step 128's own mutation-verification (this Step's PR
// description) found exactly this gap in an earlier, number-only draft of
// this file before it shipped: mutating one citation's number to another
// real row's number passed silently. Keying by (file, number) closes it --
// the mutated pair has no entry, so it fails as unpinned instead.
//
// This could not have been TestNoStepRefInSource itself with a widened root
// list: that function's entire contract is "found any citation at all under
// these roots" (its own doc comment: "A nil/empty result means the scanned
// roots cite only technical-plan sections"), which is a ban, and a pin is
// the opposite assertion -- finding a citation here is fine, PROVIDED it
// still resolves correctly. Reusing the same function for both would mean
// CheckStepRefs takes on a mode flag or a defined-set parameter it has no
// other reason to carry, in service of two check families whose failure
// conditions are opposite of each other. A new file, reusing what
// CheckStepRefs's own regexes already teach about the citation shapes this
// repo uses, is the honest boundary.

// stepRefPinScanDirs are Step 128's own swept roots: schema migrations
// recording which Step introduced a change, and the ops surfaces (guides,
// runbooks, deploy manifests, CI workflow comments) that describe shipped
// behavior by the Step that built it.
var stepRefPinScanDirs = []string{"migrations", "docs/guides", "docs/runbooks", "deploy", ".github"}

// stepRefPinScanExtensions is deliberately wider than stepRefScanExtensions
// (stepref.go's own Go/TypeScript pair): the pin roots are SQL comments,
// YAML/JSON manifests, Markdown prose and Dockerfiles, and Step 128's sweep
// found real citations in every one of those shapes -- including a file
// with no extension at all (deploy/sandbox-image/Dockerfile). ".go" stays in
// the set too: migrations/embed.go lives under a pinned root and could
// legitimately grow a citation later.
var stepRefPinScanExtensions = map[string]bool{
	".sql":  true,
	".md":   true,
	".yml":  true,
	".yaml": true,
	".json": true,
	".go":   true,
	"":      true,
}

// stepRefCitePattern is stepRefPattern (stepref.go) with the number CAPTURED
// instead of merely detected, and widened to swallow a joined run in one
// match ("Steps 32/33/34", "Steps 55-56") so splitStepCitation can expand it.
var stepRefCitePattern = regexp.MustCompile(`\bSteps?[\s-]+\(?(\d+[a-z]?(?:[/\-,]\s*\d+[a-z]?)*)\)?`)

// stepRefJoinPattern is the separator between numbers in a joined citation.
var stepRefJoinPattern = regexp.MustCompile(`[/\-,]\s*`)

// stepRefPinNarrativeLine generalizes narrativeStepLine (stepref.go) to the
// comment markers the pin roots actually use -- "--" (SQL), "#" (YAML,
// Dockerfile) -- in addition to "//"/"*", plus a bare Markdown line with no
// marker at all. A runbook is as entitled to write "Step 1: reboot the pod"
// as a Go test is to write "Step 1: the timer fires", for the identical
// reason stepref.go's own top doc comment gives.
var stepRefPinNarrativeLine = regexp.MustCompile(`^\s*(?:-{2,}|#+|//+|\*)?\s*Step\s+\d+[a-z]?:\s`)

// stepRefPinWrappedHead/Tail are wrappedStepRefHead/Tail (stepref.go)
// generalized the same way, for a citation whose comment wraps mid-word
// under a non-Go marker. Found live: deploy/sandbox-image/Dockerfile's own
// "... toolchain in images", Step" / "# 74) is what actually lands it" --
// invisible to the "//"/"*"-only tail stepref.go uses, because a Dockerfile
// comment marker is "#".
var (
	stepRefPinWrappedHead = regexp.MustCompile(`\bSteps?\s*$`)
	stepRefPinWrappedTail = regexp.MustCompile(`^\s*(?:-{2,}|#+|//+|\*)?\s*\(?(\d+[a-z]?(?:[/\-,]\s*\d+[a-z]?)*)`)
)

// planStepRowPattern matches one row of docs/IMPLEMENTATION_PLAN.md's Step
// table: "| 74 | title | content | ref |" or "| 9 ∥ | title | ... |" -- the
// parallel-marker and any sub-lettered suffix on the number are both
// optional and not captured into the row number itself (no row's own number
// carries a letter; see stepRefPinBaseNumber for why a CITATION's letter
// suffix is stripped instead of looked up directly).
var planStepRowPattern = regexp.MustCompile(`(?m)^\|\s*(\d+)\s*∥?\s*\|\s*([^|]+?)\s*\|`)

// StepCitation is one "Step N" (or one member of a joined "Steps N/M/O")
// citation found under stepRefPinScanDirs, already reduced to the base plan
// row it names.
type StepCitation struct {
	File string
	Line int
	Num  string
	Text string
}

// stepRefPinBaseNumber strips a trailing sub-PR letter down to the plan row
// it names. "Step 73a"/"Step 73b" cite sub-parts of the SAME row -- Step
// 73's own content notes it shipped as two corrected sub-parts -- and Step
// 43's own title spells out the identical convention explicitly: "(3 PRs --
// (a)+(b)+(c) all shipped)". No row in docs/IMPLEMENTATION_PLAN.md is
// itself numbered with a trailing letter (planStepRowPattern's own capture
// never produces one), so a letter suffix on a CITATION is always this
// sub-PR shorthand, never a distinct row to resolve separately.
func stepRefPinBaseNumber(numstr string) string {
	return strings.TrimRight(numstr, "abcdefghijklmnopqrstuvwxyz")
}

// splitStepCitation expands a possibly-joined citation ("32/33/34",
// "55-56") into one StepCitation per member, each reduced to its base row.
func splitStepCitation(file string, line int, numstr, text string) []StepCitation {
	parts := stepRefJoinPattern.Split(numstr, -1)
	out := make([]StepCitation, 0, len(parts))
	for _, p := range parts {
		out = append(out, StepCitation{File: file, Line: line, Num: stepRefPinBaseNumber(p), Text: text})
	}
	return out
}

// LoadPlanStepTitles parses docs/IMPLEMENTATION_PLAN.md's own Step table and
// returns row number (e.g. "74") -> title, exactly as printed in the
// table's second column.
func LoadPlanStepTitles(root string) (map[string]string, error) {
	path := filepath.Join(root, "docs", "IMPLEMENTATION_PLAN.md")
	raw, err := os.ReadFile(path) //nolint:gosec // a repo-relative doc path, not user input
	if err != nil {
		return nil, fmt.Errorf("read implementation plan: %w", err)
	}
	out := make(map[string]string)
	for _, m := range planStepRowPattern.FindAllStringSubmatch(string(raw), -1) {
		out[m[1]] = m[2]
	}
	return out, nil
}

// ScanStepCitationsForPinning walks the given roots and returns every Step
// citation found, expanded to one entry per plan row named (a joined
// "Steps 32/33/34" yields three). Unlike CheckStepRefs (stepref.go), finding
// a citation here is not itself a failure -- the caller (
// TestStepRefsPinnedToPlanRows) decides that by comparing against the
// pinned set -- so this only excludes local narrative ("Step 1: ...")
// scenario lines, never a real plan citation.
func ScanStepCitationsForPinning(root string, scanDirs []string) ([]StepCitation, error) {
	var out []StepCitation
	for _, dir := range scanDirs {
		base := filepath.Join(root, dir)
		if _, err := os.Stat(base); os.IsNotExist(err) {
			continue
		}
		err := filepath.WalkDir(base, func(path string, d fs.DirEntry, err error) error {
			if err != nil {
				return err
			}
			if d.IsDir() || !stepRefPinScanExtensions[filepath.Ext(path)] {
				return nil
			}
			rel, relErr := filepath.Rel(root, path)
			if relErr != nil {
				rel = path
			}
			rel = filepath.ToSlash(rel)
			raw, readErr := os.ReadFile(path) //nolint:gosec // walking a repo-relative tree
			if readErr != nil {
				return readErr
			}
			lines := strings.Split(string(raw), "\n")
			for i, line := range lines {
				if stepRefPinNarrativeLine.MatchString(line) {
					continue
				}
				for _, m := range stepRefCitePattern.FindAllStringSubmatch(line, -1) {
					out = append(out, splitStepCitation(rel, i+1, m[1], strings.TrimSpace(line))...)
				}
				if i+1 < len(lines) && stepRefPinWrappedHead.MatchString(line) {
					if tm := stepRefPinWrappedTail.FindStringSubmatch(lines[i+1]); tm != nil {
						text := strings.TrimSpace(line) + " " + strings.TrimSpace(lines[i+1])
						out = append(out, splitStepCitation(rel, i+1, tm[1], text)...)
					}
				}
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
		if out[i].Line != out[j].Line {
			return out[i].Line < out[j].Line
		}
		return out[i].Num < out[j].Num
	})
	return out, nil
}
