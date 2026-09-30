package reviewtriage

import "strings"

// The unified-diff-header line prefixes/markers ExtractChangedPaths below
// recognizes -- mirrors internal/domain/reviewpost's own diffFileHeaderRE
// precedent (position.go: "^\+\+\+ b/(.+)$") for what counts as a real
// file-header line, extended (adversarial-review fix, D3: "sensitive-glob
// rule is blind to deleted and renamed files") to also recognize a
// deletion's own "--- a/<path>" line (paired with a "+++ /dev/null"
// post-change side) and a rename's own "rename from <path>"/"rename to
// <path>" lines.
const (
	// addedMarkerPrefix/removedMarkerPrefix are a file section's own two
	// header lines, unified-diff's "+++ "/"--- " -- the text FOLLOWING
	// either prefix is a path, possibly git-C-style-quoted (extractPath
	// below handles both forms).
	addedMarkerPrefix   = "+++ "
	removedMarkerPrefix = "--- "
	// addedPathPrefix/removedPathPrefix are the literal "b/"/"a/" this
	// package strips off the path FOLLOWING addedMarkerPrefix/
	// removedMarkerPrefix once any C-style quoting has already been
	// resolved -- present on every ADDED-or-MODIFIED ("+++ b/<path>") or
	// REMOVED-or-MODIFIED ("--- a/<path>") file's own header line.
	addedPathPrefix   = "b/"
	removedPathPrefix = "a/"
	// devNullPath is unified diff's own fixed "this side does not exist"
	// literal -- NEVER git-quoted (it is not a real path), so it is
	// compared as a plain literal, never run through extractPath's own
	// quote handling.
	devNullPath = "/dev/null"
	// diffGitHeaderPrefix marks the start of each new file's own diff
	// section. Recognized for two reasons: (1) resetting pendingRemoved
	// (below) so a stale "--- a/<path>" value can never leak forward from
	// an earlier file section into a LATER, unrelated one, and (2, new in
	// this fix) as the ONE source of a changed file's own path for a file
	// SHAPE that emits no "+++"/"---"/rename line at all -- see
	// parseDiffGitHeaderLine's own doc comment below.
	diffGitHeaderPrefix = "diff --git "
	// renameFromPrefix/renameToPrefix are a DETECTED RENAME's own two
	// lines (git's unified-diff extension, present whenever git's own
	// rename-detection fires, WHETHER OR NOT the rename also carries a
	// content change) -- each is its own single, unambiguous line (a
	// fixed literal prefix, the path -- possibly C-style-quoted -- is
	// everything after it to end of line), unlike the
	// "diff --git a/<old> b/<new>" header line itself, which concatenates
	// BOTH paths onto one line with no unambiguous separator when either
	// path could itself contain the literal substring " b/" (parsed here
	// ONLY as a last-resort fallback, see parseDiffGitHeaderLine, never
	// preferred over these two lines when they are present). For a PURE
	// rename (100% similarity, no content change at all), these two lines
	// are the ONLY place either path appears anywhere in the diff -- no
	// "--- a/"/"+++ b/" pair is emitted for such a file at all.
	renameFromPrefix = "rename from "
	renameToPrefix   = "rename to "
)

// ExtractChangedPaths deterministically parses diff (a unified,
// possibly-multi-file diff, exactly the shape internal/app/
// reviewcontext.Fetch's own GetCompareDiff call returns and already
// hands to review.RenderTurnPrompt) into the ordered, deduplicated list
// of repo-relative paths it touches -- §26.3's own "changed paths,
// promoted to a first-class structured signal (not just used inline)".
// Pure, no I/O: diff is already-fetched text, never re-fetched here.
//
// Every file this diff touches contributes AT LEAST one path (a
// modification or addition: its post-change path via addedMarkerPrefix; a
// deletion: its pre-change path via removedMarkerPrefix, paired against
// devNullPath; a detected rename: BOTH its pre- and post-change paths, via
// renameFromPrefix/renameToPrefix -- a rename vacates one location and
// populates another, and both are real changes to the tree this package's
// own downstream signals, sensitive-glob matching and distinct-root
// counting, care about; and, new in this fix, a file whose change carries
// NONE of the above lines at all -- see the D2 section below) -- never
// zero, for any file shape this function recognizes as a file section at
// all.
//
// # Adversarial-review fix, D2 (adversarial review of PR #182, BLOCKING): binary/mode-only/quoted-path files used to contribute NO path at all
//
// Before this fix, this function harvested a path ONLY from "+++ b/",
// "--- a/" (paired with "+++ /dev/null"), and "rename from"/"rename to"
// lines. Three real, unremarkable diff shapes emit NONE of those:
//
//   - a BINARY file's own change -- "diff --git a/x b/x" followed by
//     "index ..." and "Binary files a/x and b/x differ", with no "---"/
//     "+++" pair at all;
//   - a MODE-ONLY change (a file's executable bit flipped, content
//     unchanged) -- "diff --git a/x b/x" followed by "old mode .../new
//     mode ...", again no "---"/"+++" pair;
//   - a file whose path itself needs git's own C-style quoting (any
//     non-ASCII byte, by default -- core.quotePath) -- e.g.
//     `+++ "b/uni_caf\303\251.go"` -- which never matched the OLD,
//     unquoted-only "+++ b/" prefix test at all, so this shape silently
//     contributed no path even though it DOES have an ordinary "---"/
//     "+++" pair.
//
// A live finding on such a file used to retire while the file was
// genuinely still changed -- and adversarially reachable with a single
// byte (git classifies content with a NUL in roughly its first 8000 bytes
// as binary, so appending one NUL to a flagged text file converts its own
// diff section from a normal "---"/"+++" pair into a binary one). Worse,
// binary/mode-only changes are exactly the shapes some findings exist
// BECAUSE of ("this script is now world-executable, unjustified", "a
// 12 MB blob was committed to git") -- the two path-derived downstream
// signals (sensitive-glob matching, distinct-root counting) went BLIND on
// them too, the identical unsafe direction as the rename/deletion gap the
// PRIOR fix (immediately below) closed.
//
// The fix: extractPath (below) now resolves BOTH the quoted and unquoted
// forms of every "+++"/"---"/rename line, closing the quoted-path gap
// directly; and parseDiffGitHeaderLine (below) is consulted, as a
// fallback ONLY, for a file section that produced NO path at all from any
// of those lines -- exactly the binary/mode-only shapes, which carry
// their own two paths on the "diff --git a/<old> b/<new>" line and
// nowhere else.
//
// # Adversarial-review fix, D3 [prior Step]: deleted and renamed files used to contribute NO path at all
//
// Before THAT fix, this function recognized ONLY "+++ b/<path>" -- a
// deleted file's own "+++ /dev/null" header (no "b/" prefix) never
// matched, contributing NO path entry at all, and a 100%-similarity
// rename (no "---"/"+++" pair emitted at all) likewise contributed
// nothing. This was NOT a purely safe, only-ever-conservative gap the way
// the "an added line's own content happens to start with a header-shaped
// string" false-positive risk (still real, still accepted, see below) is
// -- it was a genuine false NEGATIVE, the unsafe direction.
//
// Known, accepted limitation (unchanged by either fix, the ONE remaining
// case where this function's own parse can only ever ADD a spurious path,
// never miss a real one -- internal/domain/reviewpost's own
// diffFileHeaderRE already accepts the identical tradeoff, position.go):
// an ADDED source line whose own content happens to start with one of
// this function's own recognized prefixes (e.g. the four literal
// characters "+++ b") would misidentify as a file header. This is
// vanishingly rare in practice, and the failure direction here IS safe:
// it can only ever ADD a spurious path to the result, nudging root
// dispersion or a sensitive-glob match toward the MORE conservative
// (deep-routing) outcome, never the less safe one. parseDiffGitHeaderLine's
// own unquoted-form ambiguity (its own doc comment, below) carries the
// identical "may add a spurious path, never miss a real one" bias.
func ExtractChangedPaths(diff string) []string {
	if diff == "" {
		return nil
	}

	seen := make(map[string]bool)
	var out []string
	add := func(p string) {
		if p == "" || seen[p] {
			return
		}
		seen[p] = true
		out = append(out, p)
	}

	// pendingRemoved is the most recently seen removedMarkerPrefix line's
	// own path -- consumed (and cleared) by the VERY NEXT devNullPath or
	// addedMarkerPrefix line, which is always this SAME file section's own
	// paired post-change side in a well-formed unified diff. Explicitly
	// cleared on diffGitHeaderPrefix too, so a malformed/truncated diff
	// can never leak a stale value forward onto an unrelated LATER file's
	// deletion.
	var pendingRemoved string
	// sectionHasPath tracks whether the CURRENT file section (since the
	// most recent diffGitHeaderPrefix line, or the start of diff if none
	// yet) has already contributed a path via a "+++"/"---"/rename line --
	// D2's own signal for whether parseDiffGitHeaderLine's fallback is
	// needed at all when this section ends (a NEW diffGitHeaderPrefix
	// line, or the end of diff). A section that DID contribute one of
	// those never needs the fallback: those lines are always at least as
	// trustworthy as the "diff --git" line's own ambiguous unquoted-form
	// parse (this function's own top doc comment).
	var sectionHasPath bool
	// sectionOldPath/sectionNewPath/sectionHeaderOK are the CURRENT file
	// section's own diff --git header, parsed eagerly (parseDiffGitHeaderLine)
	// the moment that line is seen, but only ADDED to the result (via
	// finalizeSection, below) once the section is known to have produced
	// no path any other way.
	var sectionOldPath, sectionNewPath string
	var sectionHeaderOK bool

	finalizeSection := func() {
		if sectionHasPath || !sectionHeaderOK {
			return
		}
		// D2: this section's own diff --git header is the ONLY source of
		// a path for it (a binary or mode-only change) -- add both sides
		// (a no-op for an unrenamed file, where they are identical and
		// add's own dedup collapses them to one entry).
		add(sectionOldPath)
		add(sectionNewPath)
	}

	for _, line := range strings.Split(diff, "\n") {
		switch {
		case strings.HasPrefix(line, diffGitHeaderPrefix):
			finalizeSection()
			pendingRemoved = ""
			sectionHasPath = false
			sectionOldPath, sectionNewPath, sectionHeaderOK = parseDiffGitHeaderLine(line)
		case strings.HasPrefix(line, removedMarkerPrefix):
			p := extractPath(line, removedMarkerPrefix)
			if p == devNullPath {
				// This file's own PRE-change side does not exist -- an
				// ADDED file. No path here; reset defensively (this line
				// should never follow a still-pending removal, but never
				// let one leak forward regardless).
				pendingRemoved = ""
			} else {
				pendingRemoved = strings.TrimPrefix(p, removedPathPrefix)
			}
		case strings.HasPrefix(line, addedMarkerPrefix):
			p := extractPath(line, addedMarkerPrefix)
			if p == devNullPath {
				// This file's own POST-change side does not exist -- a
				// DELETED file. pendingRemoved (this SAME file section's
				// own paired "--- a/<path>" line, just above) is this
				// file's ONE AND ONLY path.
				add(pendingRemoved)
				if pendingRemoved != "" {
					sectionHasPath = true
				}
			} else {
				add(strings.TrimPrefix(p, addedPathPrefix))
				sectionHasPath = true
			}
			pendingRemoved = ""
		case strings.HasPrefix(line, renameFromPrefix):
			add(extractPath(line, renameFromPrefix))
			sectionHasPath = true
		case strings.HasPrefix(line, renameToPrefix):
			add(extractPath(line, renameToPrefix))
			sectionHasPath = true
		}
	}
	finalizeSection()

	return out
}

// extractPath strips prefix from line and resolves the remainder as
// EITHER a plain path OR a git C-style-quoted one (unquotePathLiteral,
// below) -- every "+++"/"---"/rename-from/rename-to line's own path is
// git-quoted by that SAME rule (core.quotePath, on by default: any
// non-ASCII byte, or one of a small set of special characters, triggers
// quoting for the WHOLE line's own path, e.g.
// `+++ "b/uni_caf\303\251.go"`, D2 above) or left bare, and this function
// handles both without the caller needing to know which it is looking at.
//
// An unquoted path loses one trailing TAB: git (and so GitHub's compare
// diff) ends a "---"/"+++" line with a TAB when the path contains a space,
// so the line cannot be misread as a path followed by a traditional diff's
// timestamp. A real path never ends in a TAB here -- git quotes any path
// containing one -- so the TAB is never part of the name.
func extractPath(line, prefix string) string {
	rest := strings.TrimPrefix(line, prefix)
	if strings.HasPrefix(rest, `"`) {
		if quoted, _, ok := leadingQuotedLiteral(rest); ok {
			return unquotePathLiteral(quoted)
		}
	}
	return strings.TrimSuffix(rest, "\t")
}

// parseDiffGitHeaderLine parses a "diff --git <old> <new>" line's own two
// paths -- consulted ONLY as ExtractChangedPaths' own fallback (D2, above)
// for a file section that produced no path from any "+++"/"---"/rename
// line at all (a binary or mode-only change, this function's own two real
// callers-in-spirit). oldPath/newPath are returned WITHOUT their own
// "a/"/"b/" prefix; ok is false when line does not even start with
// diffGitHeaderPrefix (should be unreachable given this function's own
// one call site) or the remainder could not be parsed at all.
//
// Two forms, handled separately:
//
//   - BOTH paths git-C-style-quoted (core.quotePath fires whenever either
//     path needs it): `diff --git "a/<old>" "b/<new>"` -- unambiguous by
//     construction, since a quoted literal's own closing quote is
//     unambiguous (leadingQuotedLiteral, below) regardless of what
//     characters -- including a literal " b/" substring -- the path
//     itself contains.
//   - UNQUOTED (the common case: pure ASCII, nothing to quote):
//     `diff --git a/<old> b/<new>` -- ambiguous in principle (either path
//     COULD contain the literal substring " b/"), resolved by splitting
//     at the LAST " b/" in the line, mirroring this file's own top-level
//     doc comment's accepted "may add a spurious path, never miss a real
//     one" bias: a path containing " b/" is vanishingly rare, and
//     splitting at the wrong point only ever changes WHICH spurious path
//     string gets added, never causes a real path to go unreported (the
//     section itself is still recognized, and finalizeSection's own
//     "some path beats no path" fallback still fires).
func parseDiffGitHeaderLine(line string) (oldPath, newPath string, ok bool) {
	rest, ok := strings.CutPrefix(line, diffGitHeaderPrefix)
	if !ok {
		return "", "", false
	}

	if strings.HasPrefix(rest, `"`) {
		oldQuoted, remainder, ok2 := leadingQuotedLiteral(rest)
		if !ok2 {
			return "", "", false
		}
		remainder = strings.TrimPrefix(remainder, " ")
		newQuoted, _, ok3 := leadingQuotedLiteral(remainder)
		if !ok3 {
			return "", "", false
		}
		oldPath = strings.TrimPrefix(unquotePathLiteral(oldQuoted), removedPathPrefix)
		newPath = strings.TrimPrefix(unquotePathLiteral(newQuoted), addedPathPrefix)
		return oldPath, newPath, true
	}

	const splitMarker = " b/"
	idx := strings.LastIndex(rest, splitMarker)
	if idx < 0 {
		return "", "", false
	}
	oldPath = strings.TrimPrefix(rest[:idx], removedPathPrefix)
	newPath = rest[idx+len(splitMarker):]
	return oldPath, newPath, true
}

// leadingQuotedLiteral finds a git C-style-quoted literal (a double-quote,
// then any run of characters with escaped quotes/backslashes skipped,
// then the closing double-quote) starting at the very beginning of s.
// Returns the quoted literal INCLUDING its own surrounding quotes (for
// unquotePathLiteral to consume), the remainder of s after the closing
// quote, and whether a well-formed closing quote was found at all.
func leadingQuotedLiteral(s string) (quoted, rest string, ok bool) {
	if len(s) == 0 || s[0] != '"' {
		return "", s, false
	}
	for i := 1; i < len(s); i++ {
		switch s[i] {
		case '\\':
			i++ // skip the escaped character (handles `\"` and `\\`).
		case '"':
			return s[:i+1], s[i+1:], true
		}
	}
	return "", s, false
}

// unquotePathLiteral decodes a git C-style-quoted path literal (as
// produced by leadingQuotedLiteral, including its own surrounding
// quotes) -- git's own quote_c_style: a small set of named backslash
// escapes (\a \b \f \n \r \t \v \\ \") plus \NNN octal-byte escapes for
// everything else quoting was triggered by (typically non-ASCII bytes,
// core.quotePath's own default behavior). s unchanged (returned as-is) if
// it is not actually quote-delimited -- defensive, should be unreachable
// given this function's one caller already checked for a leading quote.
func unquotePathLiteral(s string) string {
	if len(s) < 2 || s[0] != '"' || s[len(s)-1] != '"' {
		return s
	}
	inner := s[1 : len(s)-1]

	var b strings.Builder
	b.Grow(len(inner))
	for i := 0; i < len(inner); i++ {
		c := inner[i]
		if c != '\\' || i+1 >= len(inner) {
			b.WriteByte(c)
			continue
		}
		i++
		switch inner[i] {
		case 'a':
			b.WriteByte('\a')
		case 'b':
			b.WriteByte('\b')
		case 'f':
			b.WriteByte('\f')
		case 'n':
			b.WriteByte('\n')
		case 'r':
			b.WriteByte('\r')
		case 't':
			b.WriteByte('\t')
		case 'v':
			b.WriteByte('\v')
		case '\\':
			b.WriteByte('\\')
		case '"':
			b.WriteByte('"')
		default:
			if inner[i] >= '0' && inner[i] <= '7' {
				val := 0
				digits := 0
				for digits < 3 && i < len(inner) && inner[i] >= '0' && inner[i] <= '7' {
					val = val*8 + int(inner[i]-'0')
					i++
					digits++
				}
				i-- // compensate the loop's own i++
				b.WriteByte(byte(val))
			} else {
				// Not a recognized escape -- keep the backslash-escaped
				// character literally rather than silently dropping the
				// backslash (defensive; git itself never emits this, this
				// function's own fixed set of escapes above is exhaustive
				// for quote_c_style's real output).
				b.WriteByte(inner[i])
			}
		}
	}
	return b.String()
}
