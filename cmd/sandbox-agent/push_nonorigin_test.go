// This file covers pushOneRepo's own non-"origin" remote path (main.go,
// the else-branch added alongside §30.5): reading remote.<name>.url out
// of the RUNTIME's own worktree config (readRuntimeRemoteURL, AS THE
// RUNTIME uid), validating it (reposource.ValidateRepoURL), and pushing
// to that raw URL. Before this file, no test anywhere ever exercised
// this branch at all -- the only tests with a non-nil repoSpec.Remote
// (push_test.go) use values ValidateRemoteName itself rejects first, so
// readRuntimeRemoteURL was never even called. Deleting the
// ValidateRepoURL check, or making readRuntimeRemoteURL run as
// sandbox-agent instead of the runtime uid, left the whole suite green.
package main

import (
	"errors"
	"net/http/cgi"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/narvidev/narvi/contracts/gen/go/sandboxws"
	"github.com/narvidev/narvi/internal/domain/reposource"
)

// startPushTestGitHTTPSServer duplicates internal/sandboxagent/gitclone's
// own identically-named/-purposed test helper (that package's own
// startGitHTTPSServer) -- a DIFFERENT Go package, so nothing there is
// visible here. Serves every bare repo directly under reposParent over a
// REAL TLS listener via git-http-backend, so a real `git push` (not a
// stand-in) exercises the non-origin path end-to-end.
func startPushTestGitHTTPSServer(t *testing.T, reposParent string) *httptest.Server {
	t.Helper()

	execPathOut, err := exec.Command("git", "--exec-path").Output()
	if err != nil {
		t.Fatalf("git --exec-path: %v", err)
	}
	backendPath := filepath.Join(strings.TrimSpace(string(execPathOut)), "git-http-backend")
	if _, statErr := os.Stat(backendPath); statErr != nil {
		t.Skipf("git-http-backend not available at %s, skipping: %v", backendPath, statErr)
	}

	cgiHandler := &cgi.Handler{
		Path: backendPath,
		Root: "/",
		Env: []string{
			"GIT_HTTP_EXPORT_ALL=1",
			"GIT_PROJECT_ROOT=" + reposParent,
			// git-http-backend must be allowed to accept a push (git
			// receive-pack), unlike gitclone's own read-only server.
			"GIT_HTTP_RECEIVE_PACK=1",
		},
	}

	server := httptest.NewUnstartedServer(cgiHandler)
	server.StartTLS()
	t.Cleanup(server.Close)
	return server
}

// initBareRepoForPushTest creates a real, empty --bare repo at dir with
// receive.denyCurrentBranch=updateInstead -- so a push of "main" to it,
// including while it is checked out, succeeds and is directly
// inspectable afterward without a separate checkout step.
func initBareRepoForPushTest(t *testing.T, dir string) {
	t.Helper()
	if out, err := exec.Command("git", "init", "-q", "--bare", "-b", "main", dir).CombinedOutput(); err != nil {
		t.Fatalf("git init --bare %s: %v\n%s", dir, err, out)
	}
	if out, err := exec.Command("git", "-C", dir, "config", "http.receivepack", "true").CombinedOutput(); err != nil {
		t.Fatalf("git -C %s config http.receivepack true: %v\n%s", dir, err, out)
	}
}

// TestPushOneRepo_NonOriginRemote_ValidHTTPSURL_Pushes proves the happy
// path of the non-origin branch end-to-end, with a REAL push over a real
// (self-signed) TLS listener: the runtime adds a remote named "upstream"
// (an ordinary, unprivileged `git remote add`, exactly what the coding
// agent is free to do mid-session) pointing at a valid https:// URL,
// and pushOneRepo({Remote: "upstream"}) must resolve that URL from the
// RUNTIME's own config and actually push the repo's real commit there.
func TestPushOneRepo_NonOriginRemote_ValidHTTPSURL_Pushes(t *testing.T) {
	t.Setenv("GIT_SSL_NO_VERIFY", "true")

	reposParent := t.TempDir()
	upstreamDir := filepath.Join(reposParent, "upstream.git")
	initBareRepoForPushTest(t, upstreamDir)

	server := startPushTestGitHTTPSServer(t, reposParent)
	upstreamURL := server.URL + "/upstream.git"

	workspaceDir := t.TempDir()
	h := newSeededPushTestHandler(t, workspaceDir, "widgets")
	repoDir := filepath.Join(workspaceDir, "widgets")

	shaOut, err := exec.Command("git", "-C", repoDir, "rev-parse", "HEAD").CombinedOutput()
	if err != nil {
		t.Fatalf("git -C %s rev-parse HEAD: %v\n%s", repoDir, err, shaOut)
	}
	wantSHA := strings.TrimSpace(string(shaOut))

	// The RUNTIME's own, unprivileged remote add -- never through the
	// agent-owned git-dir.
	if out, err := exec.Command("git", "-C", repoDir, "remote", "add", "upstream", upstreamURL).CombinedOutput(); err != nil {
		t.Fatalf("git -C %s remote add upstream %s: %v\n%s", repoDir, upstreamURL, err, out)
	}

	spec := sandboxws.PushReposElem{
		Name:   "widgets",
		Branch: "main",
		Remote: pushTestStrPtr("upstream"),
	}
	if _, err := h.pushOneRepo(spec); err != nil {
		t.Fatalf("pushOneRepo(%+v) error = %v, want nil (a valid https upstream remote must push)", spec, err)
	}

	gotOut, err := exec.Command("git", "-C", upstreamDir, "rev-parse", "--verify", "HEAD").CombinedOutput()
	if err != nil {
		t.Fatalf("git -C %s rev-parse --verify HEAD (after push): %v\n%s", upstreamDir, err, gotOut)
	}
	if got := strings.TrimSpace(string(gotOut)); got != wantSHA {
		t.Errorf("upstream HEAD after push = %q, want %q (the widgets repo's real commit)", got, wantSHA)
	}
}

// TestPushOneRepo_NonOriginRemote_InvalidRuntimeURLRefusedBeforeSpawn
// proves the other half: an invalid URL sitting in the RUNTIME's own
// config for a validly-NAMED remote (so ValidateRemoteName itself does
// NOT reject it -- unlike every existing push_test.go case) must be
// refused by reposource.ValidateRepoURL BEFORE any push subprocess is
// ever spawned. Uses the SAME wall-clock-vs-baseline proof
// TestPushOneRepo_MaliciousInputsRejectedBeforeSpawn already established
// for the name/branch/remote-name cases, now for a runtime-config URL.
func TestPushOneRepo_NonOriginRemote_InvalidRuntimeURLRefusedBeforeSpawn(t *testing.T) {
	workspaceDir := t.TempDir()
	h := newSeededPushTestHandler(t, workspaceDir, "widgets")
	repoDir := filepath.Join(workspaceDir, "widgets")

	markerDir := t.TempDir()
	execMarker := filepath.Join(markerDir, "ext-helper-should-never-run")

	tests := []struct {
		name string
		url  string
	}{
		{name: "non-https scheme (file://)", url: "file:///etc/passwd"},
		{name: "ext:: arbitrary-command remote helper", url: "ext::sh -c 'touch " + execMarker + "'"},
		{name: "no scheme at all", url: "not-a-url-at-all"},
	}

	for i, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			remoteName := "bad" + string(rune('a'+i))
			// The RUNTIME's own, unprivileged remote add -- an ordinary
			// action the coding agent (or a prompt-injected one) can take
			// entirely on its own.
			if out, err := exec.Command("git", "-C", repoDir, "remote", "add", remoteName, tc.url).CombinedOutput(); err != nil {
				t.Fatalf("git -C %s remote add %s %s: %v\n%s", repoDir, remoteName, tc.url, err, out)
			}

			spec := sandboxws.PushReposElem{Name: "widgets", Branch: "main", Remote: pushTestStrPtr(remoteName)}
			_, err := h.pushOneRepo(spec)

			if err == nil {
				t.Fatalf("pushOneRepo(%+v) error = nil, want a validation error rejecting the runtime-config URL %q", spec, tc.url)
			}
			// The proof this branch's own ValidateRepoURL check -- not
			// some unrelated failure (a network error, a missing remote)
			// -- is what rejected it: pushOneRepo wraps
			// *reposource.InvalidRepoURLError verbatim, and that type is
			// returned ONLY by the explicit check that runs strictly
			// BEFORE the push Spawn call in the code (main.go's own
			// `if err := reposource.ValidateRepoURL(runtimeURL); err !=
			// nil` in the non-origin branch).
			var invalidErr *reposource.InvalidRepoURLError
			if !errors.As(err, &invalidErr) {
				t.Errorf("pushOneRepo(%+v) error = %v (%T), want it to wrap *reposource.InvalidRepoURLError -- "+
					"otherwise this isn't proven to be THIS validation check rejecting it before any spawn", spec, err, err)
			}
		})
	}

	// Real, non-timing-based proof for the ext:: case specifically: if a
	// push subprocess had ever actually been spawned against this URL,
	// git's own "ext::" remote helper would have run the shell command
	// and created this marker file. It must not exist.
	if _, statErr := os.Stat(execMarker); !os.IsNotExist(statErr) {
		t.Errorf("marker file for the ext:: remote helper exists (stat error = %v) -- "+
			"a real git push actually ran against the invalid runtime-config URL", statErr)
	}
}
