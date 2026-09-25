package compat

import (
	"fmt"
	"regexp"
	"sort"
	"strings"
)

// RoutesSurface is the Surface every routes.golden Finding carries -- and
// therefore the "### <path>" subsection CheckVersionAndChangelog demands
// in CHANGELOG.md whenever routes.golden changes, the same mechanism a
// changed schema file goes through (Compare marks it changed).
const RoutesSurface = "controlplane/testdata/routes.golden"

// routeLineRE is routes.golden's entire line grammar: an upper-case HTTP
// method, exactly one space, and a path that starts with "/" and contains
// no whitespace -- exactly what the control-plane binary's own `routes`
// subcommand prints, one fmt.Fprintln per route.
var routeLineRE = regexp.MustCompile(`^[A-Z]+ /\S*$`)

// DiffRoutes implements rows 40-41 against controlplane/testdata/
// routes.golden: one "METHOD /path" line per route. A line present in
// base but not head is treated as "removed or renamed or method changed"
// (row 40, MAJOR) -- a rename, a path-parameter rename, or a method
// change on the same path is indistinguishable from a plain removal at
// this line-oriented grain, and row 40 assigns them the same severity
// anyway, so no finer split is needed. A line present in head but not
// base is row 41 (MINOR).
//
// Every row is graded, not only the ones under /api/: a route outside
// /api/ (/oauth/*, /.well-known/*, /mcp, /auth/*, /webhooks/*,
// /sessions/{sessionID}/*, /health) has a consumer outside the running
// build just as surely as an /api/ one does -- a third-party MCP client,
// an OAuth provider or webhook sender configured with the URL, an older
// sandbox-agent mid rolling deploy, a health probe. This used to read the
// /api/ rows only and skip every other line, so such a route could be
// added, removed, or re-methoded with no VERSION bump and no CHANGELOG
// entry at all, contradicting COMPATIBILITY.md's own VERSION/CHANGELOG
// discipline. COMPATIBILITY.md's "What is covered" section and rows 40/41
// say the same thing in prose.
//
// Fail closed: a line on either side that is not exactly "METHOD /path"
// (routeLineRE), or that repeats an earlier line, is a FAIL-CLOSED
// fc-routes-line Finding naming the side and line number, and no route
// diff is reported at all -- a set difference over a file this function
// could only partly read would be a guess, and silently skipping a line
// is exactly how the non-/api/ rows escaped grading before.
func DiffRoutes(baseRoutes, headRoutes []byte) []Finding {
	base, baseBad := parseRoutes("base", baseRoutes)
	head, headBad := parseRoutes("head", headRoutes)
	if len(baseBad) > 0 || len(headBad) > 0 {
		return append(baseBad, headBad...)
	}

	var findings []Finding
	var removed, added []string
	for line := range base {
		if !head[line] {
			removed = append(removed, line)
		}
	}
	for line := range head {
		if !base[line] {
			added = append(added, line)
		}
	}
	sort.Strings(removed)
	sort.Strings(added)

	for _, line := range removed {
		findings = append(findings, Finding{RuleID: "40", Severity: SeverityMajor, Pointer: line, Surface: RoutesSurface, Message: "route removed, renamed, or method changed: " + line})
	}
	for _, line := range added {
		findings = append(findings, Finding{RuleID: "41", Severity: SeverityMinor, Pointer: line, Surface: RoutesSurface, Message: "route added: " + line})
	}
	return findings
}

// parseRoutes reads one side of routes.golden into its set of route
// lines, plus one fc-routes-line Finding per line it refuses to read. A
// single trailing newline ends the last line; anything else that is not
// a route line -- a blank line included -- is refused, never skipped.
func parseRoutes(side string, data []byte) (map[string]bool, []Finding) {
	out := map[string]bool{}
	text := strings.TrimSuffix(string(data), "\n")
	if text == "" {
		return out, nil
	}

	var bad []Finding
	refuse := func(n int, why string) {
		bad = append(bad, Finding{
			RuleID:   "fc-routes-line",
			Severity: SeverityFailClosed,
			Surface:  RoutesSurface,
			Pointer:  fmt.Sprintf("%s line %d", side, n),
			Message:  fmt.Sprintf("%s routes.golden line %d %s -- refusing to grade a route table this checker cannot fully read; the file is the control-plane binary's own `routes` output (\"METHOD /path\", one route per line), so regenerate it rather than editing it by hand", side, n, why),
		})
	}

	firstSeen := map[string]int{}
	for i, line := range strings.Split(text, "\n") {
		n := i + 1
		if !routeLineRE.MatchString(line) {
			refuse(n, fmt.Sprintf("is not a \"METHOD /path\" route line: %q", line))
			continue
		}
		if first, dup := firstSeen[line]; dup {
			refuse(n, fmt.Sprintf("repeats line %d (%q)", first, line))
			continue
		}
		firstSeen[line] = n
		out[line] = true
	}
	return out, bad
}
