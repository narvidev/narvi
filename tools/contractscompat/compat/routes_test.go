package compat

import (
	"strings"
	"testing"
)

// routesGoldenFixture is a slice of the real controlplane/testdata/
// routes.golden, byte order and all: /api/ rows mixed with the non-/api/
// rows (well-known metadata, the MCP endpoint, OAuth, the socket the
// sandbox-agent and client-ws clients share, the health probe) that
// DiffRoutes used to skip without a finding.
const routesGoldenFixture = "GET /.well-known/oauth-protected-resource/mcp\n" +
	"GET /api/sessions\n" +
	"GET /health\n" +
	"GET /sessions/{sessionID}/ws\n" +
	"POST /mcp\n" +
	"POST /oauth/revoke\n" +
	"POST /oauth/token\n"

// changelogWith builds a head CHANGELOG.md whose top section is version,
// carrying one "### <path>" subsection per entry of subsections, above
// the base's own "## [1.0.0]".
func changelogWith(version string, subsections ...string) string {
	var b strings.Builder
	b.WriteString("## [" + version + "]\n")
	for _, s := range subsections {
		b.WriteString("### " + s + "\n- routes changed\n")
	}
	b.WriteString("\n## [1.0.0]\n")
	return b.String()
}

// routesOnlyInput is a full Compare input whose only possible difference
// is routes.golden: one schema surface, byte-identical on both sides,
// base at VERSION 1.0.0.
func routesOnlyInput(t *testing.T, baseRoutes, headRoutes, headVersion, headChangelog string) Input {
	t.Helper()
	schemaFile := "t/v1/x.schema.json"
	schema := mustMarshalFile(t, minimalFile("https://narvi.dev/t/v1/x.schema.json", defsOf("Widget", schemaObj("type", "string"))))
	return Input{
		BaseManifestRaw:  minimalManifestJSON(t, nil),
		HeadManifestRaw:  minimalManifestJSON(t, nil),
		BaseVersion:      "1.0.0",
		HeadVersion:      headVersion,
		BaseChangelogRaw: []byte("## [1.0.0]\n"),
		HeadChangelogRaw: []byte(headChangelog),
		BaseRoutes:       []byte(baseRoutes),
		HeadRoutes:       []byte(headRoutes),
		BaseSchemaFiles:  map[string][]byte{schemaFile: schema},
		HeadSchemaFiles:  map[string][]byte{schemaFile: schema},
	}
}

func replaceLine(t *testing.T, doc, old, repl string) string {
	t.Helper()
	if !strings.Contains(doc, old+"\n") {
		t.Fatalf("fixture has no line %q", old)
	}
	return strings.Replace(doc, old+"\n", repl, 1)
}

// TestRoutesGoldenGradedAsAWhole runs routes.golden changes through the
// whole Compare pipeline -- grading, fail-closed parsing, and the
// VERSION/CHANGELOG discipline together -- since the defect was never in
// one function alone: the non-/api/ rows were skipped by DiffRoutes, and
// nothing else compared the file, so the discipline never saw them.
func TestRoutesGoldenGradedAsAWhole(t *testing.T) {
	fixture := routesGoldenFixture
	added := replaceLine(t, fixture, "POST /oauth/revoke", "POST /oauth/introspect\nPOST /oauth/revoke\n")

	cases := []struct {
		name          string
		base, head    string
		headVersion   string
		headChangelog string
		want          []wantFinding // each must be present
		wantMsg       []string      // each must be a substring of some finding's message
		absent        []string      // rule ids that must NOT appear
		breaking      bool
		noFindings    bool
	}{
		{
			name: "unchanged file, non-/api/ rows included: no finding",
			base: fixture, head: fixture,
			headVersion: "1.0.0", headChangelog: "## [1.0.0]\n",
			noFindings: true,
		},
		{
			name: "non-/api/ route added with no VERSION bump: graded MINOR, bump demanded",
			base: fixture, head: added,
			headVersion: "1.0.0", headChangelog: "## [1.0.0]\n",
			want:     []wantFinding{{"41", SeverityMinor}, {"version-changelog", SeverityMajor}},
			wantMsg:  []string{"route added: POST /oauth/introspect", "VERSION was not bumped strictly upward"},
			breaking: true,
		},
		{
			name: "non-/api/ route added with only a PATCH bump: too small",
			base: fixture, head: added,
			headVersion: "1.0.1", headChangelog: changelogWith("1.0.1", RoutesSurface),
			want:     []wantFinding{{"41", SeverityMinor}, {"version-changelog", SeverityMajor}},
			wantMsg:  []string{"is smaller than the highest finding class (MINOR) requires"},
			breaking: true,
		},
		{
			name: "non-/api/ route added, MINOR bump but no routes.golden CHANGELOG subsection",
			base: fixture, head: added,
			headVersion: "1.1.0", headChangelog: changelogWith("1.1.0"),
			want:     []wantFinding{{"41", SeverityMinor}, {"version-changelog", SeverityMajor}},
			wantMsg:  []string{`missing a "### controlplane/testdata/routes.golden" subsection`},
			breaking: true,
		},
		{
			name: "non-/api/ route added, MINOR bump and CHANGELOG subsection: passes",
			base: fixture, head: added,
			headVersion: "1.1.0", headChangelog: changelogWith("1.1.0", RoutesSurface),
			want:   []wantFinding{{"41", SeverityMinor}},
			absent: []string{"version-changelog", "40"},
		},
		{
			name: "non-/api/ route removed: row 40 MAJOR, breaking even with a MAJOR bump",
			base: fixture, head: replaceLine(t, fixture, "GET /health", ""),
			headVersion: "2.0.0", headChangelog: changelogWith("2.0.0", RoutesSurface),
			want:     []wantFinding{{"40", SeverityMajor}},
			wantMsg:  []string{"route removed, renamed, or method changed: GET /health"},
			absent:   []string{"version-changelog"},
			breaking: true,
		},
		{
			name: "non-/api/ route method changed: row 40 MAJOR on the old line",
			base: fixture, head: replaceLine(t, fixture, "POST /oauth/revoke", "DELETE /oauth/revoke\n"),
			headVersion: "2.0.0", headChangelog: changelogWith("2.0.0", RoutesSurface),
			want:     []wantFinding{{"40", SeverityMajor}, {"41", SeverityMinor}},
			wantMsg:  []string{"method changed: POST /oauth/revoke", "route added: DELETE /oauth/revoke"},
			breaking: true,
		},
		{
			name: "non-/api/ path parameter renamed: a rename at line grain, row 40 MAJOR",
			base: fixture, head: replaceLine(t, fixture, "GET /sessions/{sessionID}/ws", "GET /sessions/{id}/ws\n"),
			headVersion: "2.0.0", headChangelog: changelogWith("2.0.0", RoutesSurface),
			want:     []wantFinding{{"40", SeverityMajor}},
			breaking: true,
		},
		{
			name: "byte-only change (lines reordered) with no bump: still needs the bump",
			base: fixture, head: "POST /mcp\n" + replaceLine(t, fixture, "POST /mcp", ""),
			headVersion: "1.0.0", headChangelog: "## [1.0.0]\n",
			want:     []wantFinding{{"version-changelog", SeverityMajor}},
			absent:   []string{"40", "41"},
			breaking: true,
		},
		{
			name: "byte-only change with a PATCH bump and CHANGELOG subsection: passes",
			base: fixture, head: "POST /mcp\n" + replaceLine(t, fixture, "POST /mcp", ""),
			headVersion: "1.0.1", headChangelog: changelogWith("1.0.1", RoutesSurface),
			absent: []string{"version-changelog", "40", "41"},
		},
		{
			name: "/api/ route added: row 41 MINOR, unchanged",
			base: fixture, head: replaceLine(t, fixture, "GET /api/sessions", "GET /api/sessions\nGET /api/widgets\n"),
			headVersion: "1.1.0", headChangelog: changelogWith("1.1.0", RoutesSurface),
			want:   []wantFinding{{"41", SeverityMinor}},
			absent: []string{"version-changelog", "40"},
		},
		{
			name: "/api/ route removed: row 40 MAJOR, unchanged",
			base: fixture, head: replaceLine(t, fixture, "GET /api/sessions", ""),
			headVersion: "2.0.0", headChangelog: changelogWith("2.0.0", RoutesSurface),
			want:     []wantFinding{{"40", SeverityMajor}},
			breaking: true,
		},
		{
			name: "head line with a trailing field fails closed, no route diff reported",
			base: fixture, head: added + "GET /oauth/userinfo extra\n",
			headVersion: "1.1.0", headChangelog: changelogWith("1.1.0", RoutesSurface),
			want:     []wantFinding{{"fc-routes-line", SeverityFailClosed}},
			wantMsg:  []string{`head routes.golden line 9 is not a "METHOD /path" route line: "GET /oauth/userinfo extra"`},
			absent:   []string{"40", "41"},
			breaking: true,
		},
		{
			name: "base line that is not a route fails closed",
			base: fixture + "NOT-A-ROUTE\n", head: fixture,
			headVersion: "1.0.0", headChangelog: "## [1.0.0]\n",
			want:     []wantFinding{{"fc-routes-line", SeverityFailClosed}},
			wantMsg:  []string{`base routes.golden line 8 is not a "METHOD /path" route line: "NOT-A-ROUTE"`},
			breaking: true,
		},
		{
			name: "blank line fails closed rather than being skipped",
			base: fixture, head: replaceLine(t, fixture, "GET /health", "GET /health\n\n"),
			headVersion: "1.0.1", headChangelog: changelogWith("1.0.1", RoutesSurface),
			want:     []wantFinding{{"fc-routes-line", SeverityFailClosed}},
			wantMsg:  []string{`head routes.golden line 4 is not a "METHOD /path" route line: ""`},
			breaking: true,
		},
		{
			name: "duplicated line fails closed",
			base: fixture, head: fixture + "GET /health\n",
			headVersion: "1.0.1", headChangelog: changelogWith("1.0.1", RoutesSurface),
			want:     []wantFinding{{"fc-routes-line", SeverityFailClosed}},
			wantMsg:  []string{`head routes.golden line 8 repeats line 3 ("GET /health")`},
			breaking: true,
		},
		{
			name: "CRLF line endings fail closed",
			base: fixture, head: strings.ReplaceAll(fixture, "\n", "\r\n"),
			headVersion: "1.0.1", headChangelog: changelogWith("1.0.1", RoutesSurface),
			want:     []wantFinding{{"fc-routes-line", SeverityFailClosed}},
			breaking: true,
		},
		{
			name: "lower-case method fails closed",
			base: fixture, head: replaceLine(t, fixture, "GET /health", "get /health\n"),
			headVersion: "1.0.1", headChangelog: changelogWith("1.0.1", RoutesSurface),
			want:     []wantFinding{{"fc-routes-line", SeverityFailClosed}},
			breaking: true,
		},
		{
			name: "path without a leading slash fails closed",
			base: fixture, head: replaceLine(t, fixture, "GET /health", "GET health\n"),
			headVersion: "1.0.1", headChangelog: changelogWith("1.0.1", RoutesSurface),
			want:     []wantFinding{{"fc-routes-line", SeverityFailClosed}},
			breaking: true,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			report, err := Compare(routesOnlyInput(t, tc.base, tc.head, tc.headVersion, tc.headChangelog))
			if err != nil {
				t.Fatalf("Compare: %v", err)
			}
			if tc.noFindings && len(report.Findings) != 0 {
				t.Fatalf("want no findings, got %+v", report.Findings)
			}
			for _, w := range tc.want {
				if !containsFinding(report.Findings, w.ruleID, w.severity) {
					t.Errorf("want rule %s severity %s, got %+v", w.ruleID, w.severity, report.Findings)
				}
			}
			for _, msg := range tc.wantMsg {
				found := false
				for _, f := range report.Findings {
					if strings.Contains(f.Message, msg) {
						found = true
						break
					}
				}
				if !found {
					t.Errorf("want a finding whose message contains %q, got %+v", msg, report.Findings)
				}
			}
			for _, id := range tc.absent {
				for _, f := range report.Findings {
					if f.RuleID == id {
						t.Errorf("rule %s must not appear, got %+v", id, f)
					}
				}
			}
			if got := report.HasBreaking(); got != tc.breaking {
				t.Errorf("HasBreaking() = %v, want %v; findings %+v", got, tc.breaking, report.Findings)
			}
		})
	}
}

// TestParseRoutesAcceptsTheGeneratedShape pins the other side of failing
// closed: everything the `routes` subcommand can actually produce --
// including an empty table and a last line with or without its trailing
// newline -- still parses, with nothing refused.
func TestParseRoutesAcceptsTheGeneratedShape(t *testing.T) {
	cases := []struct {
		name      string
		data      string
		wantCount int
	}{
		{"empty file", "", 0},
		{"trailing newline", "GET /a\n", 1},
		{"no trailing newline", "GET /a", 1},
		{"real mix of /api/ and non-/api/ rows", routesGoldenFixture, 7},
		{"root path", "GET /\n", 1},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			routes, bad := parseRoutes("head", []byte(tc.data))
			if len(bad) != 0 {
				t.Fatalf("want nothing refused, got %+v", bad)
			}
			if len(routes) != tc.wantCount {
				t.Fatalf("want %d routes, got %d (%v)", tc.wantCount, len(routes), routes)
			}
		})
	}
}
