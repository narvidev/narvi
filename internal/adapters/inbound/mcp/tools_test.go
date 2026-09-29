package mcp

import (
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	sdkmcp "github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/narvidev/narvi/internal/domain/mcpscope"
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
	"narvi_list_models":            "GET /api/models",
	"narvi_list_sessions":          "GET /api/sessions",
	"narvi_get_session":            "GET /api/sessions/{sessionID}",
	"narvi_get_session_status":     "GET /api/sessions/{sessionID}/status",
	"narvi_wait_for_session":       "GET /api/sessions/{sessionID}/status",
	"narvi_get_session_result":     "GET /api/sessions/{sessionID}/result",
	"narvi_get_session_transcript": "GET /api/sessions/{sessionID}/events",
	"narvi_list_plans":             "GET /api/sessions/{sessionID}/plans",
	"narvi_create_session":         "POST /api/sessions",
	"narvi_approve_plan":           "POST /api/sessions/{sessionID}/plans/{planId}/approve",
	"narvi_reject_plan":            "POST /api/sessions/{sessionID}/plans/{planId}/reject",
	"narvi_request_plan_revision":  "POST /api/sessions/{sessionID}/turns",
	"narvi_send_prompt":            "POST /api/sessions/{sessionID}/turns",
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
// SAME way buildServer builds one for a full-scope grant (every tool,
// stub Twins since this test never actually invokes a twin), by
// constructing the server directly and calling its own ListTools method
// in-process -- no HTTP, no client transport, since only the SHAPE of
// the advertised tool set is under test here (the full HTTP+client round
// trip is exercised by TestParity_ToolsListIsRoleIndependent and this
// package's own handler_test.go).
func realToolsListTools(t testing.TB) []*sdkmcp.Tool {
	t.Helper()
	twins := Twins{
		ListModels:       stubHandler(200, `{}`),
		ListSessions:     stubHandler(200, `{}`),
		GetSession:       stubHandler(200, `{}`),
		GetSessionStatus: stubHandler(200, `{}`),
		ListEvents:       stubHandler(200, `{}`),
		GetSessionResult: stubHandler(200, `{}`),
		CreateSession:    stubHandler(201, `{}`),
		ListPlans:        stubHandler(200, `{}`),
		ApprovePlan:      stubHandler(200, `{}`),
		RejectPlan:       stubHandler(200, `{}`),
		CreateTurn:       stubHandler(201, `{}`),
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

// TestToolsList_ExactlyThirteenToolsDeterministicOrder pins the full
// table: a grant holding every advertised scope sees exactly these
// thirteen tools, in this order -- the eight reads, then the five writes.
// Which subset a narrower grant sees is TestToolsList_ScopeFilter_Table's
// (technical plan §43.17).
func TestToolsList_ExactlyThirteenToolsDeterministicOrder(t *testing.T) {
	want := []string{
		"narvi_list_models", "narvi_list_sessions", "narvi_get_session", "narvi_get_session_status", "narvi_wait_for_session", "narvi_get_session_result", "narvi_get_session_transcript", "narvi_list_plans",
		"narvi_create_session", "narvi_approve_plan", "narvi_reject_plan", "narvi_request_plan_revision", "narvi_send_prompt",
	}
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

// TestToolAnnotations_MatchTwinMethod pins technical plan §43.8: a tool
// whose twin is a GET is read-only, non-destructive, idempotent and
// closed-world; a tool whose twin writes is never marked read-only, and
// every one carries the destructive and open-world hints explicitly (a nil
// one means "assume the worst" to a client). Every write's own four are
// pinned too, as technical plan §43.8 and §43.21 list them.
func TestToolAnnotations_MatchTwinMethod(t *testing.T) {
	twinMethod := map[string]string{}
	for _, spec := range toolSpecs(Twins{}) {
		twinMethod[spec.Name] = spec.Twin.method
	}
	for _, tool := range realToolsListTools(t) {
		ann := tool.Annotations
		if ann == nil || ann.DestructiveHint == nil || ann.OpenWorldHint == nil {
			t.Fatalf("%s: Annotations = %+v, want every hint set explicitly", tool.Name, ann)
		}
		switch method := twinMethod[tool.Name]; method {
		case http.MethodGet:
			if !ann.ReadOnlyHint || *ann.DestructiveHint || !ann.IdempotentHint || *ann.OpenWorldHint {
				t.Errorf("%s (a GET twin): annotations %+v, want read-only, not destructive, idempotent, closed-world", tool.Name, *ann)
			}
		default:
			if ann.ReadOnlyHint {
				t.Errorf("%s (a %s twin): ReadOnlyHint = true, want false -- a write tool is never marked read-only", tool.Name, method)
			}
		}
	}
	// Each write's own four hints (technical plan §43.8, §43.21): destructive
	// only for the rejection, which ends a plan version; idempotent where a
	// repeat changes nothing more (a same-key create, a decided plan);
	// open-world where a turn it starts reaches the code host.
	type hints struct{ destructive, idempotent, openWorld bool }
	wantWrites := map[string]hints{
		"narvi_create_session":        {destructive: false, idempotent: true, openWorld: true},
		"narvi_approve_plan":          {destructive: false, idempotent: true, openWorld: true},
		"narvi_reject_plan":           {destructive: true, idempotent: true, openWorld: false},
		"narvi_request_plan_revision": {destructive: false, idempotent: false, openWorld: true},
		"narvi_send_prompt":           {destructive: false, idempotent: false, openWorld: true},
	}
	seen := 0
	for _, tool := range realToolsListTools(t) {
		want, ok := wantWrites[tool.Name]
		if !ok {
			if twinMethod[tool.Name] != http.MethodGet {
				t.Errorf("%s: a write tool with no expected annotations in this test", tool.Name)
			}
			continue
		}
		seen++
		ann := tool.Annotations
		got := hints{destructive: *ann.DestructiveHint, idempotent: ann.IdempotentHint, openWorld: *ann.OpenWorldHint}
		if ann.ReadOnlyHint || got != want {
			t.Errorf("%s: annotations %+v, want not read-only and %+v", tool.Name, *ann, want)
		}
	}
	if seen != len(wantWrites) {
		t.Errorf("saw %d of the %d write tools this test pins", seen, len(wantWrites))
	}
}

// TestRefusedWhileTurnOpen_MarksExactlyTheGatedTools pins which tools the
// instructions say are refused while a turn is open (technical plan
// §43.21): approve, whose twin refuses while any turn is open (the
// stale-plan guard), and the two turn tools, whose twin is POST .../turns
// with RejectIfOpen -- never reject, whose twin has no such gate, and never
// a read or the create. The flag changes nothing a tool does; this keeps
// the paragraph true.
func TestRefusedWhileTurnOpen_MarksExactlyTheGatedTools(t *testing.T) {
	want := map[string]bool{"narvi_approve_plan": true, "narvi_request_plan_revision": true, "narvi_send_prompt": true}
	for _, spec := range toolSpecs(Twins{}) {
		if spec.RefusedWhileTurnOpen != want[spec.Name] {
			t.Errorf("%s: RefusedWhileTurnOpen = %v, want %v", spec.Name, spec.RefusedWhileTurnOpen, want[spec.Name])
		}
	}
}

// TestWriteTwinsRequireWriteScope is §43.17's structural guard (technical
// plan §43.9): every tool whose twin is not a GET requires mcp:write, and
// every GET twin requires mcp:read -- so a POST twin can never be declared
// under the read scope, where a read-only grant would see and call it,
// and a read can never be hidden behind the write scope by mistake. A
// tool's BuildRequest cannot change its twin's method or path (twinCall
// carries neither), so the method in the table is the method that runs.
func TestWriteTwinsRequireWriteScope(t *testing.T) {
	sawWrite := false
	for _, spec := range toolSpecs(Twins{}) {
		switch spec.Twin.method {
		case http.MethodGet:
			if spec.Scope != mcpscope.Read {
				t.Errorf("%s: GET twin under scope %q, want %q", spec.Name, spec.Scope, mcpscope.Read)
			}
		default:
			sawWrite = true
			if spec.Scope != mcpscope.Write {
				t.Errorf("%s: %s twin under scope %q, want %q", spec.Name, spec.Twin.method, spec.Scope, mcpscope.Write)
			}
			if spec.CreateBrake != (spec.Name == "narvi_create_session") {
				t.Errorf("%s: CreateBrake = %v, want it set on the session-starting tool only", spec.Name, spec.CreateBrake)
			}
		}
	}
	if !sawWrite {
		t.Fatal("no tool in the table writes -- this guard proves nothing without one")
	}
}
