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

// TestPostPromptAsync_PlanModeSelectsNativePlanAgent proves this Step's own
// real wiring: cmd.PlanMode == true makes postPromptAsync (session.go)
// send OpenCode's own native "agent":"plan" field on prompt_async — the
// empirically-verified, structurally-enforced plan/build split
// (promptAsyncRequest's own doc comment, types.go) — while cmd.PlanMode ==
// false omits the field entirely (OpenCode's own default "build" agent).
// A minimal, self-contained httptest.Server capturing the raw request
// body (rather than fake_server_test.go's own fuller SSE-liveness fake,
// unneeded for this one request-shape assertion) mirrors this package's
// own established httptest.Server-backed fake style (fake_server_test.
// go's own doc comment) for exactly the same reason: deterministically
// observing one outbound request body needs no real/live OpenCode
// process.
func TestPostPromptAsync_PlanModeSelectsNativePlanAgent(t *testing.T) {
	tests := []struct {
		name      string
		planMode  bool
		wantAgent *string
	}{
		{name: "plan mode true sends agent=plan", planMode: true, wantAgent: strPtr("plan")},
		{name: "plan mode false omits agent entirely", planMode: false, wantAgent: nil},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var captured promptAsyncRequest
			var sawAgentKey bool

			// This adapter's background persistent SSE loop (started by New,
			// below) also hits this same test server (GET /event) the moment
			// it's constructed -- only the prompt_async request itself is
			// ever inspected as this test's own body; every other path gets
			// a bare 200 (GET /event) so that background loop doesn't spam
			// reconnect warnings or race this test's own captured/sawAgentKey
			// locals.
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

			a := New(srv.URL, testSSEInactivityTimeout, testReconnectInterval, testRequestTimeout, testSummarizeTimeout, testTransientRetryBackoff, testRuntimeVersion, testSandboxID)
			t.Cleanup(a.Close)

			cmd := sandboxws.Prompt{Text: "do the thing", PlanMode: tt.planMode}
			if err := a.postPromptAsync(t.Context(), "ses_test", cmd, nil); err != nil {
				t.Fatalf("postPromptAsync: %v", err)
			}

			if tt.wantAgent == nil {
				if sawAgentKey {
					t.Errorf("request body carried an \"agent\" key, want it omitted entirely (PlanMode=false)")
				}
				return
			}

			if !sawAgentKey {
				t.Fatal("request body carried no \"agent\" key at all, want one present (PlanMode=true)")
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

func strPtr(s string) *string { return &s }
