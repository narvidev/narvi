package ops

import (
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
)

// TestScanRegisteredRoutes_MatchesGolden closes the gap a reviewer proved
// in routes.go's own former claim ("can only ENLARGE the registered set,
// never omit a real route" -- now corrected, see ScanRegisteredRoutes's
// own doc comment): ScanRegisteredRoutes is a narrow, purely syntactic
// go/ast walk that recognizes exactly five method names
// (chiRouterMethods) plus one grouping call (Route(...)) -- a route
// registered any other real way is invisible to it while staying
// perfectly reachable in production, e.g. a path held in a `const`,
// router.Method(...)/.Handle(...)/.HandleFunc(...), a helper outside
// controlplane/ taking a chi.Router, or a Mount'd sub-router whose own
// unprefixed route name (e.g. "GET /health") collides with, and silently
// overwrites, an existing top-level key in ScanRegisteredRoutes's own
// "found" map -- losing the real, distinct, Mount-prefixed route (e.g.
// "GET /api/probe/sub/health") from the set entirely, in favor of a
// duplicate of an already-exempted top-level route.
//
// CheckGuideDrift's own completeness check (guidedrift.go's
// "route-undocumented" direction) trusts ScanRegisteredRoutes's OWN
// "found" set as ground truth: a route this scanner never sees is not
// reported as undocumented, it is treated as though it does not exist at
// all. A blind spot in the scanner is therefore a blind spot in the
// entire omission check, silently.
//
// controlplane/testdata/routes.golden is this repo's one artifact
// captured directly from the REAL, live chi.Walk (controlplane.App.
// Routes(), pinned against it by the integration-tagged
// TestBuild_RouteTableMatchesGolden, controlplane/build_integration_test.
// go) -- ground truth this scanner cannot itself see, but this test can
// read as a plain committed file, no Postgres container required. This
// test asserts ScanRegisteredRoutes(controlplane) and that golden name
// the EXACT same route set, in both directions: any route the golden has
// that this scanner MISSES (the blind-spot gap above -- e.g. someone adds
// a router.Method(...) route or a Mount'd sub-router, regenerates the
// golden from the live router the way TestBuild_RouteTableMatchesGolden
// expects, and never notices the static scanner didn't move) and any
// route this scanner FINDS that the golden does NOT have (a stale or
// wrong scanner assumption) both fail here, naming every offending route.
//
// See this package's own guidedrift_test.go (TestNoGuideDrift's doc
// comment) and docs/guides/README.md's "What this check cannot catch"
// section for the completeness check this test backs; see routes_test.go
// for ScanRegisteredRoutes's own narrower, synthetic-fixture unit tests.
func TestScanRegisteredRoutes_MatchesGolden(t *testing.T) {
	root := repoRoot(t)

	scanned, err := ScanRegisteredRoutes(filepath.Join(root, "controlplane"))
	if err != nil {
		t.Fatalf("ScanRegisteredRoutes: %v", err)
	}
	if len(scanned) == 0 {
		t.Fatal("ScanRegisteredRoutes found zero routes -- almost certainly a scan-path bug (this repo has real chi routes), not a genuinely empty binary")
	}

	goldenPath := filepath.Join(root, "controlplane", "testdata", "routes.golden")
	goldenBytes, err := os.ReadFile(goldenPath)
	if err != nil {
		t.Fatalf("read %s: %v", goldenPath, err)
	}

	var golden []string
	for _, line := range strings.Split(strings.TrimRight(string(goldenBytes), "\n"), "\n") {
		if strings.TrimSpace(line) == "" {
			continue
		}
		golden = append(golden, line)
	}
	if len(golden) == 0 {
		t.Fatalf("%s is empty -- almost certainly a fixture bug (this repo has real chi routes), not a genuinely route-less binary", goldenPath)
	}

	goldenSet := make(map[string]bool, len(golden))
	for _, r := range golden {
		goldenSet[r] = true
	}

	var missing, extra []string
	for _, r := range golden {
		if _, ok := scanned[r]; !ok {
			missing = append(missing, r)
		}
	}
	for r := range scanned {
		if !goldenSet[r] {
			extra = append(extra, r)
		}
	}
	sort.Strings(missing)
	sort.Strings(extra)

	if len(missing) > 0 || len(extra) > 0 {
		t.Errorf("ScanRegisteredRoutes(controlplane) does not match %s -- the golden is the live router's own chi.Walk output; any difference here means this scanner's static go/ast walk and the real, running router have diverged, which also means CheckGuideDrift's completeness check cannot see whatever this scanner is missing:\nmissing (in golden, not found by ScanRegisteredRoutes -- reachable in production, invisible to the omission check): %s\nextra (found by ScanRegisteredRoutes, not in golden -- a stale or wrong scanner assumption): %s",
			goldenPath, formatMissingExtra(missing), formatMissingExtra(extra))
	}
}

// formatMissingExtra renders a route list for
// TestScanRegisteredRoutes_MatchesGolden's own failure message -- "(none)"
// rather than an empty, easy-to-miss line when one side of the diff is
// clean, mirroring controlplane/build_integration_test.go's own
// formatRouteList.
func formatMissingExtra(routes []string) string {
	if len(routes) == 0 {
		return "(none)"
	}
	return "\n  " + strings.Join(routes, "\n  ")
}
