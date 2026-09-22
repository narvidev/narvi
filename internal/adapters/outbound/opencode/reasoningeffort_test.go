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

// H1 (adversarial review): the entire reasoning-effort feature (§29.8) was
// a no-op at the last hop -- promptAsyncRequest (types.go) declared no
// "variant" field at all, and postPromptAsync (session.go) never read
// cmd.Effort, despite every upstream hop (columns, REST intake,
// BuildPromptPayload, the wire field, the workflow-engine echo) being
// correctly wired. This file pins the fix: a non-nil cmd.Effort must reach
// prompt_async's own "variant" field verbatim; a nil cmd.Effort must omit
// it entirely (mirroring "agent"'s own present/omitted contract,
// planmode_test.go's own established style).
//
// Mirrors TestPostPromptAsync_PlanModeSelectsNativePlanAgent's own exact
// httptest.Server-capturing-the-raw-request-body style -- deterministically
// observing one outbound request body needs no real/live OpenCode process.
func TestPostPromptAsync_EffortMapsToVariant(t *testing.T) {
	tests := []struct {
		name        string
		effort      *string
		wantVariant *string
	}{
		{name: "non-nil effort sends variant verbatim", effort: strPtr("high"), wantVariant: strPtr("high")},
		{name: "nil effort omits variant entirely", effort: nil, wantVariant: nil},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var captured promptAsyncRequest
			var sawVariantKey bool

			// This adapter's background persistent SSE loop (started by New,
			// below) also hits this same test server (GET /event) the moment
			// it's constructed -- only the prompt_async request itself is
			// ever inspected as this test's own body; every other path gets
			// a bare 200 (GET /event) so that background loop doesn't spam
			// reconnect warnings or race this test's own captured/
			// sawVariantKey locals.
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
				_, sawVariantKey = raw["variant"]
				w.WriteHeader(http.StatusOK)
			}))
			t.Cleanup(srv.Close)

			a := New(srv.URL, testSSEInactivityTimeout, testReconnectInterval, testRequestTimeout, testSummarizeTimeout, testTransientRetryBackoff, testRuntimeVersion, testSandboxID)
			t.Cleanup(a.Close)

			cmd := sandboxws.Prompt{Text: "do the thing", Effort: tt.effort}
			if err := a.postPromptAsync(t.Context(), "ses_test", cmd, nil); err != nil {
				t.Fatalf("postPromptAsync: %v", err)
			}

			if tt.wantVariant == nil {
				if sawVariantKey {
					t.Errorf("request body carried a \"variant\" key, want it omitted entirely (Effort=nil)")
				}
				return
			}

			if !sawVariantKey {
				t.Fatal("request body carried no \"variant\" key at all, want one present (Effort non-nil)")
			}
			if captured.Variant == nil || *captured.Variant != *tt.wantVariant {
				got := "<nil>"
				if captured.Variant != nil {
					got = *captured.Variant
				}
				t.Errorf("Variant = %s, want %s", got, *tt.wantVariant)
			}
		})
	}
}
