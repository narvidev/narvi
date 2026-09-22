package opencode

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/narvidev/narvi/contracts/gen/go/sandboxws"
)

// This file closes a confirmed re-review finding: the wiring from a
// sentinel-auto-fix session's own provenance tag all the way to which
// OpenCode agent its turns actually run under (SessionConfig.
// CapabilityRestricted -> Adapter.capabilityRestricted -> postPromptAsync's
// own agent-selection switch, session.go) had ZERO test coverage anywhere
// -- neither planmode_test.go (this package's own established pattern for
// testing this exact switch, but only ever constructing an Adapter with
// capabilityRestricted left at its default false) nor
// sentinelfixagent_test.go/sentinelfixagent_realbinary_test.go (which only
// prove MergeSentinelFixAgentConfig/the real binary's own agent
// registration, never postPromptAsync's own selection of it) ever
// exercised this. Before this fix, deleting the `case
// a.capabilityRestricted:` branch entirely, or hardcoding
// CapabilityRestricted to false in sessionconfig.go, would have compiled
// and passed every existing test in this diff.
//
// Mirrors TestPostPromptAsync_PlanModeSelectsNativePlanAgent's own exact
// httptest.Server-capturing-the-raw-request-body style (that test's own
// doc comment explains why a minimal fake server, not a real/live OpenCode
// process, is the right tool here).

// TestPostPromptAsync_CapabilityRestrictedSelectsSentinelFixAgent proves:
// an Adapter constructed with capabilityRestricted=true sends
// "agent":"sentinel-fix" on prompt_async for an ordinary (non-plan-mode)
// build turn; capabilityRestricted=false omits the field entirely
// (OpenCode's own default "build" agent) -- the exact same
// present/omitted contract TestPostPromptAsync_PlanModeSelectsNativePlanAgent
// already pins for PlanMode, now pinned for this SECOND, independent
// selector too.
func TestPostPromptAsync_CapabilityRestrictedSelectsSentinelFixAgent(t *testing.T) {
	tests := []struct {
		name                 string
		capabilityRestricted bool
		wantAgent            *string
	}{
		{name: "capability restricted sends agent=sentinel-fix", capabilityRestricted: true, wantAgent: strPtr(sentinelFixAgentName)},
		{name: "capability unrestricted omits agent entirely", capabilityRestricted: false, wantAgent: nil},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var captured promptAsyncRequest
			var sawAgentKey bool

			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if !strings.HasSuffix(r.URL.Path, "/prompt_async") {
					w.WriteHeader(http.StatusOK)
					return
				}

				body, err := io.ReadAll(r.Body)
				if err != nil {
					t.Errorf("read request body: %v", err)
					w.WriteHeader(http.StatusInternalServerError)
					return
				}
				if err := json.Unmarshal(body, &captured); err != nil {
					t.Errorf("unmarshal request body: %v", err)
					w.WriteHeader(http.StatusInternalServerError)
					return
				}
				var raw map[string]json.RawMessage
				if err := json.Unmarshal(body, &raw); err != nil {
					t.Errorf("unmarshal request body as raw map: %v", err)
					w.WriteHeader(http.StatusInternalServerError)
					return
				}
				_, sawAgentKey = raw["agent"]
				w.WriteHeader(http.StatusOK)
			}))
			t.Cleanup(srv.Close)

			a := New(srv.URL, testSSEInactivityTimeout, testReconnectInterval, testRequestTimeout, testSummarizeTimeout, testTransientRetryBackoff, testRuntimeVersion, testSandboxID, tt.capabilityRestricted)
			t.Cleanup(a.Close)

			cmd := sandboxws.Prompt{Text: "fix the missing test coverage"}
			if err := a.postPromptAsync(t.Context(), "ses_test", cmd, nil); err != nil {
				t.Fatalf("postPromptAsync: %v", err)
			}

			if tt.wantAgent == nil {
				if sawAgentKey {
					t.Errorf("request body carried an \"agent\" key, want it omitted entirely (capabilityRestricted=false)")
				}
				return
			}

			if !sawAgentKey {
				t.Fatal("request body carried no \"agent\" key at all, want one present (capabilityRestricted=true)")
			}
			if captured.Agent == nil || *captured.Agent != *tt.wantAgent {
				got := "<nil>"
				if captured.Agent != nil {
					got = *captured.Agent
				}
				t.Errorf("Agent = %s, want %s", got, *tt.wantAgent)
			}
		})
	}
}

// TestPostPromptAsync_PlanModeTakesPrecedenceOverCapabilityRestricted
// proves the documented ordering in postPromptAsync's own switch
// (session.go): cmd.PlanMode is checked FIRST, so a plan-mode turn on a
// capability-restricted session still sends "agent":"plan", never
// "sentinel-fix" -- pinning the "PlanMode still takes precedence above"
// doc comment that was otherwise only ever asserted in prose.
func TestPostPromptAsync_PlanModeTakesPrecedenceOverCapabilityRestricted(t *testing.T) {
	var captured promptAsyncRequest

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.HasSuffix(r.URL.Path, "/prompt_async") {
			w.WriteHeader(http.StatusOK)
			return
		}
		body, err := io.ReadAll(r.Body)
		if err != nil {
			t.Errorf("read request body: %v", err)
			w.WriteHeader(http.StatusInternalServerError)
			return
		}
		if err := json.Unmarshal(body, &captured); err != nil {
			t.Errorf("unmarshal request body: %v", err)
			w.WriteHeader(http.StatusInternalServerError)
			return
		}
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(srv.Close)

	a := New(srv.URL, testSSEInactivityTimeout, testReconnectInterval, testRequestTimeout, testSummarizeTimeout, testTransientRetryBackoff, testRuntimeVersion, testSandboxID, true)
	t.Cleanup(a.Close)

	cmd := sandboxws.Prompt{Text: "do the thing", PlanMode: true}
	if err := a.postPromptAsync(t.Context(), "ses_test", cmd, nil); err != nil {
		t.Fatalf("postPromptAsync: %v", err)
	}

	if captured.Agent == nil || *captured.Agent != planAgentName {
		got := "<nil>"
		if captured.Agent != nil {
			got = *captured.Agent
		}
		t.Errorf("Agent = %s, want %s (PlanMode must take precedence over capabilityRestricted)", got, planAgentName)
	}
}
