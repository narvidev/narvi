package compat

import (
	"strings"
	"testing"
	"unicode"
	"unicode/utf8"
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
		{
			// The review repro: with \S as the path class this passed with
			// exit 0 as a MINOR "route added: GET /health\u00a0".
			name: "whitespace-only variant of an existing route (trailing no-break space) fails closed, not graded as an added route",
			base: fixture, head: replaceLine(t, fixture, "GET /health", "GET /health\nGET /health\u00a0\n"),
			headVersion: "1.1.0", headChangelog: changelogWith("1.1.0", RoutesSurface),
			want:     []wantFinding{{"fc-routes-line", SeverityFailClosed}},
			wantMsg:  []string{`head routes.golden line 4 is not a "METHOD /path" route line: "GET /health\u00a0"`},
			absent:   []string{"40", "41"},
			breaking: true,
		},
		{
			name: "route replaced by a vertical-tab variant fails closed, not graded as a removal plus an addition",
			base: fixture, head: replaceLine(t, fixture, "GET /health", "GET /health\v\n"),
			headVersion: "2.0.0", headChangelog: changelogWith("2.0.0", RoutesSurface),
			want:     []wantFinding{{"fc-routes-line", SeverityFailClosed}},
			wantMsg:  []string{`head routes.golden line 3 is not a "METHOD /path" route line: "GET /health\v"`},
			absent:   []string{"40", "41"},
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

// TestParseRoutesRefusesOffGrammarLines pins COMPATIBILITY.md's "Routes"
// grammar line by line: exactly an upper-case method ([A-Z]+), one ASCII
// space, and a path starting with "/" with no whitespace of any kind and
// no control character in it. Every shape below is one a looser grammar
// would read as a route -- a whitespace-only variant of a real route
// included -- and each must be refused as fc-routes-line at exactly the
// line(s) named, never skipped and never graded.
func TestParseRoutesRefusesOffGrammarLines(t *testing.T) {
	cases := []struct {
		name string
		data string
		want []string // Pointers of the refused lines, in order
	}{
		// Whitespace RE2's \S admits, and control characters, in the path.
		{"trailing vertical tab", "GET /health\v\n", []string{"head line 1"}},
		{"trailing no-break space U+00A0", "GET /health\u00a0\n", []string{"head line 1"}},
		{"line separator U+2028 inside the path", "GET /he\u2028alth\n", []string{"head line 1"}},
		{"paragraph separator U+2029", "GET /health\u2029\n", []string{"head line 1"}},
		{"ideographic space U+3000", "GET /health\u3000\n", []string{"head line 1"}},
		{"next line U+0085", "GET /health\u0085\n", []string{"head line 1"}},
		{"trailing tab", "GET /health\t\n", []string{"head line 1"}},
		{"trailing form feed", "GET /health\f\n", []string{"head line 1"}},
		{"NUL inside the path", "GET /he\x00alth\n", []string{"head line 1"}},
		{"escape inside the path", "GET /\x1b[0m\n", []string{"head line 1"}},
		{"trailing DEL", "GET /health\x7f\n", []string{"head line 1"}},
		{"C1 control U+009B", "GET /health\u009b\n", []string{"head line 1"}},
		{"bad line after a good one is refused at its own line", "GET /a\nGET /health\u00a0\n", []string{"head line 2"}},

		// The method, the separator, and where a line starts.
		{"leading space before the method", " GET /health\n", []string{"head line 1"}},
		{"leading tab before the method", "\tGET /health\n", []string{"head line 1"}},
		{"junk before the method", "xGET /health\n", []string{"head line 1"}},
		{"empty method", " /health\n", []string{"head line 1"}},
		{"double-space separator", "GET  /health\n", []string{"head line 1"}},
		{"tab separator", "GET\t/health\n", []string{"head line 1"}},
		{"no-break space separator", "GET\u00a0/health\n", []string{"head line 1"}},

		// Blank lines at either end of the file: only ONE trailing newline
		// ends the last line; nothing is trimmed beyond it.
		{"trailing blank line at end of file", "GET /a\n\n", []string{"head line 2"}},
		{"two trailing blank lines", "GET /a\n\n\n", []string{"head line 2", "head line 3"}},
		{"whitespace-only last line", "GET /a\n \n", []string{"head line 2"}},
		{"leading blank line", "\nGET /a\n", []string{"head line 1"}},
		{"leading whitespace-only line", " \nGET /a\n", []string{"head line 1"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, bad := parseRoutes("head", []byte(tc.data))
			var got []string
			for _, f := range bad {
				if f.RuleID != "fc-routes-line" || f.Severity != SeverityFailClosed || f.Surface != RoutesSurface {
					t.Errorf("want an fc-routes-line FAIL-CLOSED finding on %s, got %+v", RoutesSurface, f)
				}
				got = append(got, f.Pointer)
			}
			if strings.Join(got, ",") != strings.Join(tc.want, ",") {
				t.Fatalf("refused %q, want %q; findings %+v", got, tc.want, bad)
			}
		})
	}
}

// TestRouteLinePathRunes checks routeLineRE's path class against
// COMPATIBILITY.md's wording for every Unicode code point, rather than
// trusting the \p{Z}/\p{Cc} argument on routeLineRE: a rune inside a path
// is accepted exactly when it is neither whitespace of any kind
// (unicode.IsSpace) nor a control character (unicode.IsControl).
func TestRouteLinePathRunes(t *testing.T) {
	mismatches := 0
	for r := rune(0); r <= unicode.MaxRune; r++ {
		if !utf8.ValidRune(r) {
			continue // a surrogate half: no UTF-8 encoding, cannot occur in a line
		}
		want := !unicode.IsSpace(r) && !unicode.IsControl(r)
		if got := routeLineRE.MatchString("GET /a" + string(r) + "b"); got != want {
			t.Errorf("U+%04X inside a path: accepted = %v, want %v (IsSpace %v, IsControl %v)", r, got, want, unicode.IsSpace(r), unicode.IsControl(r))
			if mismatches++; mismatches >= 20 {
				t.Fatal("too many mismatches, stopping")
			}
		}
	}
}
