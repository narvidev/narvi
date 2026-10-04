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
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"golang.org/x/sync/errgroup"

	"github.com/narvidev/narvi/contracts/gen/go/sandboxws"
	"github.com/narvidev/narvi/contracts/gen/go/sessionconfig"
	"github.com/narvidev/narvi/internal/adapters/outbound/opencode"
	"github.com/narvidev/narvi/internal/platform"
	"github.com/narvidev/narvi/internal/sandboxagent/boot"
	"github.com/narvidev/narvi/internal/sandboxagent/gitdir"
	"github.com/narvidev/narvi/internal/sandboxagent/supervisor"
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

// recordedBootSignals is a bootSignals that records MarkBootComplete, and
// whether h's boot was already marked complete when it was called.
type recordedBootSignals struct {
	h                      *commandHandler
	marked                 bool
	bootCompleteWhenMarked bool
}

func (*recordedBootSignals) ReportBootStarted() {}
func (s *recordedBootSignals) MarkBootComplete() {
	s.marked = true
	if s.h != nil {
		s.bootCompleteWhenMarked = s.h.bootComplete.Load()
	}
}

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

	signals := &recordedBootSignals{h: h}
	checkoutBootSignals{bootSignals: signals, handler: h}.MarkBootComplete()
	if !signals.marked {
		t.Error("the bridge was never told the boot completed")
	}
	// The order the control plane relies on: once the bridge says the boot
	// completed, the sandbox reads Ready and a checkout may follow at once,
	// so it must already be accepted.
	if !signals.bootCompleteWhenMarked {
		t.Error("the bridge was told the boot completed before a checkout would run")
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

// newRepoCheckoutHandler is newCheckoutTestHandler with a real repo,
// "widgets", in its workspace, its agent git-dir seeded, so a checkout runs
// real git up to its fetch.
func newRepoCheckoutHandler(t *testing.T, adapter *opencode.Adapter) *commandHandler {
	t.Helper()
	h := newCheckoutTestHandler(t, adapter)
	h.sup = supervisor.New()
	h.cfg.RuntimeUID, h.cfg.RuntimeGID = uint32(os.Getuid()), uint32(os.Getgid())
	wt := filepath.Join(h.cfg.WorkspaceDir, "widgets")
	for _, args := range [][]string{
		{"init", "-q", "-b", "main", wt},
		{"-C", wt, "-c", "user.name=Test", "-c", "user.email=test@example.com", "commit", "-q", "--allow-empty", "-m", "base"},
	} {
		if out, err := exec.Command("git", args...).CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
	}
	root := filepath.Join(t.TempDir(), "gitdirs")
	if err := gitdir.EnsureRoot(root); err != nil {
		t.Fatalf("gitdir.EnsureRoot: %v", err)
	}
	h.layout = gitdir.Layout{Root: root, WorkspaceDir: h.cfg.WorkspaceDir}
	if err := gitdir.Seed(context.Background(), h.sup, h.layout.Repo("widgets"), h.cfg.SessionConfig.Repos[0].Url, nil,
		h.timeouts.GitSyncStepTimeout, h.timeouts.ProcessStopGracePeriod); err != nil {
		t.Fatalf("gitdir.Seed: %v", err)
	}
	return h
}

// heldFetch puts a git on PATH whose fetch marks that it has started,
// waits until release is called, then fails; every other call runs the
// real git. A checkout through it stays in its fetch for as long as a test
// needs it to.
type heldFetch struct {
	started     string
	releasePath string
}

func holdFetches(t *testing.T) *heldFetch {
	t.Helper()
	realGit, err := exec.LookPath("git")
	if err != nil {
		t.Fatalf("exec.LookPath(git): %v", err)
	}
	dir := t.TempDir()
	f := &heldFetch{started: filepath.Join(dir, "started"), releasePath: filepath.Join(dir, "release")}
	bin := filepath.Join(dir, "bin")
	if err := os.Mkdir(bin, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	script := "#!/bin/sh\nfor a in \"$@\"; do\n  if [ \"$a\" = fetch ]; then\n    : > \"" + f.started + "\"\n" +
		"    while [ ! -e \"" + f.releasePath + "\" ]; do sleep 0.05; done\n    exit 1\n  fi\ndone\nexec \"" + realGit + "\" \"$@\"\n"
	if err := os.WriteFile(filepath.Join(bin, "git"), []byte(script), 0o755); err != nil {
		t.Fatalf("write the holding git: %v", err)
	}
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
	t.Cleanup(f.release)
	return f
}

func (f *heldFetch) release() { _ = os.WriteFile(f.releasePath, nil, 0o644) }

func (f *heldFetch) waitStarted(t *testing.T) {
	t.Helper()
	deadline := time.Now().Add(30 * time.Second)
	for {
		if _, err := os.Stat(f.started); err == nil {
			return
		}
		if time.Now().After(deadline) {
			t.Fatal("the checkout's fetch never started")
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// TestHandleCheckout_RunsOffTheReadLoop: HandleCheckout is called on the
// bridge's read loop, and returns while its checkout is still in its
// fetch, so a stop or any other command behind it is read at once.
func TestHandleCheckout_RunsOffTheReadLoop(t *testing.T) {
	h := newRepoCheckoutHandler(t, nil)
	fetches := holdFetches(t)
	checkoutBootSignals{bootSignals: &recordedBootSignals{}, handler: h}.MarkBootComplete()

	returned := make(chan struct{})
	var readLoop errgroup.Group
	readLoop.Go(func() error {
		h.HandleCheckout(context.Background(), checkoutOf("c1", "widgets"))
		close(returned)
		return nil
	})
	fetches.waitStarted(t)
	select {
	case <-returned:
	case <-time.After(5 * time.Second):
		t.Error("HandleCheckout did not return while its checkout was still in its fetch: it holds the read loop")
	}
	fetches.release()
	if err := readLoop.Wait(); err != nil {
		t.Fatalf("readLoop.Wait(): %v", err)
	}
	if err := h.group.Wait(); err != nil {
		t.Fatalf("h.group.Wait(): %v", err)
	}
}

// TestHandleCheckout_ATurnWaitsForARealCheckout: a prompt that arrives
// while a checkout is running -- here held in its fetch -- starts its turn
// only once the checkout has finished, because the checkout holds the lock
// every turn takes to start.
func TestHandleCheckout_ATurnWaitsForARealCheckout(t *testing.T) {
	oc := newHeldOpenCodeServer(t)
	h := newRepoCheckoutHandler(t, newTestAdapter(t, oc.srv.URL))
	fetches := holdFetches(t)
	checkoutBootSignals{bootSignals: &recordedBootSignals{}, handler: h}.MarkBootComplete()

	h.HandleCheckout(context.Background(), checkoutOf("c1", "widgets"))
	fetches.waitStarted(t)
	h.HandlePrompt(context.Background(), sandboxws.Prompt{Type: "prompt", MessageId: "p1", SessionId: "test-session", Gen: 4, Text: "review this"})
	select {
	case <-oc.received:
		t.Fatal("the turn reached OpenCode while a checkout was rewriting the worktree")
	case <-time.After(500 * time.Millisecond):
	}

	fetches.release()
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

// TestCheckoutRepoResult_CapsTheErrorText: a repo's error is capped as a
// critical event's text is, so a result naming several repos fits the
// smallest frame a control plane reads; a sha that is "" is null, and a
// checked_out repo carries no error.
func TestCheckoutRepoResult_CapsTheErrorText(t *testing.T) {
	failed := checkoutRepoResult("widgets", sandboxws.CheckoutResultReposElemOutcomeFetchFailed, "", "", strings.Repeat("x", 10000))
	if failed.Error == nil || len(*failed.Error) > 4096 || !strings.HasSuffix(*failed.Error, "...[truncated]") {
		t.Errorf("error of %d bytes, want it capped at 4096 and marked truncated", len(deref(failed.Error)))
	}
	if failed.HeadSha != nil || failed.RefSha != nil {
		t.Errorf("headSha, refSha = %v, %v, want null for \"\"", failed.HeadSha, failed.RefSha)
	}
	sha := strings.Repeat("a", 40)
	ok := checkoutRepoResult("widgets", sandboxws.CheckoutResultReposElemOutcomeCheckedOut, sha, sha, "")
	if ok.Error != nil || ok.HeadSha == nil || *ok.HeadSha != sha {
		t.Errorf("checked_out entry = %+v, want no error and its head", ok)
	}
}

func deref(s *string) string {
	if s == nil {
		return ""
	}
	return *s
}
