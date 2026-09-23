package gitclone_test

import (
	"context"
	"errors"
	"fmt"
	"net/http/cgi"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/narvidev/narvi/contracts/gen/go/sessionconfig"
	"github.com/narvidev/narvi/internal/sandboxagent/gitclone"
	"github.com/narvidev/narvi/internal/sandboxagent/gitdir"
	"github.com/narvidev/narvi/internal/sandboxagent/supervisor"
)

// These tests spawn real `git` subprocesses -- git is always present on
// both macOS and Linux CI runners (§6.4's own git-based workflow assumes
// this too).

const (
	testCloneTimeout = 30 * time.Second
	testStopGrace    = 2 * time.Second
)

// TestMain sets GIT_SSL_NO_VERIFY=true ONCE, before any test runs (never
// racing any test's own t.Parallel() goroutines, unlike an os.Setenv call
// from inside a test would): every successful-clone test below now clones
// over a real, but self-signed-TLS, local git-http-backend server (see
// startGitHTTPSServer) rather than a bare filesystem path, because
// reposource.ValidateRepoURL (wired into CloneAll this Step) accepts only
// an absolute "https://" URL -- a plain local path or a non-https scheme
// is correctly rejected before ever reaching git at all. Trusting the
// self-signed cert here is acceptable ONLY because these are throwaway
// test servers, never anything resembling production configuration --
// the same technique, for the same reason, as cmd/sandbox-agent's own
// push_integration_test.go.
//
// This package used to ALSO register a global OTel SDK MeterProvider/
// ManualReader here (audit-remediation batch B7, Finding 3), so
// telemetry.go's own gitFetchDurationHistogram/gitCheckoutDurationHistogram
// -- lazily resolved via sync.OnceValue on first use -- observed a real
// reader regardless of which test in this package ran first. §33.3 deleted
// that local histogram machinery entirely: this package now
// reports fetch/checkout timing via OnGitFetchTiming/OnGitCheckoutTiming
// callbacks (sync.go) instead of recording anything itself, so there is no
// longer any OTel state for this test binary to set up.
func TestMain(m *testing.M) {
	_ = os.Setenv("GIT_SSL_NO_VERIFY", "true")
	os.Exit(m.Run())
}

// startGitHTTPSServer serves reposParent via git's own smart-HTTP backend
// (git-http-backend, via net/http/cgi -- a real git server, not a mock of
// one) over TLS, so these tests can clone a genuine "https://" URL end to
// end, exactly like a real production remote. Skips (not fails) the
// calling test if git-http-backend isn't available in this environment.
func startGitHTTPSServer(t *testing.T, reposParent string) *httptest.Server {
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
		},
	}

	server := httptest.NewUnstartedServer(cgiHandler)
	server.StartTLS()
	t.Cleanup(server.Close)
	return server
}

// runGit runs git with args in dir, failing the test immediately on any
// error.
func runGit(t *testing.T, dir string, args ...string) {
	t.Helper()

	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %s (dir=%s) failed: %v\n%s", strings.Join(args, " "), dir, err, out)
	}
}

// initRepo creates a fresh git repo at dir on branch "main" with one
// commit (a README.md), configuring a throwaway local user identity so the
// commit itself never depends on any ambient git config.
func initRepo(t *testing.T, dir string) {
	t.Helper()

	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatalf("MkdirAll(%s): %v", dir, err)
	}

	runGit(t, dir, "init", "-b", "main")
	runGit(t, dir, "config", "user.email", "test@example.com")
	runGit(t, dir, "config", "user.name", "Test")

	if err := os.WriteFile(filepath.Join(dir, "README.md"), []byte("hello\n"), 0o644); err != nil {
		t.Fatalf("write README.md: %v", err)
	}
	runGit(t, dir, "add", ".")
	runGit(t, dir, "commit", "-m", "initial commit")
}

// currentBranch returns the checked-out branch name at dir.
func currentBranch(t *testing.T, dir string) string {
	t.Helper()

	cmd := exec.Command("git", "rev-parse", "--abbrev-ref", "HEAD")
	cmd.Dir = dir
	out, err := cmd.Output()
	if err != nil {
		t.Fatalf("git -C %s rev-parse --abbrev-ref HEAD: %v", dir, err)
	}
	return strings.TrimSpace(string(out))
}

func TestCloneAll_SinglePrimarySucceeds(t *testing.T) {
	t.Parallel()

	reposParent := t.TempDir()
	initRepo(t, filepath.Join(reposParent, "repo1-src"))
	server := startGitHTTPSServer(t, reposParent)

	workspaceDir := t.TempDir()
	repos := []sessionconfig.SessionConfigReposElem{
		{Name: "repo1", Url: server.URL + "/repo1-src"},
	}

	sup := supervisor.New()
	results, err := gitclone.CloneAll(context.Background(), sup, gitdir.Layout{Root: t.TempDir(), WorkspaceDir: workspaceDir}, nil, nil, repos, nil, testCloneTimeout, testStopGrace)
	if err != nil {
		t.Fatalf("CloneAll() error = %v, want nil", err)
	}
	if len(results) != 1 {
		t.Fatalf("len(results) = %d, want 1", len(results))
	}

	got := results[0]
	if got.Err != nil {
		t.Errorf("results[0].Err = %v, want nil", got.Err)
	}
	if !got.Primary {
		t.Error("results[0].Primary = false, want true (position 0)")
	}

	wantDir := filepath.Join(workspaceDir, "repo1")
	if got.Dir != wantDir {
		t.Errorf("results[0].Dir = %q, want %q", got.Dir, wantDir)
	}
	if _, statErr := os.Stat(filepath.Join(wantDir, "README.md")); statErr != nil {
		t.Errorf("cloned repo missing README.md: %v", statErr)
	}
}

// TestCloneAll_HardensTheActualCloneInvocation is F2's own regression
// test. gitclone.cloneOne used to build `git clone`'s own argument list
// and hand it to githarden.Harden, which requires a "-C <dir>" to rewrite
// around -- but `git clone`'s own target directory is a trailing
// POSITIONAL argument (it does not exist yet, and -C requires its target
// to already exist), never a "-C <dir>", so Harden's own "-C" scan found
// nothing and returned the args COMPLETELY UNCHANGED: every hardening
// flag, and GIT_ALLOW_PROTOCOL, silently absent from the one git
// invocation that creates a repo's own .git/config in the first place --
// while the call site itself looked identical to every properly-hardened
// one.
//
// This intercepts the REAL "git" binary CloneAll spawns, via a fake
// executable placed first on PATH (matching exactly how
// supervisor.Spawn's own exec.Command resolves a bare "git"), and
// inspects the actual argv/env the real git binary would have received --
// rather than merely asserting CloneAll "succeeds", which a successful
// clone would have done even before this fix (the missing flags do not
// break a normal clone; they just leave it unhardened).
func TestCloneAll_HardensTheActualCloneInvocation(t *testing.T) {
	realGit, lookErr := exec.LookPath("git")
	if lookErr != nil {
		t.Fatalf("exec.LookPath(git): %v", lookErr)
	}

	fakeGitDir := t.TempDir()
	captureFile := filepath.Join(t.TempDir(), "capture.txt")

	// The fake git records its own argv (one token per line, delimited so
	// a token containing no delimiter character is unambiguous -- every
	// token this test checks for is a single "-c"/"key=value" pair with
	// no embedded angle brackets) and GIT_ALLOW_PROTOCOL, THEN either (for
	// the "clone" invocation specifically) creates a real, local, empty
	// git repo at its own target directory -- entirely offline, never
	// actually contacting repoURL (an unreachable "https://example.invalid"
	// address, deliberately, so this test never depends on network access)
	// -- or (every OTHER invocation) execs the REAL git binary with the
	// same argv/env. Step 171 (§30.5) made this test's own original
	// concern (does CloneAll's OWN clone invocation carry the right
	// hardening flags) inseparable from a second one this same PATH
	// override now also intercepts: gitdir.Seed runs immediately after a
	// successful clone, needs a REAL .git directory left behind by the
	// clone to seed FROM, and its own `git init --bare`/`git config` calls
	// need a REAL git binary underneath them -- a bare `exit 0` (this
	// test's pre-Step-171 shape) satisfies neither.
	fakeGit := "#!/bin/sh\n" +
		"{\n" +
		"  printf 'ARGV:'\n" +
		"  for a in \"$@\"; do printf ' <%s>' \"$a\"; done\n" +
		"  printf '\\n'\n" +
		"  printf 'GIT_ALLOW_PROTOCOL=%s\\n' \"$GIT_ALLOW_PROTOCOL\"\n" +
		"} >> \"" + captureFile + "\"\n" +
		"is_clone=0\n" +
		"for a in \"$@\"; do [ \"$a\" = clone ] && is_clone=1; done\n" +
		"if [ \"$is_clone\" = 1 ]; then\n" +
		"  prev=\"\"; cur=\"\"\n" +
		"  for a in \"$@\"; do prev=\"$cur\"; cur=\"$a\"; done\n" +
		"  mkdir -p \"$cur\"\n" +
		"  \"" + realGit + "\" init -q \"$cur\"\n" +
		"  exit 0\n" +
		"fi\n" +
		"exec \"" + realGit + "\" \"$@\"\n"
	if err := os.WriteFile(filepath.Join(fakeGitDir, "git"), []byte(fakeGit), 0o755); err != nil {
		t.Fatalf("write fake git: %v", err)
	}

	// t.Setenv, not t.Parallel -- this test mutates process-wide PATH, and
	// t.Setenv itself forbids combining the two.
	t.Setenv("PATH", fakeGitDir+string(os.PathListSeparator)+os.Getenv("PATH"))

	workspaceDir := t.TempDir()
	repoURL := "https://example.invalid/owner/repo.git"
	repos := []sessionconfig.SessionConfigReposElem{
		{Name: "repo1", Url: repoURL},
	}

	sup := supervisor.New()
	results, err := gitclone.CloneAll(context.Background(), sup, gitdir.Layout{Root: t.TempDir(), WorkspaceDir: workspaceDir}, nil, nil, repos, nil, testCloneTimeout, testStopGrace)
	if err != nil {
		t.Fatalf("CloneAll() error = %v, want nil (the fake git always exits 0)", err)
	}
	if len(results) != 1 || results[0].Err != nil {
		t.Fatalf("CloneAll() results = %+v, want one successful result", results)
	}

	captured, readErr := os.ReadFile(captureFile)
	if readErr != nil {
		t.Fatalf("the fake git was never invoked (no capture file): %v", readErr)
	}
	out := string(captured)

	for _, want := range []string{
		// Step 171 (§30.5): ArgsForClone no longer carries safe.directory
		// at all -- a fresh `git clone`'s own target .git is owned by this
		// process itself, never by the runtime, so there is nothing
		// dubious about its ownership in the first place (see
		// githarden.ArgsForClone's own doc comment). core.hooksPath is
		// still kept, as defense in depth.
		"<-c> <core.hooksPath=/dev/null>",
		"<-c> <credential.helper=>",
		"<-c> <protocol.allow=never>",
		"<-c> <protocol.https.allow=always>",
		"<-c> <protocol.ext.allow=never>",
		"<-c> <protocol.ftp.allow=never>",
		// remote.origin.proxy/http.<url>.proxy: F3's own two proxy
		// overrides, both present here -- unlike gitFetchRef/
		// resolveDefaultBranch (sync.go), which target "origin" by NAME
		// and rely on remote.origin.proxy= alone, clone's own url-keyed
		// http.<url>.proxy override is real: `git clone`'s initial fetch
		// contacts the exact url on its own command line (repoURL,
		// below), so RepoURLProxyArg's key is guaranteed to match it.
		"<-c> <remote.origin.proxy=>",
		"<-c> <http." + repoURL + ".proxy=>",
		"<clone>",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("captured clone invocation missing %q\nfull capture:\n%s", want, out)
		}
	}
	if !strings.Contains(out, "GIT_ALLOW_PROTOCOL=https") {
		t.Errorf("captured clone invocation's env did not carry GIT_ALLOW_PROTOCOL=https\nfull capture:\n%s", out)
	}
}

func TestCloneAll_ExplicitBranchChecksOutThatBranch(t *testing.T) {
	t.Parallel()

	reposParent := t.TempDir()
	srcDir := filepath.Join(reposParent, "repo1-src")
	initRepo(t, srcDir) // default branch "main"

	runGit(t, srcDir, "checkout", "-b", "feature-x")
	if err := os.WriteFile(filepath.Join(srcDir, "feature.txt"), []byte("feature\n"), 0o644); err != nil {
		t.Fatalf("write feature.txt: %v", err)
	}
	runGit(t, srcDir, "add", ".")
	runGit(t, srcDir, "commit", "-m", "feature commit")
	runGit(t, srcDir, "checkout", "main")

	server := startGitHTTPSServer(t, reposParent)

	branch := "feature-x"
	workspaceDir := t.TempDir()
	repos := []sessionconfig.SessionConfigReposElem{
		{Name: "repo1", Url: server.URL + "/repo1-src", Branch: &branch},
	}

	sup := supervisor.New()
	results, err := gitclone.CloneAll(context.Background(), sup, gitdir.Layout{Root: t.TempDir(), WorkspaceDir: workspaceDir}, nil, nil, repos, nil, testCloneTimeout, testStopGrace)
	if err != nil {
		t.Fatalf("CloneAll() error = %v, want nil", err)
	}

	clonedDir := results[0].Dir
	if head := currentBranch(t, clonedDir); head != "feature-x" {
		t.Errorf("cloned repo branch = %q, want %q", head, "feature-x")
	}
	if _, statErr := os.Stat(filepath.Join(clonedDir, "feature.txt")); statErr != nil {
		t.Errorf("cloned repo missing feature.txt (wrong branch checked out?): %v", statErr)
	}
}

func TestCloneAll_NilBranchClonesDefaultBranch(t *testing.T) {
	t.Parallel()

	reposParent := t.TempDir()
	initRepo(t, filepath.Join(reposParent, "repo1-src")) // default branch "main"
	server := startGitHTTPSServer(t, reposParent)

	workspaceDir := t.TempDir()
	repos := []sessionconfig.SessionConfigReposElem{
		{Name: "repo1", Url: server.URL + "/repo1-src"}, // Branch left nil
	}

	sup := supervisor.New()
	results, err := gitclone.CloneAll(context.Background(), sup, gitdir.Layout{Root: t.TempDir(), WorkspaceDir: workspaceDir}, nil, nil, repos, nil, testCloneTimeout, testStopGrace)
	if err != nil {
		t.Fatalf("CloneAll() error = %v, want nil", err)
	}

	if head := currentBranch(t, results[0].Dir); head != "main" {
		t.Errorf("cloned repo branch = %q, want %q (the source's own default, no --branch flag)", head, "main")
	}
}

// TestCloneAll_PrimaryFailureStopsImmediately proves a fatal primary
// failure stops CloneAll before a LATER repo is ever even attempted --
// proven by that repo's target directory never existing at all.
func TestCloneAll_PrimaryFailureStopsImmediately(t *testing.T) {
	t.Parallel()

	workspaceDir := t.TempDir()
	repos := []sessionconfig.SessionConfigReposElem{
		// Port 1 is never a listener in any real test/CI environment --
		// this connects (and fails, connection refused) near-instantly,
		// with no DNS lookup and no dependency on outbound network
		// access, unlike a real unreachable hostname would risk.
		{Name: "bad-primary", Url: "https://127.0.0.1:1/nowhere-xyz.git"},
		{Name: "never-attempted", Url: "https://example.invalid/never.git"},
	}

	sup := supervisor.New()
	results, err := gitclone.CloneAll(context.Background(), sup, gitdir.Layout{Root: t.TempDir(), WorkspaceDir: workspaceDir}, nil, nil, repos, nil, testCloneTimeout, testStopGrace)
	if err == nil {
		t.Fatal("CloneAll() error = nil, want a fatal error for the failed primary repo")
	}
	if len(results) != 1 {
		t.Fatalf("len(results) = %d, want 1 (only the primary should have been attempted)", len(results))
	}
	if results[0].Err == nil {
		t.Error("results[0].Err = nil, want a clone error")
	}

	if _, statErr := os.Stat(filepath.Join(workspaceDir, "never-attempted")); !os.IsNotExist(statErr) {
		t.Errorf("second repo's directory stat = %v, want IsNotExist (repo never attempted)", statErr)
	}
}

// TestCloneAll_SecondaryFailureContinues proves a secondary repo's clone
// failure is a logged warning, not fatal -- CloneAll returns nil and a
// LATER repo after it still gets cloned.
func TestCloneAll_SecondaryFailureContinues(t *testing.T) {
	t.Parallel()

	reposParent := t.TempDir()
	initRepo(t, filepath.Join(reposParent, "primary-src"))
	initRepo(t, filepath.Join(reposParent, "later-src"))
	server := startGitHTTPSServer(t, reposParent)

	workspaceDir := t.TempDir()
	repos := []sessionconfig.SessionConfigReposElem{
		{Name: "primary", Url: server.URL + "/primary-src"},
		// Port 1 is never a listener in any real test/CI environment --
		// this connects (and fails, connection refused) near-instantly,
		// with no DNS lookup and no dependency on outbound network
		// access.
		{Name: "bad-secondary", Url: "https://127.0.0.1:1/nowhere-xyz.git"},
		{Name: "later", Url: server.URL + "/later-src"},
	}

	sup := supervisor.New()
	results, err := gitclone.CloneAll(context.Background(), sup, gitdir.Layout{Root: t.TempDir(), WorkspaceDir: workspaceDir}, nil, nil, repos, nil, testCloneTimeout, testStopGrace)
	if err != nil {
		t.Fatalf("CloneAll() error = %v, want nil (a secondary failure is a warning, not fatal)", err)
	}
	if len(results) != 3 {
		t.Fatalf("len(results) = %d, want 3 (every repo attempted)", len(results))
	}

	if results[0].Err != nil {
		t.Errorf("results[0] (primary) Err = %v, want nil", results[0].Err)
	}
	if results[1].Err == nil {
		t.Error("results[1] (bad secondary) Err = nil, want a clone error")
	}
	if results[2].Err != nil {
		t.Errorf("results[2] (later) Err = %v, want nil -- loop must continue past the secondary failure", results[2].Err)
	}
	if _, statErr := os.Stat(filepath.Join(workspaceDir, "later", "README.md")); statErr != nil {
		t.Errorf("later repo not actually cloned: %v", statErr)
	}
}

// TestCloneAll_MaliciousRepoNameRejectedBeforeAnySpawn proves a repo.Name
// attempting path traversal (a ".." segment) is rejected by
// reposource.ValidateRepoName BEFORE CloneAll's own filepath.Join, let
// alone cloneOne's sup.Spawn call, ever runs for it -- proven two ways:
// results[0].Dir stays the empty string (no filepath.Join was ever
// performed against the malicious name at all, per CloneResult.Dir's own
// doc comment), and the traversal target itself is never created.
func TestCloneAll_MaliciousRepoNameRejectedBeforeAnySpawn(t *testing.T) {
	t.Parallel()

	parentDir := t.TempDir()
	workspaceDir := filepath.Join(parentDir, "workspace")
	repos := []sessionconfig.SessionConfigReposElem{
		{Name: "../escaped-outside-workspace", Url: "https://example.invalid/never.git"},
	}

	sup := supervisor.New()
	results, err := gitclone.CloneAll(context.Background(), sup, gitdir.Layout{Root: t.TempDir(), WorkspaceDir: workspaceDir}, nil, nil, repos, nil, testCloneTimeout, testStopGrace)
	if err == nil {
		t.Fatal("CloneAll() error = nil, want a fatal validation error for the malicious repo name")
	}
	if len(results) != 1 {
		t.Fatalf("len(results) = %d, want 1", len(results))
	}
	if results[0].Err == nil {
		t.Error("results[0].Err = nil, want a validation error")
	}
	if results[0].Dir != "" {
		t.Errorf("results[0].Dir = %q, want empty -- an invalid Name must never even reach filepath.Join", results[0].Dir)
	}

	escapeTarget := filepath.Join(parentDir, "escaped-outside-workspace")
	if _, statErr := os.Stat(escapeTarget); !os.IsNotExist(statErr) {
		t.Errorf("escape target stat = %v, want IsNotExist (a path-traversal target must never be created)", statErr)
	}
}

// TestCloneAll_MaliciousRepoURLRejectedBeforeAnySpawn proves a repo.Url
// using git's own "ext::" alternate transport -- which, if it ever
// reached a real `git clone` subprocess, would execute an arbitrary
// shell command (here, one that creates a marker file) -- is rejected by
// reposource.ValidateRepoURL BEFORE cloneOne's sup.Spawn call ever runs
// for it.
//
// This is proven via the marker file's own absence, not merely "the
// eventual git invocation fails harmlessly": the marker file could ONLY
// ever be created by a REAL git process actually executing the ext::
// command, and that only happens inside cloneOne -- a function CloneAll
// never even calls once validateRepoSpec has already rejected this repo.
// This is a strictly stronger, more concrete proof than "zero processes
// spawned" bookkeeping would be, since supervisor.Supervisor exposes no
// process count to assert against directly.
func TestCloneAll_MaliciousRepoURLRejectedBeforeAnySpawn(t *testing.T) {
	t.Parallel()

	markerPath := filepath.Join(t.TempDir(), "should-never-exist")
	workspaceDir := t.TempDir()
	repos := []sessionconfig.SessionConfigReposElem{
		{Name: "widgets", Url: fmt.Sprintf(`ext::sh -c "touch %s"`, markerPath)},
	}

	sup := supervisor.New()
	results, err := gitclone.CloneAll(context.Background(), sup, gitdir.Layout{Root: t.TempDir(), WorkspaceDir: workspaceDir}, nil, nil, repos, nil, testCloneTimeout, testStopGrace)
	if err == nil {
		t.Fatal("CloneAll() error = nil, want a fatal validation error for the ext:: repo url")
	}
	if len(results) != 1 {
		t.Fatalf("len(results) = %d, want 1", len(results))
	}
	if results[0].Err == nil {
		t.Error("results[0].Err = nil, want a validation error")
	}
	if _, statErr := os.Stat(markerPath); !os.IsNotExist(statErr) {
		t.Errorf("marker file stat = %v, want IsNotExist -- the ext:: url was actually executed by a real git process, "+
			"proving validation did NOT run before sup.Spawn", statErr)
	}
}

// TestGitCloneDashDash_RealDefenseInDepth proves, against the REAL git
// binary (verified directly, not assumed from documentation), that "--"
// placed in cloneOne's own exact position (after every -c/--branch flag,
// immediately before the repo URL and target dir) genuinely stops git's
// own option parser from treating a leading-"-" repo URL as an OPTION at
// all, in favor of a literal positional value.
//
// This is isolated from reposource.ValidateRepoURL's own separate
// rejection of any such value (which, in production, never lets such a
// value reach cloneOne at all -- ValidateRepoURL's https-only allowlist
// rejects it first): this test invokes git directly, bypassing CloneAll/
// reposource entirely, to lock in "--"'s own real CLI behavior as the
// second, independent layer of this Step's defense in depth, exactly as
// this Step's own brief asks ("reason about it directly against real git
// CLI semantics").
//
// The proof deliberately uses a made-up, unrecognized "--...-xyz" string
// rather than a real flag like "--upload-pack": this isolates the
// MECHANICAL option-vs-positional parsing question "--" answers, via
// git's own two structurally different, LOCALE-INDEPENDENT failure
// modes -- exit code 129 ("unknown option", git's own usage-error path,
// triggered only when the string is read as an OPTION at all) without
// "--", versus exit code 128 ("repository does not exist", git's own
// fatal-application-error path, triggered once the SAME string is
// instead read as a literal <repository> positional) with "--" -- rather
// than on a real flag's own downstream side effect, which (verified
// directly against this git version) additionally depends on whatever
// positional argument is LEFT OVER after the flag is consumed happening
// to already be a valid, reachable repository -- a precondition that
// does not hold for cloneOne's own real two-positional shape
// (repo.Url, dir), where dir is always a fresh clone destination that
// does not exist yet.
func TestGitCloneDashDash_RealDefenseInDepth(t *testing.T) {
	t.Parallel()

	tmp := t.TempDir()
	maliciousValue := "--this-is-not-a-real-git-clone-flag-xyz"

	cmdWithout := exec.Command("git", "clone", maliciousValue, filepath.Join(tmp, "dest-without-sep"))
	errWithout := cmdWithout.Run()
	exitWithout := exitCode(t, errWithout)
	if exitWithout != 129 {
		t.Fatalf("sanity check failed: without --, `git clone %s ...` exited %d, want 129 (\"unknown option\") -- "+
			"this test's own premise no longer holds against this git version", maliciousValue, exitWithout)
	}

	cmdWith := exec.Command("git", "clone", "--", maliciousValue, filepath.Join(tmp, "dest-with-sep"))
	errWith := cmdWith.Run()
	exitWith := exitCode(t, errWith)
	if exitWith == 129 {
		t.Errorf(`git clone -- %s ... exited 129 ("unknown option") -- "--" did NOT stop option parsing; `+
			"the malicious value was still read as an option", maliciousValue)
	}
	if exitWith == 0 {
		t.Fatalf("git clone -- %s ... unexpectedly succeeded (exit 0)", maliciousValue)
	}
}

// exitCode extracts a subprocess's exit code from the error exec.Cmd.Run
// returns (nil only on a genuine exit-0 success).
func exitCode(t *testing.T, err error) int {
	t.Helper()
	if err == nil {
		return 0
	}
	var exitErr *exec.ExitError
	if !errors.As(err, &exitErr) {
		t.Fatalf("Run() error = %v, want an *exec.ExitError", err)
	}
	return exitErr.ExitCode()
}

func TestWriteAgentsManifest(t *testing.T) {
	t.Parallel()

	workspaceDir := t.TempDir()
	branch := "feature-x"
	results := []gitclone.CloneResult{
		{
			Repo:    sessionconfig.SessionConfigReposElem{Name: "primary-repo"},
			Primary: true,
			Dir:     filepath.Join(workspaceDir, "primary-repo"),
		},
		{
			Repo:    sessionconfig.SessionConfigReposElem{Name: "secondary-repo", Branch: &branch},
			Primary: false,
			Dir:     filepath.Join(workspaceDir, "secondary-repo"),
		},
		{
			Repo:    sessionconfig.SessionConfigReposElem{Name: "failed-repo"},
			Primary: false,
			Dir:     filepath.Join(workspaceDir, "failed-repo"),
			Err:     errors.New("clone failed"),
		},
	}

	if err := gitclone.WriteAgentsManifest(workspaceDir, results, nil); err != nil {
		t.Fatalf("WriteAgentsManifest() error = %v", err)
	}

	data, err := os.ReadFile(filepath.Join(workspaceDir, "AGENTS.md"))
	if err != nil {
		t.Fatalf("read AGENTS.md: %v", err)
	}
	content := string(data)

	for _, want := range []string{"primary-repo", "primary", "secondary-repo", "feature-x", "(default)"} {
		if !strings.Contains(content, want) {
			t.Errorf("manifest missing %q; content:\n%s", want, content)
		}
	}
	if strings.Contains(content, "failed-repo") {
		t.Errorf("manifest includes failed-repo, want it skipped; content:\n%s", content)
	}
	if strings.Contains(content, "Boot-time degrade notices") {
		t.Errorf("manifest includes a degrade-notices section with nil degradeNotes, want it omitted entirely; content:\n%s", content)
	}
}

// TestWriteAgentsManifest_DegradeNotes proves §27.1's own adversarial-
// review LOW fix (§27.1: "recorded in the boot log and AGENTS.md"): a
// non-empty degradeNotes slice produces an additive section in the
// generated manifest, alongside (never instead of) the ordinary repo
// table.
func TestWriteAgentsManifest_DegradeNotes(t *testing.T) {
	t.Parallel()

	workspaceDir := t.TempDir()
	results := []gitclone.CloneResult{
		{
			Repo:    sessionconfig.SessionConfigReposElem{Name: "primary-repo"},
			Primary: true,
			Dir:     filepath.Join(workspaceDir, "primary-repo"),
		},
	}
	degradeNotes := []string{
		"sandbox secrets: boot-time fetch failed after retrying; this session booted with NO sandbox secrets injected",
	}

	if err := gitclone.WriteAgentsManifest(workspaceDir, results, degradeNotes); err != nil {
		t.Fatalf("WriteAgentsManifest() error = %v", err)
	}

	data, err := os.ReadFile(filepath.Join(workspaceDir, "AGENTS.md"))
	if err != nil {
		t.Fatalf("read AGENTS.md: %v", err)
	}
	content := string(data)

	for _, want := range []string{"primary-repo", "Boot-time degrade notices", "sandbox secrets: boot-time fetch failed after retrying"} {
		if !strings.Contains(content, want) {
			t.Errorf("manifest missing %q; content:\n%s", want, content)
		}
	}
}

// TestCloneAll_ScopedEnvironment_AppliesSparseCheckout proves §14.1's own
// clone-step enforcement end to end, against a real git clone + real
// sparse-checkout: a non-empty pathScope restricts the cloned working tree
// to exactly the given patterns -- files outside them never materialize on
// disk at all.
func TestCloneAll_ScopedEnvironment_AppliesSparseCheckout(t *testing.T) {
	t.Parallel()

	reposParent := t.TempDir()
	srcDir := filepath.Join(reposParent, "repo1-src")
	initRepo(t, srcDir) // README.md at the root, branch "main"

	for _, dir := range []string{"apps/web", "apps/api", "contracts/api"} {
		if err := os.MkdirAll(filepath.Join(srcDir, dir), 0o755); err != nil {
			t.Fatalf("MkdirAll(%s): %v", dir, err)
		}
	}
	if err := os.WriteFile(filepath.Join(srcDir, "apps/web/index.js"), []byte("web\n"), 0o644); err != nil {
		t.Fatalf("write apps/web/index.js: %v", err)
	}
	if err := os.WriteFile(filepath.Join(srcDir, "apps/api/index.js"), []byte("api\n"), 0o644); err != nil {
		t.Fatalf("write apps/api/index.js: %v", err)
	}
	if err := os.WriteFile(filepath.Join(srcDir, "contracts/api/openapi.yaml"), []byte("spec\n"), 0o644); err != nil {
		t.Fatalf("write contracts/api/openapi.yaml: %v", err)
	}
	runGit(t, srcDir, "add", ".")
	runGit(t, srcDir, "commit", "-m", "add apps/web, apps/api, contracts/api")

	server := startGitHTTPSServer(t, reposParent)

	workspaceDir := t.TempDir()
	repos := []sessionconfig.SessionConfigReposElem{
		{Name: "repo1", Url: server.URL + "/repo1-src"},
	}
	pathScope := []string{"/apps/web/*", "/contracts/api/*"}

	sup := supervisor.New()
	results, err := gitclone.CloneAll(context.Background(), sup, gitdir.Layout{Root: t.TempDir(), WorkspaceDir: workspaceDir}, nil, nil, repos, pathScope, testCloneTimeout, testStopGrace)
	if err != nil {
		t.Fatalf("CloneAll() error = %v, want nil", err)
	}
	if results[0].Err != nil {
		t.Fatalf("results[0].Err = %v, want nil", results[0].Err)
	}

	clonedDir := results[0].Dir
	for _, want := range []string{"apps/web/index.js", "contracts/api/openapi.yaml"} {
		if _, statErr := os.Stat(filepath.Join(clonedDir, want)); statErr != nil {
			t.Errorf("expected %s to materialize (in scope): %v", want, statErr)
		}
	}
	for _, wantAbsent := range []string{"apps/api/index.js", "README.md"} {
		if _, statErr := os.Stat(filepath.Join(clonedDir, wantAbsent)); !os.IsNotExist(statErr) {
			t.Errorf("expected %s to NOT materialize (out of scope), stat = %v", wantAbsent, statErr)
		}
	}
}

// TestCloneAll_UnscopedEnvironment_NoSparseCheckoutInvoked proves a nil
// pathScope (the overwhelming common, unscoped case) produces ZERO
// behavior change: every file materializes, exactly as before this Step.
func TestCloneAll_UnscopedEnvironment_NoSparseCheckoutInvoked(t *testing.T) {
	t.Parallel()

	reposParent := t.TempDir()
	initRepo(t, filepath.Join(reposParent, "repo1-src"))
	server := startGitHTTPSServer(t, reposParent)

	workspaceDir := t.TempDir()
	repos := []sessionconfig.SessionConfigReposElem{
		{Name: "repo1", Url: server.URL + "/repo1-src"},
	}

	sup := supervisor.New()
	results, err := gitclone.CloneAll(context.Background(), sup, gitdir.Layout{Root: t.TempDir(), WorkspaceDir: workspaceDir}, nil, nil, repos, nil, testCloneTimeout, testStopGrace)
	if err != nil {
		t.Fatalf("CloneAll() error = %v, want nil", err)
	}
	if _, statErr := os.Stat(filepath.Join(results[0].Dir, "README.md")); statErr != nil {
		t.Errorf("README.md missing, want present (unscoped clone must be full): %v", statErr)
	}
}

// TestCloneAll_InvalidPathScopeRejectedBeforeAnyClone proves a malicious/
// invalid pathScope pattern (a ".." path-traversal segment,
// internal/domain/environment.ValidatePathScope's own rejection rule) is
// rejected BEFORE any repo is even attempted -- zero clones, zero
// directories created.
func TestCloneAll_InvalidPathScopeRejectedBeforeAnyClone(t *testing.T) {
	t.Parallel()

	workspaceDir := t.TempDir()
	repos := []sessionconfig.SessionConfigReposElem{
		{Name: "repo1", Url: "https://example.invalid/repo1.git"},
	}
	pathScope := []string{"../escape"}

	sup := supervisor.New()
	results, err := gitclone.CloneAll(context.Background(), sup, gitdir.Layout{Root: t.TempDir(), WorkspaceDir: workspaceDir}, nil, nil, repos, pathScope, testCloneTimeout, testStopGrace)
	if err == nil {
		t.Fatal("CloneAll() error = nil, want a fatal validation error for the invalid path scope")
	}
	if len(results) != 0 {
		t.Fatalf("len(results) = %d, want 0 (no repo should have been attempted)", len(results))
	}
	if _, statErr := os.Stat(filepath.Join(workspaceDir, "repo1")); !os.IsNotExist(statErr) {
		t.Errorf("repo1 dir stat = %v, want IsNotExist (no repo should have been cloned before validation)", statErr)
	}
}
