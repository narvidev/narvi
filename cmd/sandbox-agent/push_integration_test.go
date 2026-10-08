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
// instead waits for the subprocess to log that its whole boot has
// completed (waitForBootComplete), makes ONE new
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
	"sync/atomic"
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

// pushTestWriteToken is the one credential startGitHTTPServer lets push:
// the write-capable token newFakeControlPlane serves. Any other credential
// -- a read-only installation token among them -- is refused a push with
// 403, as the code host refuses one (technical plan §30.4).
const pushTestWriteToken = "fake-oauth-access-token"

// startGitHTTPServer serves reposParent via git's own smart-HTTP backend
// (git-http-backend, via net/http/cgi -- a real git server, not a mock of
// one), gating ONLY the git-receive-pack half (push) behind a credential
// whose password is pushTestWriteToken -- clone/fetch (git-upload-pack) is
// left open. A request with no credential is challenged (401), and one
// with any other credential refused (403): a review session's read-only
// token cannot push (TestHandlePush_ReviewSessionsReadOnlyCredential_
// RefusedByTheServer). internal/sandboxagent/credentials' own tests
// already thoroughly prove the credential VALUE flows correctly end to
// end; this test's own job is proving HandlePush's new git-push/rev-parse/
// event-reporting logic, with a real credential round trip as supporting
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
			_, password, ok := r.BasicAuth()
			if !ok {
				w.Header().Set("WWW-Authenticate", `Basic realm="test-git-server"`)
				w.WriteHeader(http.StatusUnauthorized)
				return
			}
			if password != pushTestWriteToken {
				http.Error(w, "this credential cannot write to this repository", http.StatusForbidden)
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

	// credentialRequests counts scm-credentials requests. The clone is
	// anonymous, so only a push that reached authentication asks -- which
	// lets TestHandlePush_CredentialRefused_ProducesPushError tell a push
	// refused for its credential from one that failed before ever needing
	// one (it passed on any push_error while pushes could still race boot).
	credentialRequests atomic.Int32

	// password is the token scm-credentials serves: pushTestWriteToken,
	// or, from newFakeControlPlaneServing, any other one.
	password string
}

func newFakeControlPlane(t *testing.T, sessionID string, credentialShouldFail bool) *fakeControlPlane {
	t.Helper()
	return newFakeControlPlaneServing(t, sessionID, credentialShouldFail, pushTestWriteToken)
}

// newFakeControlPlaneServing is newFakeControlPlane with the token its
// scm-credentials serves chosen by the caller -- a read-only installation
// token, as a review session is served one (technical plan §30.4).
func newFakeControlPlaneServing(t *testing.T, sessionID string, credentialShouldFail bool, password string) *fakeControlPlane {
	t.Helper()
	fcp := &fakeControlPlane{
		sessionID:            sessionID,
		credentialShouldFail: credentialShouldFail,
		readyToPush:          make(chan struct{}),
		result:               make(chan json.RawMessage, 1),
		password:             password,
	}

	mux := http.NewServeMux()
	mux.HandleFunc("/sessions/"+sessionID+"/scm-credentials", func(w http.ResponseWriter, _ *http.Request) {
		fcp.credentialRequests.Add(1)
		if fcp.credentialShouldFail {
			http.Error(w, "no credential available for this session", http.StatusForbidden)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"username":  "x-access-token",
			"password":  fcp.password,
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
	gitServerURL, _ = setUpServedBareRepo(t)
	return gitServerURL
}

// setUpServedBareRepo is setUpBareRepoAndServer that also returns the bare
// repository's own directory, for a test that moves the remote itself.
func setUpServedBareRepo(t *testing.T) (gitServerURL, bareRepoDir string) {
	t.Helper()

	reposParent := t.TempDir()
	bareRepoDir = filepath.Join(reposParent, "repo.git")
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
	return gitServer.URL, bareRepoDir
}

// waitForBootComplete blocks until the sandbox-agent subprocess logs
// bootCompleteLogMsg (main.go), bounded by pushTestTimeout. It fails at once,
// with the subprocess's output, on either sign that boot will never complete:
// the subprocess logging shuttingDownLogMsg first, or exiting (exited, from
// runSandboxAgent). A failed boot gives one or the other: run() either
// returns early -- before its supervised group exists, as when opencode
// fails to spawn, which logs nothing this could wait on -- or shuts that
// group down first. The records it reads are Info records, which is why
// runSandboxAgent pins the subprocess's log level.
//
// The tests below stand in for the agent runtime: they edit and commit in
// the workspace, then ask for a push. The runtime gets no prompt before
// boot has finished, so they must not touch the workspace before then
// either. This used to wait only for the clone's own README.md, which
// exists as soon as `git clone` has checked it out -- while boot still has
// gitdir.Seed and every ChownWorkspaceForRuntime pass ahead of it (per repo
// inside gitclone.CloneAll, then over the whole workspace in main.go). The
// test's `git add`/`git commit` then ran inside those walks, creating and
// removing .git/index.lock and .git/HEAD.lock under them; in CI a walk hit
// "lchown .../.git/HEAD.lock: no such file or directory", boot failed, and
// the test waited out pushTestTimeout for a push result that could never
// come. This wait had already moved once, from the .git directory to
// README.md, when the same race surfaced as "cannot lock ref 'HEAD'"; any
// clone milestone short of boot completion leaves the rest of it open.
func waitForBootComplete(t *testing.T, out *syncBuffer, exited <-chan struct{}) {
	t.Helper()
	deadline := time.NewTimer(pushTestTimeout)
	defer deadline.Stop()
	poll := time.NewTicker(100 * time.Millisecond)
	defer poll.Stop()
	for {
		switch firstBootOutcome(out.String()) {
		case bootCompleteLogMsg:
			return
		case shuttingDownLogMsg:
			t.Fatalf("sandbox-agent shut down before completing boot; sandbox-agent output:\n%s", out.String())
		}
		select {
		case <-exited:
			// Its output is complete by now: exited closes only once
			// cmd.Wait has returned, which is after copying all of it.
			t.Fatalf("sandbox-agent exited while the test waited for it to complete boot; sandbox-agent output:\n%s", out.String())
		case <-deadline.C:
			t.Fatalf("timed out waiting for sandbox-agent to log %q; sandbox-agent output:\n%s", bootCompleteLogMsg, out.String())
		case <-poll.C:
		}
	}
}

// firstBootOutcome returns whichever of bootCompleteLogMsg and
// shuttingDownLogMsg appears first as a log record's own "msg" in output
// (sandbox-agent logs one JSON object per line, platform.NewLogger), or ""
// if neither does yet. Lines that are not JSON log records are skipped.
func firstBootOutcome(output string) string {
	for _, line := range strings.Split(output, "\n") {
		var record struct {
			Msg string `json:"msg"`
		}
		if json.Unmarshal([]byte(line), &record) != nil {
			continue
		}
		if record.Msg == bootCompleteLogMsg || record.Msg == shuttingDownLogMsg {
			return record.Msg
		}
	}
	return ""
}

// runSandboxAgent starts the real sandbox-agent binary against a real
// SESSION_CONFIG pointing at fcp, returning a buffer capturing its
// combined output for diagnostics, and a channel closed once it has exited.
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

func runSandboxAgent(t *testing.T, binPath, gitServerURL, workspaceDir string, fcp *fakeControlPlane) (out *syncBuffer, exited <-chan struct{}) {
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

	return startSandboxAgent(t, binPath, sessionConfigJSON, workspaceDir)
}

// startSandboxAgent is runSandboxAgent's process half, for a caller that
// writes its own SESSION_CONFIG: the real binary, booting in fresh mode
// against sessionConfigJSON, with every other setting runSandboxAgent's
// own comments explain.
func startSandboxAgent(t *testing.T, binPath, sessionConfigJSON, workspaceDir string) (out *syncBuffer, exited <-chan struct{}) {
	t.Helper()

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
		// waitForBootComplete reads boot's outcome from Info records, so an
		// NARVI_LOG_LEVEL inherited from the caller's shell above info
		// would hide them. os/exec keeps the last value of a duplicated key.
		"NARVI_LOG_LEVEL=info",
	)
	// syncBuffer, not bytes.Buffer: os/exec writes this from its own copier
	// goroutine while the tests below read it back from the test goroutine.
	// See syncBuffer's own doc comment. Assigning the SAME writer to both
	// Stdout and Stderr additionally makes os/exec reuse ONE pipe and ONE
	// copier goroutine for the pair (it compares the two interface values),
	// so the interleaving of the child's stdout and stderr is preserved.
	out = &syncBuffer{}
	cmd.Stdout = out
	cmd.Stderr = out

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

	return out, waitDone
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
	out, exited := runSandboxAgent(t, binPath, gitServerURL, workspaceDir, fcp)

	waitForBootComplete(t, out, exited)

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
	out, exited := runSandboxAgent(t, binPath, gitServerURL, workspaceDir, fcp)

	waitForBootComplete(t, out, exited)

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
	if fcp.credentialRequests.Load() == 0 {
		t.Errorf("push_error (%q) arrived without the push ever asking for a credential -- it failed before "+
			"authentication, so it says nothing about a refused credential; sandbox-agent output:\n%s", pushErr.Error, out.String())
	}
}

// TestHandlePush_ReviewSessionsReadOnlyCredential_RefusedByTheServer is the
// agent half of a review sandbox's credential not being able to push
// (technical plan §30.4): the control plane serves it the read-only
// installation token, and the code host -- here a git server that grants
// git-receive-pack to a write token alone -- refuses the push it makes
// with that token. The push asks for the credential, is refused, ends in
// push_error, and the repository is not moved.
func TestHandlePush_ReviewSessionsReadOnlyCredential_RefusedByTheServer(t *testing.T) {
	binPath := buildSandboxAgentBinary(t)
	gitServerURL := setUpBareRepoAndServer(t)
	workspaceDir := t.TempDir()

	fcp := newFakeControlPlaneServing(t, "push-read-only-session", false /* credentialShouldFail */, "read-only-installation-token")
	out, exited := runSandboxAgent(t, binPath, gitServerURL, workspaceDir, fcp)

	waitForBootComplete(t, out, exited)

	repoDir := filepath.Join(workspaceDir, "widgets")
	// The test server's certificate is self-signed, trusted here as the
	// agent's own environment trusts it (GIT_SSL_NO_VERIFY).
	before := strings.TrimSpace(mustRunGit(t, repoDir, "-c", "http.sslVerify=false", "ls-remote", "origin", "refs/heads/main"))
	if err := os.WriteFile(filepath.Join(repoDir, "change.txt"), []byte("a change a review never delivers\n"), 0o644); err != nil {
		t.Fatalf("write change file: %v", err)
	}
	mustRunGit(t, repoDir, "add", "change.txt")
	mustRunGit(t, repoDir, "commit", "-m", "a change for a push the server refuses")

	close(fcp.readyToPush)

	var result json.RawMessage
	select {
	case result = <-fcp.result:
	case <-time.After(pushTestTimeout):
		t.Fatalf("timed out waiting for push_complete/push_error; sandbox-agent output:\n%s", out.String())
	}
	var pushErr sandboxws.PushError
	if err := json.Unmarshal(result, &pushErr); err != nil || pushErr.Type != "push_error" {
		t.Fatalf("result = %s (%v), want a push_error; sandbox-agent output:\n%s", result, err, out.String())
	}
	if !strings.Contains(pushErr.Error, "403") {
		t.Errorf("push_error = %q, want the server's 403 refusal", pushErr.Error)
	}
	if fcp.credentialRequests.Load() == 0 {
		t.Errorf("push_error (%q) arrived without the push ever asking for a credential -- it failed before "+
			"the read-only token was ever presented; sandbox-agent output:\n%s", pushErr.Error, out.String())
	}
	if after := strings.TrimSpace(mustRunGit(t, repoDir, "-c", "http.sslVerify=false", "ls-remote", "origin", "refs/heads/main")); after != before {
		t.Errorf("the remote's main moved from %q to %q: the read-only token pushed", before, after)
	}
}

// TestHandlePush_BehindItsRemote_RefusedNotForced is technical plan §35.3's
// "never forced", for a sandbox whose workspace lacks commits already on
// its branch -- as after a rotation that fell back to an older snapshot.
// The remote's main holds C1 then C2, pushed after the sandbox cloned; the
// sandbox's main holds C1 then a commit of its own, C3. The push the real
// binary makes is refused as a non-fast-forward update: a push_error
// carrying git's own reason, which the control plane stores as the event's
// error (TestResilience_Scenario23_PushErrorOver32KiB_ReportedAndNextTurnCompletes),
// and the remote's main still at C2. git words the refusal by whether the
// sandbox has fetched C2 -- a restored sandbox's boot fetch usually has --
// so both are run.
func TestHandlePush_BehindItsRemote_RefusedNotForced(t *testing.T) {
	binPath := buildSandboxAgentBinary(t)

	tests := []struct {
		name    string
		fetched bool
		reason  string
	}{
		{name: "the sandbox fetched the remote's commit", fetched: true, reason: "(non-fast-forward)"},
		{name: "the sandbox never saw the remote's commit", fetched: false, reason: "(fetch first)"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			gitServerURL, bareRepoDir := setUpServedBareRepo(t)
			workspaceDir := t.TempDir()

			fcp := newFakeControlPlane(t, "push-behind-its-remote-session", false /* credentialShouldFail */)
			out, exited := runSandboxAgent(t, binPath, gitServerURL, workspaceDir, fcp)
			waitForBootComplete(t, out, exited)

			// C2: someone else's push to main, after the sandbox's clone.
			otherDir := t.TempDir()
			mustRunGit(t, otherDir, "clone", bareRepoDir, ".")
			if err := os.WriteFile(filepath.Join(otherDir, "theirs.txt"), []byte("pushed before the sandbox's push\n"), 0o644); err != nil {
				t.Fatalf("write theirs.txt: %v", err)
			}
			mustRunGit(t, otherDir, "add", "theirs.txt")
			mustRunGit(t, otherDir, "commit", "-m", "C2")
			mustRunGit(t, otherDir, "push", "origin", "main")
			remoteMain := strings.TrimSpace(mustRunGit(t, bareRepoDir, "rev-parse", "main"))

			// C3: the sandbox's own commit, on the C1 it cloned.
			repoDir := filepath.Join(workspaceDir, "widgets")
			if err := os.WriteFile(filepath.Join(repoDir, "ours.txt"), []byte("the sandbox's own work\n"), 0o644); err != nil {
				t.Fatalf("write ours.txt: %v", err)
			}
			mustRunGit(t, repoDir, "add", "ours.txt")
			mustRunGit(t, repoDir, "commit", "-m", "C3")
			if tc.fetched {
				// The test server's certificate is self-signed, trusted
				// here as the agent's own environment trusts it.
				mustRunGit(t, repoDir, "-c", "http.sslVerify=false", "fetch", "origin")
			}

			close(fcp.readyToPush)

			var result json.RawMessage
			select {
			case result = <-fcp.result:
			case <-time.After(pushTestTimeout):
				t.Fatalf("timed out waiting for push_complete/push_error; sandbox-agent output:\n%s", out.String())
			}
			var pushErr sandboxws.PushError
			if err := json.Unmarshal(result, &pushErr); err != nil || pushErr.Type != "push_error" {
				t.Fatalf("result = %s (%v), want a push_error; sandbox-agent output:\n%s", result, err, out.String())
			}
			if !strings.Contains(pushErr.Error, "[rejected]") || !strings.Contains(pushErr.Error, "main -> main "+tc.reason) {
				t.Errorf("push_error = %q, want git's refusal of main -> main %s", pushErr.Error, tc.reason)
			}
			if after := strings.TrimSpace(mustRunGit(t, bareRepoDir, "rev-parse", "main")); after != remoteMain {
				t.Errorf("the remote's main moved from %s (C2) to %s: the push overwrote a commit it did not contain", remoteMain, after)
			}
		})
	}
}
