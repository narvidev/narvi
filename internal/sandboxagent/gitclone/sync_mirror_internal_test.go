// This file is deliberately "package gitclone" (white-box, not
// "package gitclone_test" like every other *_test.go file here) solely so
// it can substitute mirrorBranchUpstreamFunc (sync.go's own test seam) for
// the duration of one test -- proving checkoutBranch treats a
// MirrorBranchUpstream failure as a non-fatal warning, never a checkout
// failure (round-2 review, R4), independent of real git's own multi-valued-
// key behavior (which TestSyncAll_FreshBranchFromOrigin_
// StaleMultiValuedRuntimeMergeKey_MirrorSucceeds, in sync_test.go, already
// covers end to end).
package gitclone

import (
	"context"
	"errors"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/narvidev/narvi/contracts/gen/go/sessionconfig"
	"github.com/narvidev/narvi/internal/domain/gitstate"
	"github.com/narvidev/narvi/internal/sandboxagent/gitdir"
	"github.com/narvidev/narvi/internal/sandboxagent/githarden"
	"github.com/narvidev/narvi/internal/sandboxagent/supervisor"
)

// runMirrorTestGit runs git with args in dir, failing the test immediately
// on any error -- a private duplicate of gitclone_test's own runGit
// (clone_test.go): this file is a DIFFERENT package (white-box "gitclone",
// not "gitclone_test"), so nothing defined over there is visible here.
func runMirrorTestGit(t *testing.T, dir string, args ...string) {
	t.Helper()

	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %s (dir=%s) failed: %v\n%s", strings.Join(args, " "), dir, err, out)
	}
}

// initMirrorTestRepo creates a fresh git repo at dir on branch "main" with
// one commit -- a private duplicate of gitclone_test's own initRepo.
func initMirrorTestRepo(t *testing.T, dir string) {
	t.Helper()

	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatalf("MkdirAll(%s): %v", dir, err)
	}

	runMirrorTestGit(t, dir, "init", "-b", "main")
	runMirrorTestGit(t, dir, "config", "user.email", "test@example.com")
	runMirrorTestGit(t, dir, "config", "user.name", "Test")

	if err := os.WriteFile(filepath.Join(dir, "README.md"), []byte("hello\n"), 0o644); err != nil {
		t.Fatalf("write README.md: %v", err)
	}
	runMirrorTestGit(t, dir, "add", ".")
	runMirrorTestGit(t, dir, "commit", "-m", "initial commit")
}

// currentMirrorTestBranch returns the checked-out branch name at dir -- a
// private duplicate of gitclone_test's own currentBranch.
func currentMirrorTestBranch(t *testing.T, dir string) string {
	t.Helper()

	cmd := exec.Command("git", "rev-parse", "--abbrev-ref", "HEAD")
	cmd.Dir = dir
	out, err := cmd.Output()
	if err != nil {
		t.Fatalf("git -C %s rev-parse --abbrev-ref HEAD: %v", dir, err)
	}
	return strings.TrimSpace(string(out))
}

// TestSyncAll_MirrorBranchUpstreamFails_ChecksOutAnywayAndWarns proves R4's
// policy directly: a MirrorBranchUpstream failure -- for ANY reason, not
// just the multi-valued-key one -- must not turn an already-successful
// checkout into a reported failure. mirrorBranchUpstreamFunc is
// substituted with a spy that unconditionally fails, regardless of what
// real git would have done, isolating this policy from the specific
// multi-valued-key trigger sync_test.go's own end-to-end test covers.
func TestSyncAll_MirrorBranchUpstreamFails_ChecksOutAnywayAndWarns(t *testing.T) {
	workspaceDir := t.TempDir()
	repoDir := filepath.Join(workspaceDir, "repo1")
	initMirrorTestRepo(t, repoDir) // local "main" only, no origin remote at all --
	// checkoutBase's own remote-tracking preference chain falls all the
	// way through to local HEAD (no remote-tracking ref is or ever will be
	// fetchable), so the fresh branch below is created from HEAD.

	// repos[].Branch is left nil, not set explicitly: an EXPLICIT branch
	// that is neither local nor fetchable is its own separate fatal
	// pre-check (syncOne's own "explicit branch neither local nor
	// fetchable" guard) -- unrelated to this test's own concern. A nil
	// Branch invents a session-scoped one (gitstate.ResolveSessionBranch)
	// that has no such requirement: with no origin remote configured at
	// all, checkoutBase's own remote-tracking preference chain falls all
	// the way through to local HEAD, exactly the "fresh, no upstream to
	// fetch" shape this test needs.
	const sessionID = "session-mirror-fails"
	wantBranch := gitstate.ResolveSessionBranch(nil, sessionID)

	orig := mirrorBranchUpstreamFunc
	t.Cleanup(func() { mirrorBranchUpstreamFunc = orig })
	sentinelErr := errors.New("synthetic mirror failure injected by test")
	var mirrorCalls int
	mirrorBranchUpstreamFunc = func(_ context.Context, _ *supervisor.Supervisor, _ githarden.Repo, _ *syscall.Credential, branch string, _, _ time.Duration) error {
		mirrorCalls++
		if branch != wantBranch {
			t.Errorf("mirrorBranchUpstreamFunc called with branch = %q, want %q", branch, wantBranch)
		}
		return sentinelErr
	}

	var logBuf strings.Builder
	origLogger := slog.Default()
	slog.SetDefault(slog.New(slog.NewJSONHandler(&logBuf, nil)))
	t.Cleanup(func() { slog.SetDefault(origLogger) })

	repos := []sessionconfig.SessionConfigReposElem{
		{Name: "repo1", Url: "https://example.invalid/repo1.git"},
	}

	sup := supervisor.New()
	results, err := SyncAll(context.Background(), sup, gitdir.Layout{Root: t.TempDir(), WorkspaceDir: workspaceDir}, nil, repos, nil, sessionID,
		10*time.Second, 10*time.Second, 2*time.Second, func(string, string, string) {}, func(string, float64, bool) {}, func(string, float64, bool) {})
	if err != nil {
		t.Fatalf("SyncAll() error = %v, want nil -- a MirrorBranchUpstream failure must not fail an already-successful checkout", err)
	}
	if len(results) != 1 {
		t.Fatalf("len(results) = %d, want 1", len(results))
	}
	if results[0].Err != nil {
		t.Fatalf("results[0].Err = %v, want nil", results[0].Err)
	}
	if results[0].State != gitstate.StateReady {
		t.Errorf("results[0].State = %s, want ready -- a mirror failure must not move the reported state off ready", results[0].State)
	}
	if head := currentMirrorTestBranch(t, repoDir); head != wantBranch {
		t.Errorf("checked-out branch = %q, want %q -- the checkout itself must have gone through despite the mirror failure", head, wantBranch)
	}
	if mirrorCalls != 1 {
		t.Fatalf("mirrorBranchUpstreamFunc called %d times, want exactly 1 -- otherwise this test didn't actually exercise the failure path", mirrorCalls)
	}

	logged := logBuf.String()
	if !strings.Contains(logged, "mirror upstream tracking onto runtime config failed") {
		t.Errorf("log output = %q, want a warning naming the mirror failure", logged)
	}
	if !strings.Contains(logged, `"level":"WARN"`) {
		t.Errorf("log output = %q, want the mirror-failure line logged at WARN, not a higher/lower level", logged)
	}
}
