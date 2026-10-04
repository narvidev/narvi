//go:build integration

// Real, subprocess-based integration tests for the checkout command
// (technical plan §21.1, §30.4): the real sandbox-agent binary, its real
// credential helper and a real git-http-backend, as push_integration_test.go
// runs them (that file's top comment says why the binary must be the real
// one). The code host is simulated as a pull request from a private fork
// whose owner never installed the App: the fork is never served, the base
// repository is served only to a token, and the base keeps the pull
// request's head as refs/pull/7/head.
package main

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/cgi"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/coder/websocket"
	"golang.org/x/sync/errgroup"

	"github.com/narvidev/narvi/contracts/gen/go/sandboxws"
)

// pullRefReadToken is the read-only installation token the base
// repository's installation serves a review sandbox.
const pullRefReadToken = "read-only-installation-token"

const pullRefTestRef = "refs/pull/7/head"

// privateForkPullRequest is a base repository, acme/widgets, whose main
// holds a file of its own (main-only.txt), and a contributor's private
// fork, contributor/widgets, whose main -- the pull request's head branch,
// named as the base's is -- holds S1 and then S2. The base keeps the pull
// request's head as refs/pull/7/head, at S1 until advancePullRef moves it.
type privateForkPullRequest struct {
	url          string
	baseBare     string
	forkWork     string
	main, s1, s2 string
	forkRequests atomic.Int32
	unauthorized atomic.Int32
}

func newPrivateForkPullRequest(t *testing.T) *privateForkPullRequest {
	t.Helper()
	parent := t.TempDir()
	f := &privateForkPullRequest{baseBare: filepath.Join(parent, "acme", "widgets.git")}
	forkBare := filepath.Join(parent, "contributor", "widgets.git")
	for _, bare := range []string{f.baseBare, forkBare} {
		if err := os.MkdirAll(bare, 0o755); err != nil {
			t.Fatalf("mkdir %s: %v", bare, err)
		}
		mustRunGit(t, parent, "init", "--bare", "-q", "-b", "main", bare)
	}

	commit := func(dir, file, content, message string) string {
		t.Helper()
		if err := os.WriteFile(filepath.Join(dir, file), []byte(content), 0o644); err != nil {
			t.Fatalf("write %s: %v", file, err)
		}
		mustRunGit(t, dir, "add", ".")
		mustRunGit(t, dir, "commit", "-q", "-m", message)
		return strings.TrimSpace(mustRunGit(t, dir, "rev-parse", "HEAD"))
	}

	baseWork := t.TempDir()
	mustRunGit(t, parent, "clone", "-q", f.baseBare, baseWork)
	commit(baseWork, "README.md", "base\n", "base")
	mustRunGit(t, baseWork, "push", "-q", "origin", "main")

	f.forkWork = t.TempDir()
	mustRunGit(t, parent, "clone", "-q", f.baseBare, f.forkWork)
	if err := os.WriteFile(filepath.Join(f.forkWork, ".gitignore"), []byte("*.log\n"), 0o644); err != nil {
		t.Fatalf("write .gitignore: %v", err)
	}
	f.s1 = commit(f.forkWork, "pr.txt", "first\n", "the pull request's first head")
	f.s2 = commit(f.forkWork, "pr.txt", "second\n", "the pull request's second head")
	mustRunGit(t, f.forkWork, "push", "-q", forkBare, "main")

	f.main = commit(baseWork, "main-only.txt", "main\n", "the base's main moves on")
	mustRunGit(t, baseWork, "push", "-q", "origin", "main")
	mustRunGit(t, f.forkWork, "push", "-q", f.baseBare, f.s1+":"+pullRefTestRef)

	f.url = f.serve(t, parent)
	return f
}

// serve serves parent through git-http-backend: everything under
// /contributor/ is answered 404 and counted -- the fork, which no
// installation can read -- and the base repository only to a request
// carrying the read token (or the write token, the only one that may push).
func (f *privateForkPullRequest) serve(t *testing.T, parent string) string {
	t.Helper()
	execPath, err := exec.Command("git", "--exec-path").Output()
	if err != nil {
		t.Fatalf("git --exec-path: %v", err)
	}
	backend := filepath.Join(strings.TrimSpace(string(execPath)), "git-http-backend")
	if _, err := os.Stat(backend); err != nil {
		t.Skipf("git-http-backend not available at %s, skipping: %v", backend, err)
	}
	cgiHandler := &cgi.Handler{Path: backend, Root: "/", Env: []string{"GIT_HTTP_EXPORT_ALL=1", "GIT_PROJECT_ROOT=" + parent}}
	server := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasPrefix(r.URL.Path, "/contributor/") {
			f.forkRequests.Add(1)
			http.NotFound(w, r)
			return
		}
		_, password, ok := r.BasicAuth()
		if !ok {
			f.unauthorized.Add(1)
			w.Header().Set("WWW-Authenticate", `Basic realm="test-git-server"`)
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		push := strings.Contains(r.URL.RawQuery, "service=git-receive-pack") || strings.HasSuffix(r.URL.Path, "git-receive-pack")
		if password != pushTestWriteToken && (push || password != pullRefReadToken) {
			http.Error(w, "this credential cannot do that here", http.StatusForbidden)
			return
		}
		cgiHandler.ServeHTTP(w, r)
	}))
	server.StartTLS()
	t.Cleanup(server.Close)
	return server.URL
}

// advancePullRef points the base repository's pull ref at sha, as the code
// host does when the contributor pushes to the fork.
func (f *privateForkPullRequest) advancePullRef(t *testing.T, sha string) {
	t.Helper()
	mustRunGit(t, f.forkWork, "push", "-q", "--force", f.baseBare, sha+":"+pullRefTestRef)
}

// sessionConfig is the review session's SESSION_CONFIG: the base
// repository, its pull ref, and -- deliberately -- the head branch's name,
// main, which the base also has as another commit, so an agent that
// honoured the branch over the ref would end on the base's main.
func (f *privateForkPullRequest) sessionConfig(cp *checkoutControlPlane) string {
	return fmt.Sprintf(`{
		"bootMode": "fresh",
		"controlPlaneWsUrl": %q,
		"correlationId": null,
		"gen": 1,
		"repos": [{"name": "widgets", "url": %q, "branch": "main", "ref": %q}],
		"sandboxId": "5b1c1e2e-6b1a-4b1a-9b1a-6b1a4b1a9b1a",
		"sandboxToken": "test-sandbox-token",
		"sessionId": %q
	}`, cp.wsURL(), f.url+"/acme/widgets.git", pullRefTestRef, cp.sessionID)
}

// checkoutControlPlane stands in for the control plane's sandbox WebSocket
// and scm-credentials endpoints: it serves the read token, hands the test
// the agent's ready, writes whatever commands the test sends, and passes
// on every event the agent sends.
type checkoutControlPlane struct {
	server    *httptest.Server
	sessionID string
	ready     chan json.RawMessage
	commands  chan []byte
	events    chan json.RawMessage
}

func newCheckoutControlPlane(t *testing.T, sessionID string) *checkoutControlPlane {
	t.Helper()
	cp := &checkoutControlPlane{
		sessionID: sessionID,
		ready:     make(chan json.RawMessage, 8),
		commands:  make(chan []byte, 8),
		events:    make(chan json.RawMessage, 256),
	}
	mux := http.NewServeMux()
	mux.HandleFunc("/sessions/"+sessionID+"/scm-credentials", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"username":  "x-access-token",
			"password":  pullRefReadToken,
			"expiresAt": time.Now().Add(time.Hour).Format(time.RFC3339),
		})
	})
	mux.HandleFunc("/sessions/"+sessionID+"/ws", func(w http.ResponseWriter, r *http.Request) {
		conn, err := websocket.Accept(w, r, &websocket.AcceptOptions{InsecureSkipVerify: true})
		if err != nil {
			return
		}
		defer func() { _ = conn.CloseNow() }()
		_, ready, err := conn.Read(r.Context())
		if err != nil {
			return
		}
		select {
		case cp.ready <- ready:
		default:
		}
		group, ctx := errgroup.WithContext(r.Context())
		group.Go(func() error {
			for {
				_, data, err := conn.Read(ctx)
				if err != nil {
					return err
				}
				select {
				case cp.events <- data:
				case <-ctx.Done():
					return ctx.Err()
				}
			}
		})
		group.Go(func() error {
			for {
				select {
				case cmd := <-cp.commands:
					if err := conn.Write(ctx, websocket.MessageText, cmd); err != nil {
						return err
					}
				case <-ctx.Done():
					return ctx.Err()
				}
			}
		})
		_ = group.Wait()
	})
	cp.server = httptest.NewServer(mux)
	t.Cleanup(cp.server.Close)
	return cp
}

func (cp *checkoutControlPlane) wsURL() string {
	return "ws" + strings.TrimPrefix(cp.server.URL, "http") + "/sessions/" + cp.sessionID + "/ws?type=sandbox"
}

// sendCheckout writes a checkout of sha to the agent.
func (cp *checkoutControlPlane) sendCheckout(t *testing.T, messageID, sha string) {
	t.Helper()
	payload, err := json.Marshal(sandboxws.Checkout{
		Type: "checkout", MessageId: messageID, SessionId: cp.sessionID, Gen: 1,
		Repos: []sandboxws.CheckoutReposElem{{Name: "widgets", Ref: pullRefTestRef, Sha: sha}},
	})
	if err != nil {
		t.Fatalf("marshal checkout: %v", err)
	}
	cp.commands <- payload
}

// awaitCheckoutResult reads the agent's events until the checkout_result
// answering messageID, decoded through the generated type.
func (cp *checkoutControlPlane) awaitCheckoutResult(t *testing.T, out *syncBuffer, messageID string) sandboxws.CheckoutResult {
	t.Helper()
	deadline := time.After(pushTestTimeout)
	for {
		select {
		case data := <-cp.events:
			var peek struct {
				Type      string `json:"type"`
				MessageID string `json:"messageId"`
			}
			if json.Unmarshal(data, &peek) != nil || peek.Type != "checkout_result" || peek.MessageID != "checkout_result:"+messageID {
				continue
			}
			var result sandboxws.CheckoutResult
			if err := json.Unmarshal(data, &result); err != nil {
				t.Fatalf("decode checkout_result %s: %v", data, err)
			}
			return result
		case <-deadline:
			t.Fatalf("timed out waiting for checkout_result:%s; sandbox-agent output:\n%s", messageID, out.String())
		}
	}
}

func worktreeHead(t *testing.T, dir string) string {
	t.Helper()
	return strings.TrimSpace(mustRunGit(t, dir, "rev-parse", "HEAD"))
}

// assertCheckedOut asserts the one repo of result was checked out at sha,
// with the ref's tip at refSHA.
func assertCheckedOut(t *testing.T, result sandboxws.CheckoutResult, sha, refSHA string, out *syncBuffer) {
	t.Helper()
	if len(result.Repos) != 1 {
		t.Fatalf("checkout_result repos = %+v, want one", result.Repos)
	}
	got := result.Repos[0]
	if got.Outcome != sandboxws.CheckoutResultReposElemOutcomeCheckedOut || got.HeadSha == nil || *got.HeadSha != sha ||
		got.RefSha == nil || *got.RefSha != refSHA || got.Error != nil {
		errText := ""
		if got.Error != nil {
			errText = *got.Error
		}
		t.Fatalf("checkout_result = %+v (error %q), want checked_out at %s with the ref at %s; sandbox-agent output:\n%s",
			got, errText, sha, refSHA, out.String())
	}
}

// TestHandleCheckout_PrivateForkWithoutTheApp_ReadFromTheBasePullRef: a
// review of a pull request from a private fork whose owner never installed
// the App. The sandbox holds only the base repository's read token, and
// the fork is never reachable; the boot reads the pull request's head from
// the base repository's pull ref, never its main, and a checkout of a
// later recorded head does the same. The agent advertises the checkout
// command, and never contacts the fork.
func TestHandleCheckout_PrivateForkWithoutTheApp_ReadFromTheBasePullRef(t *testing.T) {
	binPath := buildSandboxAgentBinary(t)
	f := newPrivateForkPullRequest(t)
	cp := newCheckoutControlPlane(t, "checkout-private-fork-session")
	workspaceDir := t.TempDir()
	out, exited := startSandboxAgent(t, binPath, f.sessionConfig(cp), workspaceDir)

	select {
	case ready := <-cp.ready:
		var r sandboxws.Ready
		if err := json.Unmarshal(ready, &r); err != nil || r.Capabilities == nil || r.Capabilities.ReviewCheckout == nil || !*r.Capabilities.ReviewCheckout {
			t.Errorf("ready = %s (%v), want capabilities.reviewCheckout true", ready, err)
		}
	case <-time.After(pushTestTimeout):
		t.Fatalf("no ready; sandbox-agent output:\n%s", out.String())
	}
	waitForBootComplete(t, out, exited)

	dir := filepath.Join(workspaceDir, "widgets")
	if head := worktreeHead(t, dir); head != f.s1 {
		t.Fatalf("boot left HEAD at %s, want the pull ref's tip %s (the base's main is %s); sandbox-agent output:\n%s", head, f.s1, f.main, out.String())
	}
	if _, err := os.Stat(filepath.Join(dir, "main-only.txt")); err == nil {
		t.Error("main-only.txt is in the worktree: the base's main was left checked out")
	}
	if f.unauthorized.Load() == 0 {
		t.Error("the base repository never challenged the clone: the test server is not gating it on the read token")
	}

	f.advancePullRef(t, f.s2)
	cp.sendCheckout(t, "c1", f.s2)
	assertCheckedOut(t, cp.awaitCheckoutResult(t, out, "c1"), f.s2, f.s2, out)
	if head := worktreeHead(t, dir); head != f.s2 {
		t.Errorf("HEAD after the checkout = %s, want %s", head, f.s2)
	}
	if n := f.forkRequests.Load(); n != 0 {
		t.Errorf("the fork was asked %d times, want never", n)
	}
}

// TestHandleCheckout_WarmReReview_PreviousTurnsEditsDiscarded: the same
// sandbox reviews again after a push. The previous turn edited a tracked
// file, staged another, deleted one and left untracked files behind, beside
// an ignored file setup installed; the checkout of the new head leaves
// exactly that head, clean, the ignored file kept.
func TestHandleCheckout_WarmReReview_PreviousTurnsEditsDiscarded(t *testing.T) {
	binPath := buildSandboxAgentBinary(t)
	f := newPrivateForkPullRequest(t)
	cp := newCheckoutControlPlane(t, "checkout-warm-rereview-session")
	workspaceDir := t.TempDir()
	out, exited := startSandboxAgent(t, binPath, f.sessionConfig(cp), workspaceDir)
	waitForBootComplete(t, out, exited)

	dir := filepath.Join(workspaceDir, "widgets")
	write := func(rel, content string) {
		t.Helper()
		path := filepath.Join(dir, rel)
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatalf("mkdir for %s: %v", rel, err)
		}
		if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
			t.Fatalf("write %s: %v", rel, err)
		}
	}
	write("pr.txt", "the previous turn's edit\n")
	write("README.md", "a staged edit\n")
	mustRunGit(t, dir, "add", "README.md")
	if err := os.Remove(filepath.Join(dir, ".gitignore")); err != nil {
		t.Fatalf("delete .gitignore: %v", err)
	}
	write("untracked.txt", "left behind\n")
	write("scratch/nested.txt", "left behind\n")
	write("deps.log", "installed by setup\n")

	f.advancePullRef(t, f.s2)
	cp.sendCheckout(t, "c1", f.s2)
	assertCheckedOut(t, cp.awaitCheckoutResult(t, out, "c1"), f.s2, f.s2, out)

	if head := worktreeHead(t, dir); head != f.s2 {
		t.Errorf("HEAD = %s, want %s", head, f.s2)
	}
	if status := strings.TrimSpace(mustRunGit(t, dir, "status", "--porcelain")); status != "" {
		t.Errorf("status = %q, want clean: the previous turn's work survived", status)
	}
	for _, gone := range []string{"untracked.txt", "scratch"} {
		if _, err := os.Stat(filepath.Join(dir, gone)); err == nil {
			t.Errorf("%s survived the checkout", gone)
		}
	}
	if body, err := os.ReadFile(filepath.Join(dir, "pr.txt")); err != nil || string(body) != "second\n" {
		t.Errorf("pr.txt = %q (%v), want S2's", body, err)
	}
	if _, err := os.Stat(filepath.Join(dir, "deps.log")); err != nil {
		t.Errorf("deps.log, an ignored file, did not survive: %v", err)
	}
}
