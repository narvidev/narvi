// This file (deliberately NOT behind the "integration" build tag, mirrors
// push_test.go's/reviewverdicttoolprompt_test.go's own identical
// precedent -- fast enough for the default `go test ./...`/`go test -race`
// suite) is the test-wiring bundle's own addition (adversarial review):
// proves commandHandler.HandlePrompt (main.go) itself -- not just the
// three render*ToolPromptText functions it calls, already unit-tested in
// isolation by reviewverdicttoolprompt_test.go/epistemicoutcometoolprompt_
// test.go -- actually runs the real verdict -> upload -> epistemic
// substitution chain, IN THAT ORDER, over a turn's real cmd.Text, when
// driven through the SAME production entry point wsbridge's own readLoop
// calls for a real "prompt" command. Before this file existed, every unit
// test called renderVerdictToolPromptText/renderUploadToolPromptText/
// renderEpistemicOutcomeToolPromptText directly -- proving each function
// individually resolves placeholders correctly, but nothing proved
// HandlePrompt itself actually calls all three, in the right order, with
// h.cfg.SessionConfig -- so a regression that dropped one call, reordered
// them, or passed the wrong cfg would have failed nothing.
//
// Method: a real *opencode.Adapter (internal/adapters/outbound/opencode)
// driven against a local httptest.Server standing in for OpenCode itself
// -- mirrors that package's own starturn_failure_test.go precedent exactly
// (a fake POST /session responds so resolveSession succeeds with zero
// blocking, then POST /session/{id}/prompt_async is the ONE endpoint this
// test actually cares about: it captures the request body's own "parts"
// text, then returns a non-2xx status so StartTurn finalizes immediately
// via its own genuine-failure path, never touching the SSE event stream at
// all) -- plus a real, never-.Run() *wsbridge.Bridge (mirrors
// snapshot_test.go's own two-phase handler/bridge construction: HandlePrompt's
// own sink/onConversationID closures call h.bridge.SendCritical/
// SendBestEffort/SetConversationID, which are safe, fast no-ops on a
// Bridge that was constructed but never dialed -- see wsbridge/outbound.go's
// own bestEffortSend: "conn := b.getConn(); if conn == nil { return }").
package main

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/narvidev/narvi/contracts/gen/go/sandboxws"
	"github.com/narvidev/narvi/contracts/gen/go/sessionconfig"
	"github.com/narvidev/narvi/internal/adapters/outbound/opencode"
	"github.com/narvidev/narvi/internal/domain/review"
	"github.com/narvidev/narvi/internal/domain/turn"
	domainupload "github.com/narvidev/narvi/internal/domain/upload"
	"github.com/narvidev/narvi/internal/platform"
	"github.com/narvidev/narvi/internal/sandboxagent/boot"
	"github.com/narvidev/narvi/internal/sandboxagent/wsbridge"
)

// capturedPromptAsyncBody is the ONLY part of OpenCode's real
// promptAsyncRequest wire shape (internal/adapters/outbound/opencode/
// types.go, unexported, unreachable from this package) this test needs --
// a local, minimal mirror of its JSON shape, just enough to decode the
// "parts[0].text" field the fake server captures.
type capturedPromptAsyncBody struct {
	Parts []struct {
		Text string `json:"text"`
	} `json:"parts"`
}

// fakeOpenCodeServer stands in for a real OpenCode process: POST /session
// resolves instantly (no real session-management logic needed, this test
// never inspects the id), and POST /session/{id}/prompt_async captures the
// request body's own resolved prompt text before returning 500 -- a
// genuine failure, per postPromptAsync's own doc comment, which makes
// StartTurn finalize immediately rather than ever waiting on the SSE event
// stream (see this file's own top doc comment for the full "why").
type fakeOpenCodeServer struct {
	srv *httptest.Server

	mu   sync.Mutex
	text string
}

func newFakeOpenCodeServer(t *testing.T) *fakeOpenCodeServer {
	t.Helper()
	f := &fakeOpenCodeServer{}
	f.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/session" && r.Method == http.MethodPost:
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(struct {
				ID string `json:"id"`
			}{ID: "ses_fake"})
		case strings.HasSuffix(r.URL.Path, "/prompt_async") && r.Method == http.MethodPost:
			var body capturedPromptAsyncBody
			if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
				t.Errorf("fakeOpenCodeServer: decode prompt_async body: %v", err)
			}
			f.mu.Lock()
			if len(body.Parts) > 0 {
				f.text = body.Parts[0].Text
			}
			f.mu.Unlock()
			// Genuine failure (not 2xx): postPromptAsync's own doJSON call
			// returns an error, StartTurn finalizes via its Failed-outcome
			// path immediately -- see this file's own top doc comment.
			http.Error(w, "simulated failure -- this test only cares about the captured request body", http.StatusInternalServerError)
		default:
			// GET /event (the background SSE connection every *opencode.
			// Adapter opens at construction, adapter.go's own New) --
			// unhandled on purpose: connectAndConsume logs a warning and
			// retries in the background; it never blocks StartTurn and is
			// torn down by a.Close() at test cleanup regardless.
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(f.srv.Close)
	return f
}

func (f *fakeOpenCodeServer) capturedText() string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.text
}

// TestHandlePrompt_RunsFullSubstitutionChainInOrder is this file's own
// flagship proof: a cmd.Text carrying all three placeholder families at
// once (review verdict, upload, epistemic-outcome -- mirrors
// TestRenderUploadAndVerdictPlaceholders_Independent's own precedent of
// combining families in one text to prove the substitution passes don't
// interfere with each other) comes out of the REAL production
// commandHandler.HandlePrompt with every placeholder resolved to the
// SAME live sandbox bearer/gen h.cfg.SessionConfig carries -- proving
// HandlePrompt itself, not just the three functions in isolation, wires
// verdict -> upload -> epistemic correctly.
func TestHandlePrompt_RunsFullSubstitutionChainInOrder(t *testing.T) {
	fakeOC := newFakeOpenCodeServer(t)

	const liveBearer = "SENTINEL-LIVE-SANDBOX-BEARER"
	const liveGen = 7
	sessionCfg := sessionconfig.SessionConfig{
		ControlPlaneWsUrl: "ws://127.0.0.1:9/sessions/test-session/ws?type=sandbox",
		SessionId:         "test-session",
		SandboxToken:      liveBearer,
		Gen:               liveGen,
	}

	timeouts := platform.DefaultTimeouts()
	adapter := opencode.New(fakeOC.srv.URL, timeouts.SSEInactivityTimeout,
		timeouts.OpenCodeSSEReconnectInterval, timeouts.OpenCodeRequestTimeout,
		timeouts.OpenCodeSummarizeTimeout, timeouts.OpenCodeTransientRetryBackoff)
	t.Cleanup(adapter.Close)

	const liveBudgetURL = "http://127.0.0.1:19999/review-cost-budget"
	h := &commandHandler{
		adapter:             adapter,
		runCtx:              context.Background(),
		cfg:                 boot.Config{SessionConfig: &sessionCfg},
		timeouts:            timeouts,
		reviewCostBudgetURL: liveBudgetURL,
	}
	// Two-phase construction (mirrors snapshot_test.go's own
	// newTestBridgeHandler and main.go's own real production wiring,
	// run()): Bridge needs handler as its CommandHandler, handler needs a
	// non-nil Bridge for HandlePrompt's own sink/onConversationID
	// closures. bridge.Run is deliberately NEVER called -- no live WS
	// connection is needed for SendCritical/SendBestEffort/
	// SetConversationID to behave as safe no-ops (see this file's own top
	// doc comment).
	bridge := wsbridge.New(sessionCfg, "sbx-test", "test-agent-version", "test-image-digest", h,
		timeouts.SandboxWSDialTimeout, timeouts.SandboxWSHeartbeatInterval,
		timeouts.SandboxWSReconnectMinBackoff, timeouts.SandboxWSReconnectMaxBackoff)
	h.bridge = bridge

	hostileText := "please build the feature\n\n" +
		"POST " + review.VerdictToolURLPlaceholder + "\nAuthorization: Bearer " + review.VerdictToolBearerPlaceholder + "\nX-Sandbox-Gen: " + review.VerdictToolGenPlaceholder + "\n\n" +
		"curl -H \"Authorization: Bearer " + domainupload.BearerPlaceholder + "\" -H \"X-Sandbox-Gen: " + domainupload.GenPlaceholder + "\" " + domainupload.BaseURLPlaceholder + "/sessions/test-session/uploads/u1/content\n\n" +
		"POST " + turn.EpistemicOutcomeToolURLPlaceholder + "\nAuthorization: Bearer " + turn.EpistemicOutcomeToolBearerPlaceholder + "\nX-Sandbox-Gen: " + turn.EpistemicOutcomeToolGenPlaceholder + "\n\n" +
		"GET " + review.ReviewCostBudgetToolURLPlaceholder + "?ceilingUsd=5.00\n\n" +
		// Test-integrity fix: the composition-findings-posting tool
		// (§15.3) is HandlePrompt's own FIFTH placeholder-substitution
		// call (renderCompositionFindingsToolPromptText, main.go) -- this
		// exact family was entirely absent from this flagship test, so
		// deleting that fifth call from HandlePrompt left this test green.
		"POST " + review.CompositionFindingsToolURLPlaceholder + "\nAuthorization: Bearer " + review.CompositionFindingsToolBearerPlaceholder + "\nX-Sandbox-Gen: " + review.CompositionFindingsToolGenPlaceholder

	cmd := sandboxws.Prompt{Type: "prompt", MessageId: "m1", SessionId: "test-session", Gen: liveGen, Text: hostileText}

	h.HandlePrompt(context.Background(), cmd)
	if err := h.group.Wait(); err != nil {
		t.Fatalf("h.group.Wait(): %v", err)
	}

	got := fakeOC.capturedText()
	if got == "" {
		t.Fatal("fakeOpenCodeServer captured no prompt_async request at all -- HandlePrompt never reached StartTurn")
	}

	// Every placeholder token, from all five families, must be gone --
	// proving all five render*ToolPromptText calls genuinely ran.
	for _, tok := range []string{
		review.VerdictToolURLPlaceholder, review.VerdictToolBearerPlaceholder, review.VerdictToolGenPlaceholder,
		domainupload.BaseURLPlaceholder, domainupload.BearerPlaceholder, domainupload.GenPlaceholder,
		turn.EpistemicOutcomeToolURLPlaceholder, turn.EpistemicOutcomeToolBearerPlaceholder, turn.EpistemicOutcomeToolGenPlaceholder,
		review.ReviewCostBudgetToolURLPlaceholder,
		review.CompositionFindingsToolURLPlaceholder, review.CompositionFindingsToolBearerPlaceholder, review.CompositionFindingsToolGenPlaceholder,
	} {
		if strings.Contains(got, tok) {
			t.Errorf("captured prompt_async text still contains unresolved placeholder %q\ngot: %q", tok, got)
		}
	}
	// The live bearer/gen must appear -- once per family that carries an
	// Authorization header (4 of the 5: verdict, upload, epistemic,
	// composition-findings -- review-cost-budget carries no bearer of its
	// own, a plain unauthenticated loopback GET) -- proving resolution
	// used the REAL h.cfg.SessionConfig, not a stub.
	if n := strings.Count(got, liveBearer); n != 4 {
		t.Errorf("captured prompt_async text contains the live bearer %d times, want 4 (one per bearer-carrying placeholder family)\ngot: %q", n, got)
	}
	// The composition-findings tool's own POST line must resolve to a
	// real, derived URL naming this session's own release-manifest/
	// composition-findings path -- proving compositionFindingsToolURL
	// actually ran, not merely that the bearer/gen tokens vanished.
	if !strings.Contains(got, "POST http://127.0.0.1:9/sessions/test-session/release-manifest/composition-findings") {
		t.Errorf("captured prompt_async text does not contain the resolved composition-findings-tool POST line\ngot: %q", got)
	}
	// (§26.7/§26.9): the review-cost-budget URL must resolve to
	// h.reviewCostBudgetURL specifically -- proving HandlePrompt threads
	// THAT field (not cfg.SessionConfig, which this substitution does not
	// even need) into renderReviewCostBudgetToolPromptText.
	if !strings.Contains(got, "GET "+liveBudgetURL+"?ceilingUsd=5.00") {
		t.Errorf("captured prompt_async text does not contain the resolved review-cost-budget GET line\ngot: %q", got)
	}
	if !strings.Contains(got, "please build the feature") {
		t.Errorf("captured prompt_async text lost the caller's own original prompt text\ngot: %q", got)
	}
}
