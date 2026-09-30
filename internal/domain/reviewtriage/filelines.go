package reviewtriage

import (
	"strconv"
	"strings"
)

// hunkHeaderPrefix opens one hunk of a file section: "@@ -a,b +c,d @@".
const hunkHeaderPrefix = "@@ "

// FileLines is one file section of a unified diff, with the lines it adds
// and deletes -- ExtractFileLines' own output, read by Decide only to find
// the lines the size rule leaves out (sizeexclusion.go). Path is the
// file's post-change path, or its pre-change path for a deletion; OldPath
// is its pre-change path when a rename moved it, empty otherwise. Both
// carry no "a/"/"b/" prefix, the same shape ExtractChangedPaths reports.
type FileLines struct {
	Path    string
	OldPath string
	Added   int
	Deleted int
}

// ExtractFileLines parses diff (the same unified diff ExtractChangedPaths
// reads) into one FileLines per file section, in diff order. Pure, no I/O.
//
// Lines are counted inside hunks only, each hunk bounded by the old/new
// line counts its own "@@" header declares -- so an added line whose own
// content begins "++ " or "-- " (and so reads "+++ "/"--- " in the diff)
// is counted as the content line it is, never mistaken for a file header.
// A binary or mode-only section has no hunk and counts zero lines, as
// GitHub's own additions/deletions do.
//
// Paths come from the same lines ExtractChangedPaths reads -- the
// "diff --git" header first, then "--- a/"/"+++ b/" and "rename from"/
// "rename to", each overriding the header's own ambiguous unquoted parse
// -- through the same quote handling (extractPath, parseDiffGitHeaderLine).
func ExtractFileLines(diff string) []FileLines {
	if diff == "" {
		return nil
	}

	var out []FileLines
	cur := -1
	// sectionHasHunk marks a section whose hunks have started: a further
	// "--- " line after that opens a new section even without a
	// "diff --git" header (a plain, non-git unified diff).
	sectionHasHunk := false
	// pendingOld is the current section's "--- a/<path>", read before its
	// "+++" line decides whether the file was deleted.
	pendingOld := ""
	oldLeft, newLeft := 0, 0

	startSection := func(path, oldPath string) {
		out = append(out, FileLines{Path: path, OldPath: oldPath})
		cur = len(out) - 1
		sectionHasHunk = false
		pendingOld = ""
		oldLeft, newLeft = 0, 0
	}

	for _, line := range strings.Split(diff, "\n") {
		if cur >= 0 && (oldLeft > 0 || newLeft > 0) {
			inHunk := true
			switch {
			case line == "":
				// A blank context line whose leading space was stripped.
				oldLeft--
				newLeft--
			case line[0] == '+':
				out[cur].Added++
				newLeft--
			case line[0] == '-':
				out[cur].Deleted++
				oldLeft--
			case line[0] == ' ':
				oldLeft--
				newLeft--
			case line[0] == '\\':
				// "\ No newline at end of file": no line of either side.
			default:
				// Not hunk content: the hunk ended early (a malformed or
				// truncated diff). Read this line as a header.
				oldLeft, newLeft = 0, 0
				inHunk = false
			}
			if inHunk {
				continue
			}
		}

		switch {
		case strings.HasPrefix(line, diffGitHeaderPrefix):
			oldPath, newPath, ok := parseDiffGitHeaderLine(line)
			if !ok {
				startSection("", "")
				continue
			}
			if oldPath == newPath {
				oldPath = ""
			}
			startSection(newPath, oldPath)
		case strings.HasPrefix(line, removedMarkerPrefix):
			if cur < 0 || sectionHasHunk {
				startSection("", "")
			}
			if p := extractPath(line, removedMarkerPrefix); p != devNullPath {
				pendingOld = strings.TrimPrefix(p, removedPathPrefix)
			} else {
				pendingOld = ""
			}
		case strings.HasPrefix(line, addedMarkerPrefix):
			if cur < 0 {
				startSection("", "")
			}
			p := extractPath(line, addedMarkerPrefix)
			if p == devNullPath {
				// A deletion: the pre-change path is the file's only one.
				if pendingOld != "" {
					out[cur].Path = pendingOld
				}
				out[cur].OldPath = ""
			} else {
				out[cur].Path = strings.TrimPrefix(p, addedPathPrefix)
				if pendingOld != "" && pendingOld != out[cur].Path {
					out[cur].OldPath = pendingOld
				}
			}
			pendingOld = ""
		case strings.HasPrefix(line, renameFromPrefix):
			if cur >= 0 {
				out[cur].OldPath = extractPath(line, renameFromPrefix)
			}
		case strings.HasPrefix(line, renameToPrefix):
			if cur >= 0 {
				out[cur].Path = extractPath(line, renameToPrefix)
			}
		case strings.HasPrefix(line, hunkHeaderPrefix):
			if cur < 0 {
				continue
			}
			if o, n, ok := parseHunkHeader(line); ok {
				oldLeft, newLeft = o, n
				sectionHasHunk = true
			}
		}
	}
	return out
}

// parseHunkHeader reads the old and new line counts from a hunk header,
// "@@ -a[,b] +c[,d] @@ ...": a count left out is 1 (unified diff's own
// convention). ok is false for a header that does not parse.
func parseHunkHeader(line string) (oldCount, newCount int, ok bool) {
	rest, found := strings.CutPrefix(line, hunkHeaderPrefix)
	if !found {
		return 0, 0, false
	}
	fields := strings.Fields(rest)
	if len(fields) < 2 || !strings.HasPrefix(fields[0], "-") || !strings.HasPrefix(fields[1], "+") {
		return 0, 0, false
	}
	oldCount, ok = rangeCount(fields[0][1:])
	if !ok {
		return 0, 0, false
	}
	newCount, ok = rangeCount(fields[1][1:])
	if !ok {
		return 0, 0, false
	}
	return oldCount, newCount, true
}

// rangeCount reads the count half of one hunk range, "start[,count]".
func rangeCount(r string) (int, bool) {
	start, count, hasCount := strings.Cut(r, ",")
	if _, err := strconv.Atoi(start); err != nil {
		return 0, false
	}
	if !hasCount {
		return 1, true
	}
	n, err := strconv.Atoi(count)
	if err != nil || n < 0 {
		return 0, false
	}
	return n, true
}
