//go:build integration

// Real, subprocess-based integration test for HandlePush (§9.3, "e2e
// happy path", design decision 7). This deliberately spawns the REAL,
// compiled sandbox-agent binary as a separate OS process (rather than
// calling commandHandler.HandlePush directly, in-process, from this test
// binary) for one load-bearing reason: internal/sandboxagent/gitclone.
// CredHelperGitArg (and HandlePush's own reuse of it) resolves "the
// currently running binary's own absolute path" via os.Executable() --
// inside a `go test`-compiled binary that resolves to the TEST binary
// itself, whose actual `func main()` is `go test`'s own generated test
// runner, not this package's real main()/runCredentialHelper dispatch.
// Re-invoking the test binary with `credential-helper get` args would
// therefore NOT exercise the real credential-helper code path at all --
// only running the REAL, separately-built production binary makes
// os.Executable() resolve correctly and lets git's own `-c
// credential.helper=!'<path>' credential-helper` re-invocation reach the
// real runCredentialHelper dispatch. This is exactly the same reasoning
// design decision 13's own e2e proof is built on.
//
// This test lets the sandbox-agent SUBPROCESS itself perform the real
// clone (its own real boot sequence, unmodified) -- the test never
// pre-populates the workspace directory itself (that would collide with
// `git clone` refusing to clone into an already-non-empty directory). It
// instead polls the filesystem for the clone to complete, makes ONE new
// local commit itself (standing in for "the agent did some work"), and
// only then signals the fake control-plane to send the "push" command --
// otherwise the WS bridge (started BEFORE cloning/booting, by main.go's
// own design) could deliver "push" before there is anything to push at
// all, or before the target directory even exists.
//
// # Honest scope gap
//
// This is a scoped-down proof of the sandbox-agent PUSH half only (a real
// subprocess clone -> a real local commit -> a real `git push` -> reading
// back a real HEAD sha). The FULL local-subprocess end-to-end proof this
// Step's own brief describes -- a real REST session creation, driving a
// real control-plane binary, a real OpenCode turn actually running, and a
// real githubapi PR creation, all stitched together in one test -- was NOT
// attempted this Step. Building that fuller test is not attempted here
// either; this comment exists so the gap is visible in the code itself,
// matching this codebase's own established honest-gap-documentation
// discipline (see e.g. this package's own boot/gitclone doc comments),
// not left to a separate report only.
package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/cgi"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/coder/websocket"
	"github.com/google/uuid"
	"golang.org/x/sync/errgroup"

	"github.com/narvidev/narvi/contracts/gen/go/sandboxws"
	"github.com/narvidev/narvi/internal/platform"
)

const pushTestTimeout = 45 * time.Second

// buildSandboxAgentBinary compiles this SAME package into a real,
// standalone binary at a temp path, once per test -- required so
// os.Executable() (see this file's own top comment) resolves to a real
// production binary, not the go-test-generated one.
func buildSandboxAgentBinary(t *testing.T) string {
	t.Helper()
	binPath := filepath.Join(t.TempDir(), "sandbox-agent-under-test")
	cmd := exec.Command("go", "build", "-o", binPath, ".")
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		t.Fatalf("go build sandbox-agent: %v\n%s", err, stderr.String())
	}
	return binPath
}

// mustRunGit runs git with args in dir, configured with a throwaway
// commit identity, failing the test on any error.
func mustRunGit(t *testing.T, dir string, args ...string) string {
	t.Helper()
	fullArgs := append([]string{"-c", "user.name=Test", "-c", "user.email=test@example.com"}, args...)
	cmd := exec.Command("git", fullArgs...)
	cmd.Dir = dir
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %s (dir=%s): %v\n%s", strings.Join(args, " "), dir, err, out)
	}
	return string(out)
}

// startGitHTTPServer serves reposParent via git's own smart-HTTP backend
// (git-http-backend, via net/http/cgi -- a real git server, not a mock of
// one), gating ONLY the git-receive-pack half (push) behind "any
// Authorization header present" -- clone/fetch (git-upload-pack) is left
// open. This is deliberately loose (it does not verify the credential's
// own VALUE): internal/sandboxagent/credentials' own tests already
// thoroughly prove the credential VALUE flows correctly end to end; this
// test's own job is proving HandlePush's new git-push/rev-parse/event-
// reporting logic, with a real credential round trip as supporting
// infrastructure it depends on, not the primary thing under test.
//
// Deliberately TLS (httptest.NewUnstartedServer + StartTLS), not plain
// HTTP: internal/sandboxagent/credentials.Get REFUSES to answer for any
// protocol other than "https" (§5.2: "scoped https+host only") -- a
// plain-http test server would make git's own credential descriptor
// report protocol=http, and our real credential helper would then
// correctly (by its own documented contract) offer NOTHING, exactly as it
// should for a real production http:// remote. Matching real GitHub usage
// requires https here. The self-signed cert is trusted via
// GIT_SSL_NO_VERIFY (see runSandboxAgent's own env) -- acceptable ONLY
// because this is a throwaway test server, never anything resembling
// production configuration.
func startGitHTTPServer(t *testing.T, reposParent string) *httptest.Server {
	t.Helper()

	execPathOut, err := exec.Command("git", "--exec-path").Output()
	if err != nil {
		t.Fatalf("git --exec-path: %v", err)
	}
	backendPath := filepath.Join(strings.TrimSpace(string(execPathOut)), "git-http-backend")
	if _, err := os.Stat(backendPath); err != nil {
		t.Skipf("git-http-backend not available at %s, skipping: %v", backendPath, err)
	}

	cgiHandler := &cgi.Handler{
		Path: backendPath,
		Root: "/",
		Env: []string{
			"GIT_HTTP_EXPORT_ALL=1",
			"GIT_PROJECT_ROOT=" + reposParent,
		},
	}

	server := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		needsAuth := strings.Contains(r.URL.RawQuery, "service=git-receive-pack") ||
			strings.HasSuffix(r.URL.Path, "git-receive-pack")
		if needsAuth {
			if _, _, ok := r.BasicAuth(); !ok {
				w.Header().Set("WWW-Authenticate", `Basic realm="test-git-server"`)
				w.WriteHeader(http.StatusUnauthorized)
				return
			}
		}
		cgiHandler.ServeHTTP(w, r)
	}))
	server.StartTLS()
	t.Cleanup(server.Close)
	return server
}

// fakeControlPlane stands in for the real control-plane's sandbox-WS
// endpoint and scm-credentials endpoint (design decision 8): accepts the
// real wsbridge.Bridge's handshake (no validation -- this test only cares
// about the push round trip, not the handshake's own already-covered-
// elsewhere auth rules), reads the bridge's first "ready" event, waits
// for the test's own readyToPush signal (see this file's own top
// comment), sends a real "push" command, then waits for the resulting
// push_complete/push_error frame.
type fakeControlPlane struct {
	server               *httptest.Server
	sessionID            string
	credentialShouldFail bool
	readyToPush          chan struct{}
	result               chan json.RawMessage
}

func newFakeControlPlane(t *testing.T, sessionID string, credentialShouldFail bool) *fakeControlPlane {
	t.Helper()
	fcp := &fakeControlPlane{
		sessionID:            sessionID,
		credentialShouldFail: credentialShouldFail,
		readyToPush:          make(chan struct{}),
		result:               make(chan json.RawMessage, 1),
	}

	mux := http.NewServeMux()
	mux.HandleFunc("/sessions/"+sessionID+"/scm-credentials", func(w http.ResponseWriter, r *http.Request) {
		if fcp.credentialShouldFail {
			http.Error(w, "no credential available for this session", http.StatusForbidden)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"username":  "x-access-token",
			"password":  "fake-oauth-access-token",
			"expiresAt": time.Now().Add(time.Hour).Format(time.RFC3339),
		})
	})
	mux.HandleFunc("/sessions/"+sessionID+"/ws", func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("type") != "sandbox" {
			http.Error(w, "unsupported ws type", http.StatusBadRequest)
			return
		}
		conn, err := websocket.Accept(w, r, &websocket.AcceptOptions{InsecureSkipVerify: true})
		if err != nil {
			return
		}
		defer func() { _ = conn.CloseNow() }()

		ctx := r.Context()
		if _, _, err := conn.Read(ctx); err != nil { // the bridge's own "ready" event
			return
		}

		select {
		case <-fcp.readyToPush:
		case <-ctx.Done():
			return
		case <-time.After(pushTestTimeout):
			return
		}

		pushCmd := sandboxws.Push{
			Type:      "push",
			MessageId: uuid.NewString(),
			SessionId: sessionID,
			Gen:       1,
			Repos:     []sandboxws.PushReposElem{{Name: "widgets", Branch: "main"}},
		}
		payload, err := json.Marshal(pushCmd)
		if err != nil {
			return
		}
		if err := conn.Write(ctx, websocket.MessageText, payload); err != nil {
			return
		}

		for {
			_, data, err := conn.Read(ctx)
			if err != nil {
				return
			}
			var peek struct {
				Type string `json:"type"`
			}
			if err := json.Unmarshal(data, &peek); err != nil {
				continue
			}
			if peek.Type == "push_complete" || peek.Type == "push_error" {
				cp := make(json.RawMessage, len(data))
				copy(cp, data)
				select {
				case fcp.result <- cp:
				default:
				}
				return
			}
			// heartbeats and anything else: keep reading.
		}
	})

	fcp.server = httptest.NewServer(mux)
	t.Cleanup(fcp.server.Close)
	return fcp
}

func (fcp *fakeControlPlane) wsURL() string {
	return "ws" + strings.TrimPrefix(fcp.server.URL, "http") + "/sessions/" + fcp.sessionID + "/ws?type=sandbox"
}

// setUpBareRepoAndServer creates a real bare repo (with an initial commit
// on "main") and serves it over a real git-http-backend server. The
// sandbox-agent SUBPROCESS itself performs the actual clone into its own
// workspace directory (see this file's own top comment for why this test
// never pre-populates that directory itself).
func setUpBareRepoAndServer(t *testing.T) (gitServerURL string) {
	t.Helper()

	reposParent := t.TempDir()
	bareRepoDir := filepath.Join(reposParent, "repo.git")
	if err := os.MkdirAll(bareRepoDir, 0o755); err != nil {
		t.Fatalf("mkdir bare repo dir: %v", err)
	}
	mustRunGit(t, reposParent, "init", "--bare", "-b", "main", bareRepoDir)
	// git-http-backend refuses receive-pack (push) over HTTP by default,
	// as a safety default -- must be explicitly opted into per repo.
	mustRunGit(t, bareRepoDir, "config", "http.receivepack", "true")

	// Seed an initial commit via a throwaway LOCAL (file-path) clone --
	// this push never touches the HTTP server at all, so it needs no auth
	// gate consideration.
	seedDir := t.TempDir()
	mustRunGit(t, reposParent, "clone", bareRepoDir, seedDir)
	if err := os.WriteFile(filepath.Join(seedDir, "README.md"), []byte("seed\n"), 0o644); err != nil {
		t.Fatalf("write seed file: %v", err)
	}
	mustRunGit(t, seedDir, "add", "README.md")
	mustRunGit(t, seedDir, "commit", "-m", "seed commit")
	mustRunGit(t, seedDir, "push", "origin", "main")

	gitServer := startGitHTTPServer(t, reposParent)
	return gitServer.URL
}

// waitForRealClone polls for the sandbox-agent subprocess's OWN real
// `git clone` (internal/sandboxagent/gitclone.CloneAll, unmodified) to
// FULLY finish populating workspaceDir/widgets, bounded by
// pushTestTimeout. Polls for the seed file (README.md) rather than just
// the .git directory's own existence: `git clone` creates .git fairly
// early (once objects are fetched) but finishes checking out working-tree
// files -- and writing HEAD -- afterward; racing that window with this
// test's own `git commit` in the SAME repo produced a real, observed
// "cannot lock ref 'HEAD': reference already exists" failure during this
// test's own development. Waiting for the checked-out working-tree file
// itself is a strictly later, unambiguous completion signal.
func waitForRealClone(t *testing.T, workspaceDir string) {
	t.Helper()
	seedFile := filepath.Join(workspaceDir, "widgets", "README.md")
	deadline := time.Now().Add(pushTestTimeout)
	for {
		if _, err := os.Stat(seedFile); err == nil {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for the real sandbox-agent clone to check out %s", seedFile)
		}
		time.Sleep(100 * time.Millisecond)
	}
}

// runSandboxAgent starts the real sandbox-agent binary against a real
// SESSION_CONFIG pointing at fcp, returning a buffer capturing its
// combined output for diagnostics.
//
// Both the normal per-test-completion path (t.Cleanup below) and the
// pushTestTimeout path (cmd.Cancel/cmd.WaitDelay) stop it via
// stopProcessGroup (processgroup_test.go): SIGTERM the whole process
// group first, wait up to timeouts.SupervisorShutdownTimeout, only then
// escalate to SIGKILL. An earlier version of this helper called only
// cmd.Process.Kill() (an unconditional SIGKILL, no process group) -- since
// SIGKILL cannot be intercepted, sandbox-agent never got a chance to run
// its own already-correct graceful shutdown (main.go's
// signal.NotifyContext -> sup.StopAll), which is what actually stops
// whatever OpenCode/git/hook processes it had spawned (each in its own
// process group, per internal/sandboxagent/supervisor.Spawn) -- silently
// orphaning them instead. Sending SIGTERM first, and waiting the same
// outer bound sandbox-agent's own StopAll is itself bounded by (main.go's
// own shutdownCtx, built from SupervisorShutdownTimeout, NOT the shorter
// per-process ProcessStopGracePeriod -- see timeouts.go's own doc comment
// distinguishing the two), gives that existing graceful path a real
// chance to run; SIGKILL is now only a backstop for sandbox-agent itself
// failing to exit in time, not the default.
//
// Honest limit: none of this helps if the TEST BINARY itself (this `go
// test` process) is killed abruptly -- a SIGKILL of it, or any other path
// that skips t.Cleanup and never lets cmd.Cancel run, still leaves
// sandbox-agent (and transitively whatever it hasn't yet stopped itself)
// running. That failure mode is not recoverable from inside the test
// process; see helpers_test.go's own startServer doc comment (internal/
// adapters/outbound/opencode) for the same caveat in the in-process-spawn
// case.
// syncBuffer is a mutex-guarded io.Writer whose contents can be read back
// safely WHILE the writer is still being written to.
//
// This exists because os/exec, for any cmd.Stdout/cmd.Stderr that is not
// an *os.File, creates an OS pipe and spawns its OWN goroutine to copy
// that pipe into the supplied io.Writer, running until the child exits
// and cmd.Wait returns. runSandboxAgent below deliberately calls cmd.Wait
// in a BACKGROUND goroutine (it must stay non-blocking so the test can
// drive the child), so that copier goroutine is live for the whole body
// of every test using it -- while each of those tests reads the captured
// output back, from the TEST goroutine, inside its own failure paths
// ("...; sandbox-agent output:\n%s").
//
// With a plain bytes.Buffer (which is NOT safe for concurrent use) those
// two accesses are an unsynchronized write/read pair on the same value: a
// genuine data race that -race reports with a stack trace through
// bytes.Buffer. Because the reads sit only on failure paths, the race
// fired only when a test was ALREADY failing (or had timed out), which
// replaced the diagnostic output those call sites exist to print with a
// confusing race report about bytes.Buffer instead -- masking the real
// failure rather than explaining it.
type syncBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *syncBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *syncBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

func runSandboxAgent(t *testing.T, binPath, gitServerURL, workspaceDir string, fcp *fakeControlPlane) *syncBuffer {
	t.Helper()

	sessionConfigJSON := fmt.Sprintf(`{
		"bootMode": "fresh",
		"controlPlaneWsUrl": %q,
		"correlationId": null,
		"gen": 1,
		"repos": [{"name": "widgets", "url": %q, "branch": null}],
		"sandboxId": "5b1c1e2e-6b1a-4b1a-9b1a-6b1a4b1a9b1a",
		"sandboxToken": "test-sandbox-token",
		"sessionId": %q
	}`, fcp.wsURL(), gitServerURL+"/repo.git", fcp.sessionID)

	credCacheDir := t.TempDir()

	timeouts := platform.DefaultTimeouts()

	ctx, cancel := context.WithTimeout(context.Background(), pushTestTimeout)
	t.Cleanup(cancel)

	cmd := exec.CommandContext(ctx, binPath)
	cmd.Env = append(os.Environ(),
		"NARVI_BOOT_MODE=fresh",
		"NARVI_SESSION_CONFIG="+sessionConfigJSON,
		"NARVI_WORKSPACE_DIR="+workspaceDir,
		"NARVI_CREDENTIAL_CACHE_DIR="+credCacheDir,
		// §30.5: boot.Config.GitDirRoot defaults to
		// /var/lib/narvi/gitdirs, which an ordinary unprivileged `go test`
		// process cannot create/write on a real dev machine or CI runner --
		// gitdir.EnsureRoot would fail boot outright. Pointed at a fresh
		// t.TempDir() instead, exactly like every other *_test.go call site
		// in this codebase that builds a gitdir.Layout of its own.
		"NARVI_GIT_DIR_ROOT="+t.TempDir(),
		// TECHNICAL_PLAN.md §30.5 ("OS-level isolation between
		// sandbox-agent and the agent runtime"): this real subprocess
		// spawns a real `opencode serve` for its own agent runtime, which
		// main.go now drops to boot.Config.RuntimeUID/RuntimeGID via a
		// *syscall.Credential -- a genuine kernel-enforced uid change,
		// which requires CAP_SETUID/root. This test process (an ordinary
		// `go test` run, unprivileged, exactly like every other
		// integration test in this file) is not root, so the default
		// target uid/gid (65534, "nobody") would make the subprocess's
		// own opencode spawn fail outright with "operation not
		// permitted" -- confirmed live: this is exactly what broke this
		// test the first time this Step's own runtimeCredential wiring
		// landed. Setting these to THIS test process's own current
		// identity is the documented escape hatch (see that Credential's
		// own construction in main.go): a uid/gid Credential naming the
		// SAME identity the process already has changes nothing and
		// needs no privilege at all, so opencode spawns exactly as it
		// did before this Step, while the real cross-uid enforcement
		// itself stays proven elsewhere (internal/sandboxagent/
		// supervisor and opencodeproc's own rooted-Linux-container
		// tests) -- this test's own job is HandlePush, not sandbox
		// isolation.
		fmt.Sprintf("NARVI_RUNTIME_UID=%d", os.Getuid()),
		fmt.Sprintf("NARVI_RUNTIME_GID=%d", os.Getgid()),
		// The git-http-backend test server (startGitHTTPServer) uses a
		// real but self-signed TLS cert -- trusted here ONLY because this
		// is a throwaway test server, never anything resembling
		// production configuration. Inherited by every git subprocess
		// (clone, push, rev-parse) via supervisor.Spec's own nil-Env
		// "inherit this process's environment" convention.
		"GIT_SSL_NO_VERIFY=true",
	)
	// syncBuffer, not bytes.Buffer: os/exec writes this from its own copier
	// goroutine while the tests below read it back from the test goroutine.
	// See syncBuffer's own doc comment. Assigning the SAME writer to both
	// Stdout and Stderr additionally makes os/exec reuse ONE pipe and ONE
	// copier goroutine for the pair (it compares the two interface values),
	// so the interleaving of the child's stdout and stderr is preserved.
	var out syncBuffer
	cmd.Stdout = &out
	cmd.Stderr = &out

	// Own process group (mirrors internal/sandboxagent/supervisor.Spawn's
	// own SysProcAttr{Setpgid: true} for every child IT spawns) so
	// SIGTERM/SIGKILL aimed at cmd.Process.Pid below can never
	// accidentally reach this TEST BINARY's own process group too, and so
	// cmd.Process.Pid itself doubles as the group id stopProcessGroup
	// needs.
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}

	// cmd.Cancel/WaitDelay (Go 1.20+) fire if ctx (pushTestTimeout) is hit
	// before the process exits on its own. Without them,
	// exec.CommandContext's default Cancel is an unconditional, immediate
	// cmd.Process.Kill() (SIGKILL) -- exactly today's leak, just on the
	// timeout path instead of the t.Cleanup path. Sending SIGTERM to the
	// group here instead gives sandbox-agent's own graceful shutdown the
	// same real chance to run on this path too.
	cmd.Cancel = func() error {
		return signalProcessGroup(cmd.Process.Pid, syscall.SIGTERM)
	}
	cmd.WaitDelay = timeouts.SupervisorShutdownTimeout

	if err := cmd.Start(); err != nil {
		t.Fatalf("start sandbox-agent: %v", err)
	}

	// waitDone is closed exactly once, by this single background cmd.
	// Wait() call -- exec.Cmd.Wait must never be called twice, so every
	// OTHER path (this func's own t.Cleanup escalation below, and
	// cmd.Cancel above) only ever SIGNALS the process group, never calls
	// Wait itself.
	//
	// errgroup.Group.Go, not a bare `go` statement: §11's no-naked-
	// goroutine rule (tools/lint/narvichecks/nakedgoroutine) applies to
	// tests too -- mirrors internal/sandboxagent/supervisor.Supervisor's
	// own `group` field precedent exactly (Spawn's reap goroutine): this
	// local Group exists solely as a lint-satisfying Go() call site, never
	// Wait()ed on -- waitDone, closed from inside the goroutine, is this
	// function's own actual synchronization signal.
	waitDone := make(chan struct{})
	var reapGroup errgroup.Group
	reapGroup.Go(func() error {
		_ = cmd.Wait()
		close(waitDone)
		return nil
	})

	t.Cleanup(func() {
		stopProcessGroup(cmd.Process.Pid, waitDone, timeouts.SupervisorShutdownTimeout)
	})

	return &out
}

// TestHandlePush_RealGitPush_Success proves a real `git push` (via the
// real sandbox-agent binary, a real git-http-backend server, and a real
// scm-credentials round trip) succeeds and produces a real PushComplete
// carrying the actual resulting HEAD sha.
func TestHandlePush_RealGitPush_Success(t *testing.T) {
	binPath := buildSandboxAgentBinary(t)
	gitServerURL := setUpBareRepoAndServer(t)
	workspaceDir := t.TempDir()

	fcp := newFakeControlPlane(t, "push-success-session", false /* credentialShouldFail */)
	out := runSandboxAgent(t, binPath, gitServerURL, workspaceDir, fcp)

	waitForRealClone(t, workspaceDir)

	repoDir := filepath.Join(workspaceDir, "widgets")
	if err := os.WriteFile(filepath.Join(repoDir, "change.txt"), []byte("a real change\n"), 0o644); err != nil {
		t.Fatalf("write change file: %v", err)
	}
	mustRunGit(t, repoDir, "add", "change.txt")
	mustRunGit(t, repoDir, "commit", "-m", "a real change for push to send")
	wantSHA := strings.TrimSpace(mustRunGit(t, repoDir, "rev-parse", "HEAD"))

	close(fcp.readyToPush)

	var result json.RawMessage
	select {
	case result = <-fcp.result:
	case <-time.After(pushTestTimeout):
		t.Fatalf("timed out waiting for push_complete/push_error; sandbox-agent output:\n%s", out.String())
	}

	var peek struct {
		Type string `json:"type"`
	}
	if err := json.Unmarshal(result, &peek); err != nil {
		t.Fatalf("unmarshal result: %v", err)
	}
	if peek.Type != "push_complete" {
		t.Fatalf("result type = %q, want %q (raw: %s, output:\n%s)", peek.Type, "push_complete", result, out.String())
	}

	var complete sandboxws.PushComplete
	if err := json.Unmarshal(result, &complete); err != nil {
		t.Fatalf("unmarshal PushComplete: %v", err)
	}
	if len(complete.Repos) != 1 {
		t.Fatalf("len(Repos) = %d, want 1", len(complete.Repos))
	}
	if complete.Repos[0].Name != "widgets" {
		t.Errorf("Repos[0].Name = %q, want %q", complete.Repos[0].Name, "widgets")
	}
	if complete.Repos[0].Branch != "main" {
		t.Errorf("Repos[0].Branch = %q, want %q", complete.Repos[0].Branch, "main")
	}
	if complete.Repos[0].Sha != wantSHA {
		t.Errorf("Repos[0].Sha = %q, want the REAL resulting HEAD sha %q", complete.Repos[0].Sha, wantSHA)
	}
}

// TestHandlePush_CredentialRefused_ProducesPushError proves a push whose
// credential the fake CP server refuses to provide produces a real
// PushError -- not a panic or a hang.
func TestHandlePush_CredentialRefused_ProducesPushError(t *testing.T) {
	binPath := buildSandboxAgentBinary(t)
	gitServerURL := setUpBareRepoAndServer(t)
	workspaceDir := t.TempDir()

	fcp := newFakeControlPlane(t, "push-failure-session", true /* credentialShouldFail */)
	out := runSandboxAgent(t, binPath, gitServerURL, workspaceDir, fcp)

	waitForRealClone(t, workspaceDir)

	repoDir := filepath.Join(workspaceDir, "widgets")
	if err := os.WriteFile(filepath.Join(repoDir, "change.txt"), []byte("a real change\n"), 0o644); err != nil {
		t.Fatalf("write change file: %v", err)
	}
	mustRunGit(t, repoDir, "add", "change.txt")
	mustRunGit(t, repoDir, "commit", "-m", "a real change for push to send")

	close(fcp.readyToPush)

	var result json.RawMessage
	select {
	case result = <-fcp.result:
	case <-time.After(pushTestTimeout):
		t.Fatalf("timed out waiting for push_complete/push_error; sandbox-agent output:\n%s", out.String())
	}

	var peek struct {
		Type string `json:"type"`
	}
	if err := json.Unmarshal(result, &peek); err != nil {
		t.Fatalf("unmarshal result: %v", err)
	}
	if peek.Type != "push_error" {
		t.Fatalf("result type = %q, want %q (raw: %s, output:\n%s)", peek.Type, "push_error", result, out.String())
	}

	var pushErr sandboxws.PushError
	if err := json.Unmarshal(result, &pushErr); err != nil {
		t.Fatalf("unmarshal PushError: %v", err)
	}
	if pushErr.Error == "" {
		t.Error("PushError.Error is empty, want a real error message")
	}
}
