package ops

import (
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
)

// This file is Y4 audit fix's own considered extension of the SAME
// discipline decisionsymbolref.go already enforces for `Symbol` (path)
// citations -- confirmed LOW finding: four Go source comments cited
// docs/DECISIONS.md's D-06 for a claim that is actually D-07's. Both D-06
// and D-07 are REAL, existing headings, so nothing already in this package
// had anything to catch: CheckDecisionSymbolRefs verifies a cited SYMBOL
// still resolves, never that a cited DECISION ID names the entry the prose
// around it actually describes.
//
// # What this file can cheaply add, and what it still cannot
//
// It CAN bind a "D-NN" citation found in real Go source to whether that
// id exists at all among docs/DECISIONS.md's own real headings
// (decisionheadings.go's own ScanDecisionHeadings) -- a citation of a
// typo'd, renumbered-away, or otherwise nonexistent id is now a build
// failure, the same shape CheckSectionRefs (sectionref.go) already
// enforces for "§N" plan-section citations.
//
// It CANNOT tell "D-06" apart from "D-07" when BOTH are real, existing
// headings and the citing comment simply names the wrong one -- exactly
// Y4's own defect, and exactly the boundary CheckSectionRefs' own doc
// comment already draws for its citation shape: catches every citation
// that resolves to NOTHING, never one that resolves to the
// wrong-but-existing thing. That would require understanding what the
// prose around the citation actually claims, which is what human review
// (the five rounds that found this) is for.
//
// Scope is deliberately narrower than CheckSectionRefs': .go source only,
// under internal/cmd/controlplane/extension/contracts -- never
// docs/DECISIONS.md itself. That file is where "D-NN" is DEFINED, not
// where a citation of it (in the sense this file checks) needs binding: a
// decision entry's own internal cross-references (e.g. D-07's own "see
// D-06 above") are a different, self-referential question this file does
// not answer, and Y4's own four sites were all Go comments, never prose
// inside DECISIONS.md itself.

// decisionIDRefPattern matches a "D-NN" decision-record citation,
// word-boundary-anchored on BOTH sides so it can never match a substring
// of an unrelated hyphenated token. Verified empirically against this
// repository's own real source before this pattern was chosen: an
// UNANCHORED "D-\d+" scan turns up exactly two false positives today, in
// opencode/compactionretry_test.go and githubapi/listopenprs.go (search
// that pair of files for the word "ROUND" immediately followed by a
// hyphen and a number, twice, to see the real, unedited text --
// deliberately not spelled out here in the composed shape a false
// positive would take, so this doc comment is not itself a citation this
// file's own check would need to resolve). Both are eliminated by these
// anchors: the letter immediately before that word's own trailing hyphen
// is also a word character, and \b only matches a transition between a
// word character and a non-word character (or a string edge), never a
// position between two word characters.
var decisionIDRefPattern = regexp.MustCompile(`\bD-(\d+)\b`)

// DecisionIDRef is one "D-NN" citation found in real Go source, outside
// docs/DECISIONS.md itself.
type DecisionIDRef struct {
	Number int // parsed digits, e.g. 6 for a "D-06" or "D-6" citation
	Cited  string
	File   string
	Count  int
}

// ScanDecisionIDRefs scans every .go file under the given roots for a
// "D-NN" citation -- mirrors CheckSectionRefs' own directory walk
// (sectionref.go), narrowed to .go only (see this file's own top doc
// comment for why docs/DECISIONS.md itself is never a target here).
func ScanDecisionIDRefs(root string, scanDirs []string) ([]DecisionIDRef, error) {
	perFile := make(map[string]map[string]DecisionIDRef)

	for _, dir := range scanDirs {
		base := filepath.Join(root, dir)
		if _, err := os.Stat(base); os.IsNotExist(err) {
			continue
		}
		err := filepath.WalkDir(base, func(path string, d fs.DirEntry, err error) error {
			if err != nil {
				return err
			}
			if d.IsDir() || !strings.HasSuffix(path, ".go") {
				return nil
			}
			raw, readErr := os.ReadFile(path) //nolint:gosec // walking a repo-relative tree
			if readErr != nil {
				return readErr
			}
			for _, m := range decisionIDRefPattern.FindAllStringSubmatch(string(raw), -1) {
				n, convErr := strconv.Atoi(m[1])
				if convErr != nil {
					continue // not reachable given \d+, but never worth failing the whole scan over
				}
				rel, relErr := filepath.Rel(root, path)
				if relErr != nil {
					rel = path
				}
				if perFile[rel] == nil {
					perFile[rel] = make(map[string]DecisionIDRef)
				}
				key := fmt.Sprintf("D-%d", n)
				ref := perFile[rel][key]
				ref.Number = n
				ref.Cited = m[0]
				ref.File = rel
				ref.Count++
				perFile[rel][key] = ref
			}
			return nil
		})
		if err != nil {
			return nil, fmt.Errorf("walk %s: %w", dir, err)
		}
	}

	var out []DecisionIDRef
	for _, byID := range perFile {
		for _, ref := range byID {
			out = append(out, ref)
		}
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].File != out[j].File {
			return out[i].File < out[j].File
		}
		return out[i].Number < out[j].Number
	})
	return out, nil
}

// CheckDecisionIDRefs returns every DecisionIDRef whose Number does not
// appear among headings (the real "### D-NN" numbers ScanDecisionHeadings
// found in docs/DECISIONS.md, decisionheadings.go) -- see this file's own
// top doc comment for exactly what this can and cannot catch.
func CheckDecisionIDRefs(refs []DecisionIDRef, headings []int) []DecisionIDRef {
	valid := make(map[int]bool, len(headings))
	for _, n := range headings {
		valid[n] = true
	}
	var bad []DecisionIDRef
	for _, ref := range refs {
		if !valid[ref.Number] {
			bad = append(bad, ref)
		}
	}
	sort.Slice(bad, func(i, j int) bool {
		if bad[i].File != bad[j].File {
			return bad[i].File < bad[j].File
		}
		return bad[i].Number < bad[j].Number
	})
	return bad
}
