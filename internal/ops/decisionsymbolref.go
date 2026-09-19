package ops

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
)

// This file is W8's own audit fix (confirmed MEDIUM finding: "the decision
// record is wrong again, about the same decision"). D-06 (docs/DECISIONS.md)
// had already drifted from what shipped once before this batch even started
// -- it described the Linear actor gate as resolving through "the SAME
// auto-linking algorithm the pre-existing AgentSessionEvent path already
// runs", the exact behaviour a PRIOR round removed as a confirmed
// HIGH-severity finding (identitylink.Resolve's own auto-linking, replaced
// by the pure identitylink.LookupLinkedUserID). Nothing had compared D-06's
// prose against the code it describes since it was written, so it drifted
// silently -- twice.
//
// This repository's own rule, stated plainly in CLAUDE.md: point, do not
// paraphrase. CheckSectionRefs/CheckDeferredTriggers (sectionref.go,
// deferreddecisions.go) already bind two other DECISIONS.md/comment
// citation shapes to something real (a plan section, a non-empty reopen
// condition) rather than trusting the prose. This is the third: a decision
// entry that names WHICH symbol implements it, in the form
// `Symbol` (path/to/file.go), is making a claim CI can actually verify --
// that the symbol still exists, in that file, today. It cannot verify the
// claim's own ALGORITHM description is still accurate (that would require
// understanding the code, which is exactly what a human review is for) --
// but a citation naming a symbol that has been renamed, moved, or deleted
// is the loudest, cheapest signal that the prose around it has drifted too,
// and this check catches exactly that signal, the same partial-coverage
// framing CheckSectionRefs' own doc comment already accepts for its own
// citation shape.
//
// The fix this enables is not "verify D-06 is correct forever" -- no
// mechanical check can do that for prose describing behavior. It is
// "reduce D-06 to claims durable enough for such a check to exist at all":
// a symbol name is durable (it either still exists or it does not); a
// paraphrase of what a function DOES is not (it silently stops matching
// reality the moment the function's own behavior changes, with nothing to
// signal the drift). D-06 and D-07 (docs/DECISIONS.md) were rewritten
// alongside this file specifically to lean on symbol citations rather than
// algorithm descriptions wherever the two could be separated.

// decisionSymbolRefPattern matches a `Symbol` (path/to/file.ext) citation --
// a backtick-quoted Go identifier (optionally dotted, e.g.
// "actorauthz.AuthorizeLinkedActor") immediately followed, across any
// amount of whitespace INCLUDING a markdown line-wrap (\s in RE2 already
// matches newlines -- no (?s) flag needed, deliberately: this pattern
// contains no "." that would need one), by a parenthesized file path ending
// in a recognized source extension. The whitespace tolerance matters
// because docs/DECISIONS.md hand-wraps prose at ~100 columns -- a citation
// written on one logical line routinely has its own "(path)" half pushed to
// the following source line by a later edit, and a checker that only
// matched same-line adjacency would silently stop verifying every citation
// an editor's own line-wrapping happened to split, which is precisely the
// "looks maintained but isn't" failure this whole file exists against.
var decisionSymbolRefPattern = regexp.MustCompile(
	"`([A-Za-z_][A-Za-z0-9_]*(?:\\.[A-Za-z_][A-Za-z0-9_]*)?)`\\s*\\(([A-Za-z0-9_./-]+\\.(?:go|sql|md))\\)",
)

// DecisionSymbolRef is one `Symbol` (path) citation found in
// docs/DECISIONS.md.
type DecisionSymbolRef struct {
	Symbol string
	Path   string
	Line   int
}

// ScanDecisionSymbolRefs parses every `Symbol` (path/to/file.ext) citation
// out of docs/DECISIONS.md. Finding zero is not itself an error (unlike
// LoadDeferredDecisions' own empty-table guard) -- this citation SHAPE is
// opt-in prose style, not a table with a fixed heading this file promises
// always has rows -- but see decisionsymbolref_test.go's own
// TestDecisionSymbolRefsAreFound for why this package still pins a
// non-zero count today.
func ScanDecisionSymbolRefs(root string) ([]DecisionSymbolRef, error) {
	path := filepath.Join(root, "docs", "DECISIONS.md")
	raw, err := os.ReadFile(path) //nolint:gosec // repo-relative, fixed name
	if err != nil {
		return nil, fmt.Errorf("read %s: %w", path, err)
	}
	doc := string(raw)

	var out []DecisionSymbolRef
	for _, m := range decisionSymbolRefPattern.FindAllStringSubmatchIndex(doc, -1) {
		symbol := doc[m[2]:m[3]]
		refPath := doc[m[4]:m[5]]
		line := 1 + strings.Count(doc[:m[0]], "\n")
		out = append(out, DecisionSymbolRef{Symbol: symbol, Path: refPath, Line: line})
	}
	return out, nil
}

// wholeWordPattern caches one compiled \b<word>\b regexp per symbol name --
// CheckDecisionSymbolRefs may check the same symbol against the same file
// repeatedly (D-06/D-07 both cite ClassifyLinearActorOrigin, for instance),
// so this avoids recompiling identical patterns.
func wholeWordPattern(symbol string) *regexp.Regexp {
	return regexp.MustCompile(`\b` + regexp.QuoteMeta(symbol) + `\b`)
}

// CheckDecisionSymbolRefs verifies every ref resolves: the cited path
// exists under root, and the cited symbol appears in that file's contents
// as a whole word (a plain substring/word-boundary search, not a Go parse
// -- this deliberately does not distinguish a real declaration from an
// ordinary reference or a comment mentioning the same identifier, mirroring
// CheckSectionRefs' own "catches every citation that resolves to nothing,
// not every citation that resolves to the WRONG thing" scope). Returns one
// human-readable message per unresolved citation, sorted for a stable
// failure message.
func CheckDecisionSymbolRefs(root string, refs []DecisionSymbolRef) []string {
	type key struct{ path, symbol string }
	cache := make(map[key]error)

	var bad []string
	for _, ref := range refs {
		k := key{ref.Path, ref.Symbol}
		err, checked := cache[k]
		if !checked {
			err = checkOneDecisionSymbolRef(root, ref)
			cache[k] = err
		}
		if err != nil {
			bad = append(bad, fmt.Sprintf("docs/DECISIONS.md:%d: `%s` (%s): %v", ref.Line, ref.Symbol, ref.Path, err))
		}
	}
	sort.Strings(bad)
	return bad
}

func checkOneDecisionSymbolRef(root string, ref DecisionSymbolRef) error {
	full := filepath.Join(root, ref.Path)
	contents, err := os.ReadFile(full) //nolint:gosec // a repo-relative path cited from docs/DECISIONS.md
	if err != nil {
		return fmt.Errorf("cited file does not exist (renamed or moved?): %w", err)
	}
	if !wholeWordPattern(ref.Symbol).Match(contents) {
		return fmt.Errorf("symbol %q not found anywhere in the cited file (renamed or removed?)", ref.Symbol)
	}
	return nil
}
