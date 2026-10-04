// Unit tests for HandleCheckout's busy answers and its exclusion with a
// running turn (checkout.go), driven through the real commandHandler with
// a real *opencode.Adapter against a fake OpenCode whose prompt_async
// holds the turn open until the test releases it -- handleprompt_test.go's
// own method, with the request held instead of failed at once.
package main

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/narvidev/narvi/contracts/gen/go/sandboxws"
	"github.com/narvidev/narvi/contracts/gen/go/sessionconfig"
	"github.com/narvidev/narvi/internal/adapters/outbound/opencode"
	"github.com/narvidev/narvi/internal/platform"
	"github.com/narvidev/narvi/internal/sandboxagent/boot"
	"github.com/narvidev/narvi/internal/sandboxagent/wsbridge"
)

// heldOpenCodeServer is a fake OpenCode whose prompt_async request is held
// until release is called, so the turn that sent it is running for as
// long as the test needs; then it fails, and the turn ends.
type heldOpenCodeServer struct {
	srv         *httptest.Server
	received    chan struct{}
	releaseCh   chan struct{}
	receiveOnce sync.Once
	releaseOnce sync.Once
}

func newHeldOpenCodeServer(t *testing.T) *heldOpenCodeServer {
	t.Helper()
	f := &heldOpenCodeServer{received: make(chan struct{}), releaseCh: make(chan struct{})}
	f.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/session" && r.Method == http.MethodPost:
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(struct {
				ID string `json:"id"`
			}{ID: "ses_fake"})
		case strings.HasSuffix(r.URL.Path, "/prompt_async") && r.Method == http.MethodPost:
			f.receiveOnce.Do(func() { close(f.received) })
			select {
			case <-f.releaseCh:
			case <-r.Context().Done():
			}
			http.Error(w, "released: the turn ends here", http.StatusInternalServerError)
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(func() {
		f.release()
		f.srv.Close()
	})
	return f
}

func (f *heldOpenCodeServer) release() { f.releaseOnce.Do(func() { close(f.releaseCh) }) }

// recordedBootSignals is a bootSignals that records MarkBootComplete.
type recordedBootSignals struct{ marked bool }

func (*recordedBootSignals) ReportBootStarted()  {}
func (s *recordedBootSignals) MarkBootComplete() { s.marked = true }

// newCheckoutTestHandler is a commandHandler for one session with one repo,
// "widgets", and a never-dialed bridge (handleprompt_test.go explains why
// its sends are safe no-ops). adapter may be nil when no turn is run.
func newCheckoutTestHandler(t *testing.T, adapter *opencode.Adapter) *commandHandler {
	t.Helper()
	sessionCfg := sessionconfig.SessionConfig{
		ControlPlaneWsUrl: "ws://127.0.0.1:9/sessions/test-session/ws?type=sandbox",
		SessionId:         "test-session",
		SandboxToken:      "test-token",
		Gen:               4,
		Repos:             []sessionconfig.SessionConfigReposElem{{Name: "widgets", Url: "https://example.invalid/acme/widgets.git"}},
	}
	timeouts := platform.DefaultTimeouts()
	h := &commandHandler{
		adapter:  adapter,
		runCtx:   context.Background(),
		cfg:      boot.Config{SessionConfig: &sessionCfg, WorkspaceDir: t.TempDir()},
		timeouts: timeouts,
	}
	h.bridge = wsbridge.New(sessionCfg, "sbx-test", "test-agent-version", "test-image-digest", h,
		timeouts.SandboxWSDialTimeout, timeouts.SandboxWSHeartbeatInterval,
		timeouts.SandboxWSReconnectMinBackoff, timeouts.SandboxWSReconnectMaxBackoff)
	return h
}

func checkoutOf(messageID, repo string) sandboxws.Checkout {
	return sandboxws.Checkout{
		Type: "checkout", MessageId: messageID, SessionId: "test-session", Gen: 4,
		Repos: []sandboxws.CheckoutReposElem{{Name: repo, Ref: "refs/pull/7/head", Sha: "0123456789abcdef0123456789abcdef01234567"}},
	}
}

func soleRepoResult(t *testing.T, result sandboxws.CheckoutResult) (sandboxws.CheckoutResultReposElemOutcome, string) {
	t.Helper()
	if len(result.Repos) != 1 {
		t.Fatalf("checkout_result repos = %+v, want one", result.Repos)
	}
	errText := ""
	if result.Repos[0].Error != nil {
		errText = *result.Repos[0].Error
	}
	return result.Repos[0].Outcome, errText
}

// TestHandleCheckout_BeforeBootCompletes: until the boot has completed, a
// checkout touches nothing and answers busy, under its deterministic
// messageId; the moment the boot is marked complete -- just before the
// bridge says so -- it runs.
func TestHandleCheckout_BeforeBootCompletes(t *testing.T) {
	h := newCheckoutTestHandler(t, nil)

	result := h.checkout(checkoutOf("c1", "widgets"))
	if result.Type != "checkout_result" || result.MessageId != "checkout_result:c1" || result.CommandMessageId != "c1" ||
		result.SessionId != "test-session" || result.Gen != 4 {
		t.Errorf("checkout_result envelope = %+v, want checkout_result:c1 for c1 on test-session gen 4", result)
	}
	if outcome, errText := soleRepoResult(t, result); outcome != sandboxws.CheckoutResultReposElemOutcomeBusy || !strings.Contains(errText, "booting") {
		t.Fatalf("before the boot completed: outcome %q, error %q, want busy because it is booting", outcome, errText)
	}

	signals := &recordedBootSignals{}
	checkoutBootSignals{bootSignals: signals, handler: h}.MarkBootComplete()
	if !signals.marked {
		t.Error("the bridge was never told the boot completed")
	}
	// A repo the session does not have fails without touching git: the
	// checkout ran, which is all this asks.
	if outcome, errText := soleRepoResult(t, h.checkout(checkoutOf("c2", "elsewhere"))); outcome != sandboxws.CheckoutResultReposElemOutcomeFailed || !strings.Contains(errText, `"elsewhere"`) {
		t.Errorf("after the boot completed: outcome %q, error %q, want failed for the unknown repo", outcome, errText)
	}
}

// TestHandleCheckout_BusyWhileATurnRuns: while a turn runs, a checkout
// answers busy and touches nothing; once the turn has returned, it runs.
func TestHandleCheckout_BusyWhileATurnRuns(t *testing.T) {
	oc := newHeldOpenCodeServer(t)
	h := newCheckoutTestHandler(t, newTestAdapter(t, oc.srv.URL))
	checkoutBootSignals{bootSignals: &recordedBootSignals{}, handler: h}.MarkBootComplete()

	h.HandlePrompt(context.Background(), sandboxws.Prompt{Type: "prompt", MessageId: "p1", SessionId: "test-session", Gen: 4, Text: "review this"})
	select {
	case <-oc.received:
	case <-time.After(30 * time.Second):
		t.Fatal("the turn never reached OpenCode")
	}

	if outcome, errText := soleRepoResult(t, h.checkout(checkoutOf("c1", "widgets"))); outcome != sandboxws.CheckoutResultReposElemOutcomeBusy || !strings.Contains(errText, "turn") {
		t.Fatalf("while a turn runs: outcome %q, error %q, want busy because a turn is running", outcome, errText)
	}

	oc.release()
	if err := h.group.Wait(); err != nil {
		t.Fatalf("h.group.Wait(): %v", err)
	}
	if outcome, _ := soleRepoResult(t, h.checkout(checkoutOf("c2", "elsewhere"))); outcome != sandboxws.CheckoutResultReposElemOutcomeFailed {
		t.Errorf("after the turn returned: outcome %q, want the checkout run (failed for the unknown repo)", outcome)
	}
}

// TestHandleCheckout_ATurnWaitsForACheckoutInProgress: a turn whose prompt
// arrives during a checkout starts only once the checkout has finished.
// The checkout is stood in for by holding checkoutMu, as one does for its
// whole run.
func TestHandleCheckout_ATurnWaitsForACheckoutInProgress(t *testing.T) {
	oc := newHeldOpenCodeServer(t)
	h := newCheckoutTestHandler(t, newTestAdapter(t, oc.srv.URL))

	h.checkoutMu.Lock()
	h.HandlePrompt(context.Background(), sandboxws.Prompt{Type: "prompt", MessageId: "p1", SessionId: "test-session", Gen: 4, Text: "review this"})
	select {
	case <-oc.received:
		t.Fatal("the turn reached OpenCode while a checkout held the worktree")
	case <-time.After(500 * time.Millisecond):
	}
	h.checkoutMu.Unlock()

	select {
	case <-oc.received:
	case <-time.After(30 * time.Second):
		t.Fatal("the turn never started once the checkout was done")
	}
	oc.release()
	if err := h.group.Wait(); err != nil {
		t.Fatalf("h.group.Wait(): %v", err)
	}
}

func newTestAdapter(t *testing.T, baseURL string) *opencode.Adapter {
	t.Helper()
	timeouts := platform.DefaultTimeouts()
	adapter := opencode.New(baseURL, timeouts.SSEInactivityTimeout,
		timeouts.OpenCodeSSEReconnectInterval, timeouts.OpenCodeRequestTimeout,
		timeouts.OpenCodeSummarizeTimeout, timeouts.OpenCodeTransientRetryBackoff,
		"0.0.0-test", "sbx-test-0002")
	t.Cleanup(adapter.Close)
	return adapter
}
