package compat

import (
	"sort"
	"strings"
)

// DiffRoutes implements rows 40-41 against controlplane/testdata/
// routes.golden: one "METHOD /path" line per route. A line present in
// base but not head is treated as "removed or renamed or method changed"
// (row 40, MAJOR) -- a rename or a method change on the same path is
// indistinguishable from a plain removal at this line-oriented grain, and
// row 40 assigns them the same severity anyway, so no finer split is
// needed. A line present in head but not base is row 41 (MINOR). Only
// lines under /api/ are considered, matching the design spec's own scope
// note (routes.golden's own %d comment lines and any non-/api/ row, if
// one is ever added, are ignored rather than fail-closed -- this is data,
// not a schema, so the keyword allowlist does not apply to it).
func DiffRoutes(baseRoutes, headRoutes []byte) []Finding {
	base := routeLines(baseRoutes)
	head := routeLines(headRoutes)

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
		findings = append(findings, Finding{RuleID: "40", Severity: SeverityMajor, Pointer: line, Surface: "controlplane/testdata/routes.golden", Message: "route removed, renamed, or method changed: " + line})
	}
	for _, line := range added {
		findings = append(findings, Finding{RuleID: "41", Severity: SeverityMinor, Pointer: line, Surface: "controlplane/testdata/routes.golden", Message: "route added: " + line})
	}
	return findings
}

func routeLines(data []byte) map[string]bool {
	out := map[string]bool{}
	for _, line := range strings.Split(string(data), "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		fields := strings.Fields(line)
		if len(fields) != 2 {
			continue
		}
		if !strings.HasPrefix(fields[1], "/api/") {
			continue
		}
		out[line] = true
	}
	return out
}
