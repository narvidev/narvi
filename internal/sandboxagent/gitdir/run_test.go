package gitdir_test

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"

	"github.com/narvidev/narvi/internal/sandboxagent/boot"
	"github.com/narvidev/narvi/internal/sandboxagent/githarden"
	"github.com/narvidev/narvi/internal/sandboxagent/gitdir"
	"github.com/narvidev/narvi/internal/sandboxagent/supervisor"
)

func runGitForRunTest(t *testing.T, dir string, args ...string) {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %v (dir=%s): %v\n%s", args, dir, err, out)
	}
}

// initRunTestRepo creates a real, tiny git repo at dir with one commit.
func initRunTestRepo(t *testing.T, dir string) {
	t.Helper()
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatalf("MkdirAll: %v", err)
	}
	runGitForRunTest(t, dir, "init", "-q", "-b", "main")
	runGitForRunTest(t, dir, "config", "user.email", "t@example.com")
	runGitForRunTest(t, dir, "config", "user.name", "T")
	if err := os.WriteFile(filepath.Join(dir, "README.md"), []byte("hi\n"), 0o644); err != nil {
		t.Fatalf("write README.md: %v", err)
	}
	runGitForRunTest(t, dir, "add", ".")
	runGitForRunTest(t, dir, "commit", "-qm", "initial")
}

// TestRun_RefusesASymlinkSwappedWorktree is correction 1's own executable
// proof (review finding on the interrupted work): SyncHeadIn's own
// O_NOFOLLOW only protects the FINAL path component (HEAD) -- if the
// runtime replaces repo.WorkTree/.git itself with a symlink to a
// DIFFERENT repository's .git AFTER Seed already ran once, Run must
// refuse outright rather than silently operating against whatever the
// symlink now points at. Seed itself only checks this once, at seed time
// (its own step 0); Run re-checks on EVERY call, which is what this test
// actually exercises.
func TestRun_RefusesASymlinkSwappedWorktree(t *testing.T) {
	base := t.TempDir()
	workspaceDir := filepath.Join(base, "workspace")
	wt := filepath.Join(workspaceDir, "repo1")
	initRunTestRepo(t, wt)

	// A second, entirely unrelated repository -- the "other repo" the
	// swapped symlink will point at.
	otherRepo := filepath.Join(base, "other-repo")
	initRunTestRepo(t, otherRepo)

	gitDirRoot := filepath.Join(base, "gitdirs")
	if err := gitdir.EnsureRoot(gitDirRoot); err != nil {
		t.Fatalf("gitdir.EnsureRoot: %v", err)
	}
	layout := gitdir.Layout{Root: gitDirRoot, WorkspaceDir: workspaceDir}
	repo := layout.Repo("repo1")
	sup := supervisor.New()
	ctx := context.Background()

	if err := gitdir.Seed(ctx, sup, repo, "https://example.invalid/repo1.git", nil, 10*time.Second, 5*time.Second); err != nil {
		t.Fatalf("gitdir.Seed: %v", err)
	}

	// A benign Run call must succeed before the swap -- proves the guard
	// isn't simply refusing everything.
	spec := supervisor.Spec{Path: "git", Args: githarden.Args(repo, "status", "--porcelain")}
	if _, err := gitdir.Run(ctx, sup, repo, nil, spec, 10*time.Second, 5*time.Second); err != nil {
		t.Fatalf("Run() before swap: unexpected error: %v", err)
	}

	// The swap: replace wt/.git with a symlink to the OTHER repo's own
	// .git.
	wtGit := filepath.Join(wt, ".git")
	if err := os.RemoveAll(wtGit); err != nil {
		t.Fatalf("remove wt/.git: %v", err)
	}
	if err := os.Symlink(filepath.Join(otherRepo, ".git"), wtGit); err != nil {
		t.Fatalf("symlink wt/.git -> other repo: %v", err)
	}

	// A marker file in the OTHER repo's worktree that must never be
	// touched by anything this call spawns -- if Run's guard is missing,
	// a command reading/writing through the swapped .git could reach it.
	marker := filepath.Join(otherRepo, "MUST-NOT-BE-TOUCHED")
	if err := os.WriteFile(marker, []byte("untouched\n"), 0o644); err != nil {
		t.Fatalf("write marker: %v", err)
	}

	_, err := gitdir.Run(ctx, sup, repo, nil, spec, 10*time.Second, 5*time.Second)
	if err == nil {
		t.Fatal("Run() after wt/.git was swapped for a symlink: error = nil, want a refusal -- the symlink-swap guard did not fire")
	}

	if data, readErr := os.ReadFile(marker); readErr != nil || string(data) != "untouched\n" {
		t.Errorf("marker in the OTHER repo was modified (readErr=%v, data=%q) -- Run spawned something against the swapped target instead of refusing outright", readErr, data)
	}
}

// TestRun_AllowsAnOrdinaryRealWorktree is the negative control for the
// test above: an ordinary, never-swapped worktree must keep working
// exactly as before -- the guard must not be so broad it refuses
// legitimate repositories.
func TestRun_AllowsAnOrdinaryRealWorktree(t *testing.T) {
	base := t.TempDir()
	workspaceDir := filepath.Join(base, "workspace")
	wt := filepath.Join(workspaceDir, "repo1")
	initRunTestRepo(t, wt)

	gitDirRoot := filepath.Join(base, "gitdirs")
	if err := gitdir.EnsureRoot(gitDirRoot); err != nil {
		t.Fatalf("gitdir.EnsureRoot: %v", err)
	}
	layout := gitdir.Layout{Root: gitDirRoot, WorkspaceDir: workspaceDir}
	repo := layout.Repo("repo1")
	sup := supervisor.New()
	ctx := context.Background()

	if err := gitdir.Seed(ctx, sup, repo, "https://example.invalid/repo1.git", nil, 10*time.Second, 5*time.Second); err != nil {
		t.Fatalf("gitdir.Seed: %v", err)
	}

	spec := supervisor.Spec{Path: "git", Args: githarden.Args(repo, "status", "--porcelain")}
	if _, err := gitdir.Run(ctx, sup, repo, nil, spec, 10*time.Second, 5*time.Second); err != nil {
		t.Fatalf("Run() on an ordinary, never-swapped worktree: unexpected error: %v", err)
	}
}

// TestSessionConfigEnvVar_MatchesBoot pins gitdir's own duplicated
// sessionConfigEnvVar constant (run.go's own doc comment explains the
// import-cycle reason it is duplicated rather than imported from boot)
// against boot.SessionConfigEnvVar -- the value it must never silently
// drift from. A drift here would leak the sandbox's own plaintext bearer
// token into the runtime's own git via RuntimeGit's env (RuntimeGit
// strips only this one var; a wrong literal here strips nothing at all).
//
// This lives in package gitdir_test (an EXTERNAL test package), not
// gitdir's own internal test files, specifically so it CAN import
// internal/sandboxagent/boot: boot itself imports gitdir (for the
// fingerprint/deps-ladder git reads, boot/fingerprint.go and boot/
// depsladder.go), so gitdir's own production code must never import boot
// back -- but this test file is compiled into a separate test binary and
// is free to depend on both.
func TestSessionConfigEnvVar_MatchesBoot(t *testing.T) {
	got := gitdir.SessionConfigEnvVarForTest()
	want := boot.SessionConfigEnvVar
	if got != want {
		t.Fatalf("gitdir's own duplicated sessionConfigEnvVar = %q, want %q (boot.SessionConfigEnvVar) -- these must never drift, see run.go's own doc comment", got, want)
	}
}
