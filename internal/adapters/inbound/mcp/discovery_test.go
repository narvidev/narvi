package mcp

import (
	"encoding/json"
	"net/http"
	"reflect"
	"strings"
	"sync"
	"testing"

	"github.com/narvidev/narvi/internal/domain/mcpscope"
)

// discoverBody / toolsListBody are modern (2026-07-28) requests.
const (
	discoverBody  = `{"jsonrpc":"2.0","id":1,"method":"server/discover","params":{"_meta":{"io.modelcontextprotocol/protocolVersion":"2026-07-28","io.modelcontextprotocol/clientCapabilities":{}}}}`
	toolsListBody = `{"jsonrpc":"2.0","id":1,"method":"tools/list","params":{"_meta":{"io.modelcontextprotocol/protocolVersion":"2026-07-28","io.modelcontextprotocol/clientCapabilities":{}}}}`
)

func scopes(s ...string) *[]string { return &s }

// listToolNames runs tools/list through handler and returns the names,
// failing on anything but a 200 with a result.
func listToolNames(t *testing.T, handler http.Handler) []string {
	t.Helper()
	status, body := rawPost(t, handler, "/mcp", toolsListBody, map[string]string{
		protocolVersionHeader: "2026-07-28",
		"Mcp-Method":          "tools/list",
	})
	if status != http.StatusOK {
		t.Fatalf("tools/list: status %d body %s, want 200", status, body)
	}
	var env struct {
		Result *struct {
			Tools []struct {
				Name string `json:"name"`
			} `json:"tools"`
		} `json:"result"`
	}
	if err := json.Unmarshal(body, &env); err != nil || env.Result == nil {
		t.Fatalf("tools/list: body %s (err %v), want a result", body, err)
	}
	names := make([]string, 0, len(env.Result.Tools))
	for _, tool := range env.Result.Tools {
		names = append(names, tool.Name)
	}
	return names
}

// TestToolsList_ScopeFilter_Table is the "a scope-less client's tool list
// omits what it cannot call" exit criterion at the unit level, driven
// through the real NewHandler: each grant sees exactly the tools its
// scopes satisfy -- none for a scope-less, unknown-scope or missing grant,
// the read tools only for mcp:read, and every tool for mcp:write (which
// implies mcp:read). Row 182's status, wait, result and transcript tools are
// mcp:read like the rest: a grant without it is told none of them exists
// (technical plan §43.20); narvi_create_session is mcp:write, so a
// read-only grant is told it does not exist (§43.17).
func TestToolsList_ScopeFilter_Table(t *testing.T) {
	reads := []string{"narvi_get_session", "narvi_get_session_result", "narvi_get_session_status", "narvi_get_session_transcript", "narvi_list_models", "narvi_list_sessions", "narvi_wait_for_session"}
	all := append([]string{"narvi_create_session"}, reads...)
	tests := []struct {
		name   string
		scopes *[]string
		want   []string
	}{
		{"scope-less grant", scopes(), []string{}},
		{"unknown scope only", scopes("mcp:admin"), []string{}},
		{"empty scope string", scopes(""), []string{}},
		{"no grant at all (defect)", nil, []string{}},
		{"mcp:read sees the reads only", scopes("mcp:read"), reads},
		{"mcp:write implies mcp:read", scopes("mcp:write"), all},
		{"both", scopes("mcp:read", "mcp:write"), all},
		{"an unknown scope beside mcp:read adds nothing", scopes("mcp:read", "mcp:admin"), reads},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := listToolNames(t, newTestHandlerWithGrant(t, testTwins(), tc.scopes))
			if !reflect.DeepEqual(got, tc.want) {
				t.Fatalf("tools/list = %v, want %v", got, tc.want)
			}
		})
	}
}

// discoverInstructions returns server/discover's instructions for a grant.
func discoverInstructions(t *testing.T, grant *[]string) string {
	t.Helper()
	status, body := rawPost(t, newTestHandlerWithGrant(t, testTwins(), grant), "/mcp", discoverBody, map[string]string{
		protocolVersionHeader: "2026-07-28",
		"Mcp-Method":          "server/discover",
	})
	if status != http.StatusOK {
		t.Fatalf("server/discover: status %d body %s, want 200 (discovery must still succeed for a narrow grant)", status, body)
	}
	var env struct {
		Result *struct {
			Instructions string `json:"instructions"`
		} `json:"result"`
	}
	if err := json.Unmarshal(body, &env); err != nil || env.Result == nil {
		t.Fatalf("server/discover: body %s (err %v)", body, err)
	}
	return env.Result.Instructions
}

// TestInstructions_NameOnlyVisibleTools: the instructions paragraph is a
// description of the deployment too, so it names exactly the visible
// tools -- none for a scope-less or missing grant, the reads alone for a
// read-only grant -- and it calls the tools read-only exactly when no write
// tool is visible: never beside narvi_create_session.
func TestInstructions_NameOnlyVisibleTools(t *testing.T) {
	for name, grant := range map[string]*[]string{"scope-less": scopes(), "no grant": nil, "unknown scope": scopes("mcp:admin")} {
		if got := discoverInstructions(t, grant); strings.Contains(got, "narvi_") {
			t.Errorf("%s grant: instructions %q name a tool", name, got)
		}
	}
	readOnly := discoverInstructions(t, scopes("mcp:read"))
	full := discoverInstructions(t, scopes("mcp:read", "mcp:write"))
	for _, spec := range toolSpecs(Twins{}) {
		if !strings.Contains(full, spec.Name) {
			t.Errorf("full grant: instructions %q do not name %s", full, spec.Name)
		}
		if got := strings.Contains(readOnly, spec.Name); got != (spec.Scope == mcpscope.Read) {
			t.Errorf("read-only grant: instructions name %s = %v, want %v: %q", spec.Name, got, spec.Scope == mcpscope.Read, readOnly)
		}
	}
	if !strings.Contains(readOnly, "READ-ONLY") || !strings.Contains(readOnly, "None of these tools writes anything.") {
		t.Errorf("read-only grant: instructions %q do not say the tools only read", readOnly)
	}
	if strings.Contains(full, "READ-ONLY") || strings.Contains(full, "None of these tools writes anything") {
		t.Errorf("full grant: instructions %q call the tools read-only beside narvi_create_session", full)
	}
	if !strings.Contains(full, "can run code in their repositories and spend on models: narvi_create_session") {
		t.Errorf("full grant: instructions %q do not say what the write tool can do", full)
	}
}

// TestInstructionsFor_Composition pins the paragraph's shape for every
// visible-set size this table can produce.
func TestInstructionsFor_Composition(t *testing.T) {
	t.Parallel()

	specs := toolSpecs(Twins{})
	if got := instructionsFor(nil); strings.Contains(got, "narvi_") || got == "" {
		t.Errorf("instructionsFor(nil) = %q", got)
	}
	one := instructionsFor(specs[:1])
	if !strings.Contains(one, "one READ-ONLY tool over") || !strings.Contains(one, specs[0].Instruction) || strings.Contains(one, specs[1].Name) {
		t.Errorf("instructionsFor(one) = %q", one)
	}
	two := instructionsFor(specs[:2])
	if !strings.Contains(two, "two READ-ONLY tools over") || !strings.Contains(two, specs[0].Instruction+" and "+specs[1].Instruction) {
		t.Errorf("instructionsFor(two) = %q", two)
	}
	three := instructionsFor(specs[:3])
	want := "This server exposes three READ-ONLY tools over this deployment's session data: " +
		specs[0].Instruction + ", " + specs[1].Instruction + ", and " + specs[2].Instruction + ". None of these tools writes anything."
	if three != want {
		t.Errorf("instructionsFor(three) =\n %q\nwant\n %q", three, want)
	}
	reads := specs[:7]
	for _, spec := range reads {
		if spec.Scope != mcpscope.Read {
			t.Fatalf("specs[:7] holds %s, which is not a read -- this test assumes the reads come first", spec.Name)
		}
	}
	allReads := instructionsFor(reads)
	want = "This server exposes seven READ-ONLY tools over this deployment's session data: " +
		specs[0].Instruction + ", " + specs[1].Instruction + ", " + specs[2].Instruction + ", " + specs[3].Instruction + ", " + specs[4].Instruction + ", " + specs[5].Instruction + ", and " + specs[6].Instruction + ". None of these tools writes anything."
	if allReads != want {
		t.Errorf("instructionsFor(the reads) =\n %q\nwant\n %q", allReads, want)
	}

	// With the write tool visible, the paragraph names the reads and the
	// write apart and never says read-only.
	all := instructionsFor(specs)
	want = "This server exposes eight tools over this deployment's session data. Seven only read and change nothing: " +
		specs[0].Instruction + ", " + specs[1].Instruction + ", " + specs[2].Instruction + ", " + specs[3].Instruction + ", " + specs[4].Instruction + ", " + specs[5].Instruction + ", and " + specs[6].Instruction +
		". One acts as the user who approved this client, within what that user's own role allows, and can run code in their repositories and spend on models: " + specs[7].Instruction + "."
	if all != want {
		t.Errorf("instructionsFor(all) =\n %q\nwant\n %q", all, want)
	}
	oneEach := instructionsFor([]toolSpec{specs[0], specs[7]})
	want = "This server exposes two tools over this deployment's session data. One only reads and changes nothing: " + specs[0].Instruction +
		". One acts as the user who approved this client, within what that user's own role allows, and can run code in their repositories and spend on models: " + specs[7].Instruction + "."
	if oneEach != want {
		t.Errorf("instructionsFor(one read, one write) =\n %q\nwant\n %q", oneEach, want)
	}
}

// TestHiddenToolCall_IsIndistinguishableFromUnknownTool: calling a tool
// the grant hides must answer EXACTLY what calling a tool that does not
// exist answers -- same HTTP status, same JSON-RPC error object, byte for
// byte once the one thing the caller itself chose (the name it sent,
// which the SDK echoes back in its message) is substituted. Anything else
// (a 403, an insufficient_scope, a different message) would confirm the
// hidden tool exists.
func TestHiddenToolCall_IsIndistinguishableFromUnknownTool(t *testing.T) {
	call := func(grant *[]string, tool string) (int, string) {
		status, body := rawPost(t, newTestHandlerWithGrant(t, testTwins(), grant), "/mcp", callToolBody(7, tool, "{}"), callToolHeaders(tool))
		return status, string(body)
	}
	const unknown = "narvi_does_not_exist"
	unknownStatus, unknownBody := call(scopes("mcp:read"), unknown)

	// The write tool under a read-only grant (technical plan §43.17): a
	// grant that may read but not write is told narvi_create_session does
	// not exist, in exactly an unknown tool's bytes -- never a 403 or an
	// insufficient_scope that would confirm it is there. A write is picked
	// by what its twin does (any method but GET), never by the scope it
	// declares, so a write declared under mcp:read is caught here too.
	writes := 0
	for _, spec := range toolSpecs(Twins{}) {
		if spec.Twin.method == http.MethodGet {
			continue
		}
		writes++
		t.Run(spec.Name+" under mcp:read", func(t *testing.T) {
			hiddenStatus, hiddenBody := call(scopes("mcp:read"), spec.Name)
			if want := strings.ReplaceAll(unknownBody, unknown, spec.Name); hiddenStatus != unknownStatus || hiddenBody != want {
				t.Fatalf("write tool under a read grant differs from an unknown tool:\n hidden:  %d %s\n unknown: %d %s", hiddenStatus, hiddenBody, unknownStatus, want)
			}
		})
	}
	if writes == 0 {
		t.Fatal("no tool in the table writes -- the read-grant rows above prove nothing without one")
	}

	// Every tool in the table, row 182's status, wait, result and transcript
	// tools included: each is hidden from a scope-less grant exactly as a
	// name that never existed is.
	for _, spec := range toolSpecs(Twins{}) {
		hidden := spec.Name
		t.Run(hidden, func(t *testing.T) {
			hiddenStatus, hiddenBody := call(scopes(), hidden)
			if hiddenStatus != unknownStatus {
				t.Fatalf("HTTP status: hidden %d, unknown %d -- must be identical", hiddenStatus, unknownStatus)
			}
			if want := strings.ReplaceAll(unknownBody, unknown, hidden); hiddenBody != want {
				t.Fatalf("hidden tool's response differs from an unknown tool's:\n hidden:  %s\n unknown: %s", hiddenBody, want)
			}
			var env jsonrpcEnvelope
			if err := json.Unmarshal([]byte(hiddenBody), &env); err != nil || env.Error == nil || env.Error.Code != -32602 {
				t.Fatalf("hidden tool response = %s, want a JSON-RPC -32602 error", hiddenBody)
			}
			// The same holds under no grant at all.
			noGrantStatus, noGrantBody := call(nil, hidden)
			if noGrantStatus != hiddenStatus || noGrantBody != hiddenBody {
				t.Fatalf("no-grant response differs from the scope-less one:\n %s\n %s", noGrantBody, hiddenBody)
			}
		})
	}
}

// TestBridge_NoAuthorizationHeaderReachesTwin is the token-passthrough
// threat row: the request a twin receives carries no header at all -- in
// particular not the Authorization header the MCP request arrived with,
// nor its cookie. The auth stand-in here deliberately leaves the header
// on the incoming request (the real gate also strips it), so this proves
// the bridge's own empty-header request on its own.
func TestBridge_NoAuthorizationHeaderReachesTwin(t *testing.T) {
	var mu sync.Mutex
	var seen []http.Header
	record := func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		seen = append(seen, r.Header.Clone())
		mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"providers":[]}`))
	}
	twins := testTwins()
	twins.ListModels = record
	handler := newTestHandlerWithGrant(t, twins, scopes("mcp:read"))
	headers := callToolHeaders("narvi_list_models")
	headers["Authorization"] = "Bearer narvi_mcp_at_must-never-reach-a-twin"
	headers["Cookie"] = "narvi_auth_session=also-never"
	status, body := rawPost(t, handler, "/mcp", callToolBody(1, "narvi_list_models", "{}"), headers)
	if status != http.StatusOK {
		t.Fatalf("status %d body %s", status, body)
	}
	mu.Lock()
	defer mu.Unlock()
	if len(seen) != 1 {
		t.Fatalf("twin called %d times, want 1", len(seen))
	}
	if len(seen[0]) != 0 {
		t.Fatalf("twin received headers %v, want none at all", seen[0])
	}
}

// TestAdvertisedScopes: exactly the scopes the tool table requires --
// mcp:read for the reads and, since narvi_create_session requires it,
// mcp:write, in canonical order. The authorization server offers this one
// list and the 401 challenge names it (controlplane).
func TestAdvertisedScopes(t *testing.T) {
	t.Parallel()

	if got := AdvertisedScopes(); !reflect.DeepEqual(got, []mcpscope.Scope{mcpscope.Read, mcpscope.Write}) {
		t.Fatalf("AdvertisedScopes() = %v, want [mcp:read mcp:write]", got)
	}
}

// TestToolSpecs_EveryToolDeclaresScopeAndInstruction: a tool with no scope
// is invisible to every grant (mcpscope.Satisfies fails closed), and one
// with no instruction fragment would leave a hole in the paragraph -- both
// are table defects, caught here rather than in production.
func TestToolSpecs_EveryToolDeclaresScopeAndInstruction(t *testing.T) {
	t.Parallel()

	for _, spec := range toolSpecs(Twins{}) {
		if !mcpscope.Known(spec.Scope) {
			t.Errorf("%s: Scope = %q, want a scope in mcpscope.Vocabulary", spec.Name, spec.Scope)
		}
		if !strings.HasPrefix(spec.Instruction, spec.Name+" ") {
			t.Errorf("%s: Instruction = %q, want it to start with the tool's own name", spec.Name, spec.Instruction)
		}
	}
}
