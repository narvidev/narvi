package mcp

import (
	"encoding/json"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	sdkmcp "github.com/modelcontextprotocol/go-sdk/mcp"
)

// repoRoot locates this repo's own root directory relative to this test
// file's own location via runtime.Caller, mirroring internal/ops/
// drift_test.go's own identical helper (a private copy, not an import:
// that helper is unexported in a different package).
func repoRoot(t testing.TB) string {
	t.Helper()
	_, file, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("runtime.Caller failed")
	}
	return filepath.Join(filepath.Dir(file), "..", "..", "..", "..")
}

// wantTwinRoutes pins each 180 tool's OWN declared twin route explicitly
// (technical plan §43.9 item 2) -- round 2 review of PR #324, finding
// N14: a prior version of TestEveryToolHasARegisteredTwin below checked
// only that "<METHOD> <pathTemplate>" was SOME line of routes.golden,
// never that it was the RIGHT line for that specific tool. A tool
// declaring a different real route (e.g. narvi_get_session's own
// pathTemplate swapped for "/api/models", while still invoking the
// correct twins.GetSession handler) passed that check undetected, since
// "GET /api/models" is itself a real golden line. This map is the
// missing piece: each tool's route, named once, independently of
// toolSpecs itself.
var wantTwinRoutes = map[string]string{
	"narvi_list_models":   "GET /api/models",
	"narvi_list_sessions": "GET /api/sessions",
	"narvi_get_session":   "GET /api/sessions/{sessionID}",
}

// TestEveryToolHasARegisteredTwin pins technical plan §43.9 item 2: a
// tool with no registered HTTP route cannot be declared, AND (finding
// N14) that its declared route is its OWN expected one (wantTwinRoutes
// above), not merely some other tool's -- both checked against
// controlplane/testdata/routes.golden, the same golden
// TestScanRegisteredRoutes_MatchesGolden (internal/ops) compares the
// static route scanner against.
func TestEveryToolHasARegisteredTwin(t *testing.T) {
	goldenPath := filepath.Join(repoRoot(t), "controlplane", "testdata", "routes.golden")
	data, err := os.ReadFile(goldenPath)
	if err != nil {
		t.Fatalf("read %s: %v", goldenPath, err)
	}
	lines := map[string]bool{}
	for _, line := range strings.Split(string(data), "\n") {
		line = strings.TrimSpace(line)
		if line != "" {
			lines[line] = true
		}
	}

	for _, spec := range toolSpecs(Twins{}) {
		got := spec.Twin.method + " " + spec.Twin.pathTemplate
		want, ok := wantTwinRoutes[spec.Name]
		if !ok {
			t.Fatalf("tool %q has no entry in wantTwinRoutes -- add one naming its own declared twin route explicitly", spec.Name)
		}
		if got != want {
			t.Errorf("tool %q declares twin %q, want its OWN route %q", spec.Name, got, want)
		}
		if !lines[want] {
			t.Errorf("tool %q's own twin route %q is not a line of %s", spec.Name, want, goldenPath)
		}
	}
}

// canonicalJSON re-marshals v with a stable, human-diffable
// (two-space-indented) encoding -- encoding/json already sorts map keys,
// so this is deterministic across runs/processes, load-bearing for
// testdata/tools.golden.json's own byte-for-byte comparison.
func canonicalJSON(t testing.TB, v any) []byte {
	t.Helper()
	data, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	return data
}

// realToolsList calls tools/list against a real *sdkmcp.Server built the
// SAME way buildServer builds one for a real request (three tools,
// stub Twins since this test never actually invokes a twin), by
// constructing the server directly and calling its own ListTools method
// in-process -- no HTTP, no client transport, since only the SHAPE of
// the advertised tool set is under test here (the full HTTP+client round
// trip is exercised by TestParity_ToolsListIsRoleIndependent and this
// package's own handler_test.go).
func realToolsListTools(t testing.TB) []*sdkmcp.Tool {
	t.Helper()
	twins := Twins{
		ListModels:   stubHandler(200, `{}`),
		ListSessions: stubHandler(200, `{}`),
		GetSession:   stubHandler(200, `{}`),
	}
	tools := make([]*sdkmcp.Tool, 0, len(toolSpecs(twins)))
	for _, spec := range toolSpecs(twins) {
		tool, err := buildTool(spec)
		if err != nil {
			t.Fatalf("buildTool(%q): %v", spec.Name, err)
		}
		tools = append(tools, tool)
	}
	return tools
}

// TestToolsList_MatchesGolden pins technical plan §43.10's own "tool list
// as a contract": tools/list's exact JSON (names, descriptions,
// annotations, bundled schemas) is pinned by testdata/tools.golden.json,
// with the usual update-on-purpose workflow (precedent: routes.golden) --
// a deliberate edit to a tool's own name/description/schema requires a
// deliberate golden update in the SAME PR, never a silent drift.
func TestToolsList_MatchesGolden(t *testing.T) {
	got := canonicalJSON(t, realToolsListTools(t))

	goldenPath := filepath.Join(repoRoot(t), "internal", "adapters", "inbound", "mcp", "testdata", "tools.golden.json")
	want, err := os.ReadFile(goldenPath)
	if err != nil {
		t.Fatalf("read %s: %v", goldenPath, err)
	}
	if strings.TrimSpace(string(got)) != strings.TrimSpace(string(want)) {
		t.Errorf("tools/list drifted from %s -- if this is a deliberate change, update the golden.\n--- got ---\n%s\n--- want ---\n%s", goldenPath, got, want)
	}
}

// TestToolsList_ExactlyThreeToolsDeterministicOrder pins "no discovery
// gating in 180" (technical plan §43.9 item 3's own parity row): every
// principal sees exactly the same three tools, in the same order.
func TestToolsList_ExactlyThreeToolsDeterministicOrder(t *testing.T) {
	want := []string{"narvi_list_models", "narvi_list_sessions", "narvi_get_session"}
	tools := realToolsListTools(t)
	if len(tools) != len(want) {
		t.Fatalf("len(tools) = %d, want %d", len(tools), len(want))
	}
	for i, tool := range tools {
		if tool.Name != want[i] {
			t.Errorf("tools[%d].Name = %q, want %q", i, tool.Name, want[i])
		}
	}
}

// TestToolAnnotations pins technical plan §43.8: every 180 tool is
// read-only, non-destructive, idempotent, and closed-world.
func TestToolAnnotations(t *testing.T) {
	for _, tool := range realToolsListTools(t) {
		ann := tool.Annotations
		if ann == nil {
			t.Fatalf("%s: Annotations = nil", tool.Name)
		}
		if !ann.ReadOnlyHint {
			t.Errorf("%s: ReadOnlyHint = false, want true", tool.Name)
		}
		if ann.DestructiveHint == nil || *ann.DestructiveHint {
			t.Errorf("%s: DestructiveHint = %v, want false", tool.Name, ann.DestructiveHint)
		}
		if !ann.IdempotentHint {
			t.Errorf("%s: IdempotentHint = false, want true", tool.Name)
		}
		if ann.OpenWorldHint == nil || *ann.OpenWorldHint {
			t.Errorf("%s: OpenWorldHint = %v, want false", tool.Name, ann.OpenWorldHint)
		}
	}
}
