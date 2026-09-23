package gitdir_test

import (
	"bytes"
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/narvidev/narvi/internal/sandboxagent/boot"
	"github.com/narvidev/narvi/internal/sandboxagent/gitdir"
	"github.com/narvidev/narvi/internal/sandboxagent/githarden"
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

// runHardened runs a hardened git command (githarden.Args(repo, args...))
// through gitdir.Run and fails the test unless the command actually
// exited 0, capturing stdout/stderr into the failure message. gitdir.Run
// itself only ever reports a spawn/wait/SyncHead error -- it never
// inspects git's own exit code (see run.go's own Run doc comment) -- so a
// caller that checks only the returned error, as every call site in this
// file used to, treats a FAILED git invocation (e.g. an agent-side commit
// with no configured identity, which git refuses with a non-zero exit
// and no Go-level error at all) exactly like a successful one. Every
// call site in this file that expects success now goes through this
// helper instead of inlining that (broken) pattern.
func runHardened(ctx context.Context, t *testing.T, sup *supervisor.Supervisor, repo githarden.Repo, args ...string) (stdout, stderr string) {
	t.Helper()
	var outBuf, errBuf bytes.Buffer
	spec := supervisor.Spec{Path: "git", Args: githarden.Args(repo, args...), Stdout: &outBuf, Stderr: &errBuf}
	result, err := gitdir.Run(ctx, sup, repo, nil, spec, 10*time.Second, 5*time.Second)
	if err != nil {
		t.Fatalf("gitdir.Run(%v): %v\nstdout: %s\nstderr: %s", args, err, outBuf.String(), errBuf.String())
	}
	if result.ExitCode != 0 {
		t.Fatalf("gitdir.Run(%v): git exited %d\nstdout: %s\nstderr: %s", args, result.ExitCode, outBuf.String(), errBuf.String())
	}
	return outBuf.String(), errBuf.String()
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
	if result, err := gitdir.Run(ctx, sup, repo, nil, spec, 10*time.Second, 5*time.Second); err != nil {
		t.Fatalf("Run() before swap: unexpected error: %v", err)
	} else if result.ExitCode != 0 {
		t.Fatalf("Run() before swap: git exited %d", result.ExitCode)
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

// TestRun_RefusesASymlinkSwappedWorktreeItself is
// TestRun_RefusesASymlinkSwappedWorktree's own missing case (finding on
// the interrupted work): that test only ever swaps wt/.git for a
// symlink, never wt itself. Run calls assertRealDir on BOTH
// repo.WorkTree and repo.WorkTree/.git, but os.Lstat only refuses a
// symlink at the FINAL path component of whatever it is given -- so
// Lstat(wt/.git), on its own, silently FOLLOWS a symlinked wt as an
// intermediate component and lands on the OTHER repo's real .git
// directory, passing that check cleanly. Only the separate
// assertRealDir(repo.WorkTree) call catches a swapped wt itself. Without
// this test, deleting that one call left the sibling test fully green
// (verified directly, see this package's own review notes) -- this test
// is the one that actually pins it.
func TestRun_RefusesASymlinkSwappedWorktreeItself(t *testing.T) {
	base := t.TempDir()
	workspaceDir := filepath.Join(base, "workspace")
	wt := filepath.Join(workspaceDir, "repo1")
	initRunTestRepo(t, wt)

	// A second, entirely unrelated repository -- the "other repo" the
	// swapped wt symlink will point at directly (not just its .git).
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
	if result, err := gitdir.Run(ctx, sup, repo, nil, spec, 10*time.Second, 5*time.Second); err != nil {
		t.Fatalf("Run() before swap: unexpected error: %v", err)
	} else if result.ExitCode != 0 {
		t.Fatalf("Run() before swap: git exited %d", result.ExitCode)
	}

	// The swap: replace wt ITSELF (not just wt/.git) with a symlink to
	// the OTHER repo's own worktree directory. wt/.git therefore resolves
	// -- via the symlinked wt as an intermediate path component -- to a
	// perfectly real directory (the other repo's own .git), which the
	// wt/.git-only guard would pass cleanly.
	if err := os.RemoveAll(wt); err != nil {
		t.Fatalf("remove wt: %v", err)
	}
	if err := os.Symlink(otherRepo, wt); err != nil {
		t.Fatalf("symlink wt -> other repo: %v", err)
	}

	// A marker file in the OTHER repo's worktree that must never be
	// touched by anything this call spawns.
	marker := filepath.Join(otherRepo, "MUST-NOT-BE-TOUCHED")
	if err := os.WriteFile(marker, []byte("untouched\n"), 0o644); err != nil {
		t.Fatalf("write marker: %v", err)
	}

	_, err := gitdir.Run(ctx, sup, repo, nil, spec, 10*time.Second, 5*time.Second)
	if err == nil {
		t.Fatal("Run() after wt itself was swapped for a symlink: error = nil, want a refusal -- the wt-swap guard did not fire")
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

	runHardened(ctx, t, sup, repo, "status", "--porcelain")
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

// TestRun_SyncsAgentHeadInBeforeEverySpawn pins Run's own SyncHeadIn
// bracket (run.go, at the top of Run, BEFORE the spawn): nothing else in
// the suite moves the RUNTIME's own HEAD between a Seed call and a later
// Run call, so every other test's agent-owned HEAD happens to already be
// correct from Seed's own one-time SyncHeadIn -- which would stay true
// even if Run's own bracket were deleted outright. This test does what no
// other one does: seed once, then have the RUNTIME switch to a new
// branch and commit (exactly what the coding agent does mid-session),
// THEN call Run -- proving the agent-owned HEAD Run's spawn actually
// operates against reflects that switch, not the stale branch Seed saw.
//
// Mirrors headSHA's own exact spec shape in cmd/sandbox-agent/main.go
// (githarden.Args(repo, "rev-parse", "HEAD") through gitdir.Run), so a
// regression here is the exact regression that function would suffer in
// production: a push of a freshly-created branch would report the
// PREVIOUS branch's SHA as the pushed head.
func TestRun_SyncsAgentHeadInBeforeEverySpawn(t *testing.T) {
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

	// The RUNTIME switches to a new branch and commits -- exactly what the
	// coding agent does mid-session -- entirely AFTER Seed's own one-time
	// SyncHeadIn already ran, so the agent-owned HEAD Seed left behind
	// still points at "main".
	runGitForRunTest(t, wt, "checkout", "-q", "-b", "feat")
	if err := os.WriteFile(filepath.Join(wt, "feat.txt"), []byte("feat\n"), 0o644); err != nil {
		t.Fatalf("write feat.txt: %v", err)
	}
	runGitForRunTest(t, wt, "add", ".")
	runGitForRunTest(t, wt, "commit", "-qm", "feat commit")

	wantSHA, err := exec.Command("git", "-C", wt, "rev-parse", "HEAD").CombinedOutput()
	if err != nil {
		t.Fatalf("git -C %s rev-parse HEAD: %v\n%s", wt, err, wantSHA)
	}

	stdout, _ := runHardened(ctx, t, sup, repo, "rev-parse", "HEAD")

	got := strings.TrimSpace(stdout)
	want := strings.TrimSpace(string(wantSHA))
	if got != want {
		t.Fatalf("Run()'s rev-parse HEAD = %q, want %q (the runtime's own \"feat\" head) -- "+
			"the agent-owned HEAD Run spawned against was stale, still pointing at \"main\"", got, want)
	}
}

// TestMirrorSparseCheckout_RuntimeHasLinkedWorktree reproduces the
// finding directly against real git: the runtime is free to run an
// ordinary, unprivileged `git worktree add` at any point during a
// session (its own metadata lives under repo.WorkTree/.git/worktrees,
// which Seed never shares or cleans up), including one that is later
// deleted from disk but stays registered ("prunable") -- git still
// counts it as a second worktree. Before the fix, MirrorSparseCheckout's
// runtime-side `git config --worktree ...` write refused outright (exit
// 128, "--worktree cannot be used with multiple working trees unless the
// config extension worktreeConfig is enabled") the moment the runtime
// repo had more than one worktree entry, which made an ordinary session
// action fail an otherwise-healthy boot.
func TestMirrorSparseCheckout_RuntimeHasLinkedWorktree(t *testing.T) {
	base := t.TempDir()
	workspaceDir := filepath.Join(base, "workspace")
	wt := filepath.Join(workspaceDir, "repo1")
	initRunTestRepo(t, wt)

	// The runtime creates a second worktree, then deletes its directory --
	// exactly the "prunable" shape the adversarial review reproduced.
	// git still counts this repo as having more than one worktree entry
	// until an explicit `git worktree prune` runs, which nothing in this
	// codebase ever does.
	runGitForRunTest(t, wt, "worktree", "add", "-q", filepath.Join(base, "wt2"), "-b", "other")
	if err := os.RemoveAll(filepath.Join(base, "wt2")); err != nil {
		t.Fatalf("remove linked worktree dir: %v", err)
	}

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

	// Agent-side sparse-checkout set, exactly like gitclone's own
	// applySparseCheckout does, via the agent-owned git-dir -- this only
	// ever touches the AGENT's own config, which Seed deletes and rebuilds
	// on every boot, so it can never itself carry the extension over to
	// the runtime side.
	runHardened(ctx, t, sup, repo, "sparse-checkout", "set", "--no-cone", "--", "/README.md")

	if err := gitdir.MirrorSparseCheckout(ctx, sup, repo, nil, 10*time.Second, 5*time.Second); err != nil {
		t.Fatalf("MirrorSparseCheckout() error = %v, want nil (an ordinary linked worktree must never fail the mirror)", err)
	}

	out, err := exec.Command("git", "-C", wt, "config", "--worktree", "--get", "core.sparseCheckout").CombinedOutput()
	if err != nil {
		t.Fatalf("git -C %s config --worktree --get core.sparseCheckout: %v\n%s", wt, err, out)
	}
	if got := string(out); got != "true\n" {
		t.Errorf("runtime core.sparseCheckout (worktree-scoped) = %q, want \"true\\n\"", got)
	}

	// The extension itself must actually be on, on the runtime side --
	// what the fix turns on explicitly, since the agent-side
	// sparse-checkout set never reaches it.
	extOut, err := exec.Command("git", "-C", wt, "config", "--get", "extensions.worktreeConfig").CombinedOutput()
	if err != nil {
		t.Fatalf("git -C %s config --get extensions.worktreeConfig: %v\n%s", wt, err, extOut)
	}
	if got := string(extOut); got != "true\n" {
		t.Errorf("runtime extensions.worktreeConfig = %q, want \"true\\n\"", got)
	}
}

// assertSymlink fails the test unless path is STILL a symlink (os.Lstat,
// never following it).
func assertSymlink(t *testing.T, path, context string) {
	t.Helper()
	info, err := os.Lstat(path)
	if err != nil {
		t.Fatalf("Lstat(%s) after %s: %v", path, context, err)
	}
	if info.Mode()&os.ModeSymlink == 0 {
		t.Fatalf("%s is no longer a symlink after %s (mode=%s) -- a real git maintenance operation replaced the agent-side symlink with a real file, breaking the shared-file model this package's own doc comment (gitdir.go) depends on", path, context, info.Mode())
	}
}

// TestSharedFileSymlinks_SurviveRealGitMaintenanceOperations is a
// PERMANENT pin (queued from this package's own implementer notes) of a
// property this package has always relied on but never directly tested:
// the agent-side packed-refs/index symlinks are FILE symlinks (unlike
// objects/refs/logs/info, which are DIRECTORY symlinks a plain rename
// could never silently replace). git's own lockfile-and-rename write
// path -- used by `pack-refs`, `gc`, and a stash pop's own index
// reapplication -- resolves a symlink at the final path component and
// renames its own lockfile OVER THE RESOLVED TARGET, never replacing the
// symlink itself with a real file (seed.go's own doc comment, and the F1
// finding notes this package's own review left on record, both measured
// this directly for `index`). This test pins the SAME property for real,
// routine git maintenance operations run through the agent-owned
// git-dir -- `git pack-refs --all`, `git gc --prune=now`, and a `stash
// pop --index` whose OWN refs/stash entry is itself PACKED (not loose)
// -- so a future git release that changes this lockfile semantics (e.g.
// unlinks and recreates the final path instead of renaming over it) goes
// RED here immediately, rather than silently corrupting the split
// git-dir shape in production.
func TestSharedFileSymlinks_SurviveRealGitMaintenanceOperations(t *testing.T) {
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

	packedRefsPath := filepath.Join(repo.GitDir, "packed-refs")
	indexPath := filepath.Join(repo.GitDir, "index")
	assertSymlink(t, packedRefsPath, "Seed")
	assertSymlink(t, indexPath, "Seed")

	// runAgent's own -c user.name/user.email pair gives the agent-side
	// commit below (step (2)) an identity: the agent-owned git-dir's
	// config carries none (only the RUNTIME repo's own .git/config, set
	// by initRunTestRepo, does), and unlike macOS, Linux never
	// auto-detects one from the host -- without this, `git commit`
	// refuses (exit 128, "unable to auto-detect email address") on every
	// Linux CI runner, silently, because runHardened is the only thing
	// that now notices (see this package's own review notes: the OLD
	// runAgent checked only gitdir.Run's returned error, never git's own
	// exit code, so this exact failure passed CI green on macOS -- where
	// user.email auto-detects from the host -- and only failed on Linux,
	// at the LATER `git log` assertion, several lines removed from its
	// real cause). Test-only identity: production Seed/agent config never
	// sets one, because production sandbox-agent never commits.
	runAgent := func(args ...string) {
		t.Helper()
		full := append([]string{"-c", "user.name=T", "-c", "user.email=t@example.com"}, args...)
		runHardened(ctx, t, sup, repo, full...)
	}

	// (1) `git pack-refs --all`, through the agent-owned git-dir --
	// rewrites packed-refs via lockfile-and-rename.
	runAgent("pack-refs", "--all")
	assertSymlink(t, packedRefsPath, "pack-refs --all")

	// (2) An agent-side commit (writes the index via `git add`), then
	// `git gc --prune=now`, through the agent-owned git-dir -- rewrites
	// packed-refs, objects, AND index.
	if err := os.WriteFile(filepath.Join(wt, "gc-trigger.txt"), []byte("gc\n"), 0o644); err != nil {
		t.Fatalf("write gc-trigger.txt: %v", err)
	}
	runAgent("add", "gc-trigger.txt")
	assertSymlink(t, indexPath, "add")
	runAgent("commit", "-qm", "trigger gc")
	runAgent("gc", "--prune=now")
	assertSymlink(t, packedRefsPath, "gc --prune=now")
	assertSymlink(t, indexPath, "gc --prune=now")

	// The runtime's own, entirely unhardened git must still resolve the
	// gc'd history -- proves the shared objects/refs survived compaction,
	// not just that the symlinks themselves are intact.
	if out, err := exec.Command("git", "-C", wt, "log", "--oneline", "-1").CombinedOutput(); err != nil {
		t.Fatalf("git -C %s log --oneline -1 (after gc): %v\n%s", wt, err, out)
	} else if !strings.Contains(string(out), "trigger gc") {
		t.Errorf("git log after gc = %q, want it to contain the gc-triggering commit", out)
	}

	// (3) A stash whose OWN refs/stash entry is itself PACKED (not
	// loose): the runtime stashes a change, the agent packs ALL refs
	// (including refs/stash, verified directly against real git), then
	// the agent pops it -- `stash pop --index` reapplies the INDEX via
	// the same lockfile-and-rename path.
	if err := os.WriteFile(filepath.Join(wt, "f.txt"), []byte("base\n"), 0o644); err != nil {
		t.Fatalf("write f.txt: %v", err)
	}
	runGitForRunTest(t, wt, "add", "f.txt")
	runGitForRunTest(t, wt, "commit", "-qm", "seed f.txt")
	if err := os.WriteFile(filepath.Join(wt, "f.txt"), []byte("stashed\n"), 0o644); err != nil {
		t.Fatalf("write stashed f.txt: %v", err)
	}
	runGitForRunTest(t, wt, "stash", "push", "-q", "-m", "wip")

	runAgent("pack-refs", "--all")
	assertSymlink(t, packedRefsPath, "pack-refs --all (with refs/stash)")
	// Confirm refs/stash itself is really packed now, not loose --
	// otherwise this test would not actually be exercising the packed
	// case P1 exists to pin.
	if _, statErr := os.Lstat(filepath.Join(wt, ".git", "refs", "stash")); !os.IsNotExist(statErr) {
		t.Fatalf("loose .git/refs/stash still exists after pack-refs --all (stat err = %v) -- "+
			"this test's own premise (a PACKED refs/stash) no longer holds", statErr)
	}
	packedRefsContent, err := os.ReadFile(filepath.Join(wt, ".git", "packed-refs"))
	if err != nil {
		t.Fatalf("read wt/.git/packed-refs: %v", err)
	}
	if !strings.Contains(string(packedRefsContent), "refs/stash") {
		t.Fatalf("packed-refs does not contain refs/stash -- this test's own premise (a PACKED refs/stash) no longer holds:\n%s", packedRefsContent)
	}

	runAgent("stash", "pop", "--index")
	assertSymlink(t, packedRefsPath, "stash pop --index (packed refs/stash)")
	assertSymlink(t, indexPath, "stash pop --index (packed refs/stash)")

	// The runtime's own worktree must show the popped content.
	data, err := os.ReadFile(filepath.Join(wt, "f.txt"))
	if err != nil {
		t.Fatalf("read f.txt after stash pop: %v", err)
	}
	if string(data) != "stashed\n" {
		t.Errorf("f.txt after popping the packed stash = %q, want %q", data, "stashed\n")
	}
	if out, err := exec.Command("git", "-C", wt, "stash", "list").CombinedOutput(); err != nil {
		t.Fatalf("git -C %s stash list: %v\n%s", wt, err, out)
	} else if strings.TrimSpace(string(out)) != "" {
		t.Errorf("git stash list after pop = %q, want empty (the packed stash entry must be gone)", out)
	}
}

// TestSparseCheckoutMirror_BothDirectionsAndSeedImport is a queued pin
// (from the implementer's own admission) of the sparse-checkout mirror in
// BOTH directions, plus Seed's own reverse import, in one end-to-end
// test:
//
//  1. Agent-side `sparse-checkout set` -> gitdir.MirrorSparseCheckout ->
//     the RUNTIME's own config says sparse, and the runtime's own NEXT
//     checkout (an ordinary branch switch, its own unprivileged action)
//     keeps the tree sparse -- it must not re-materialize an
//     out-of-scope path.
//  2. Agent-side `sparse-checkout disable` -> gitdir.MirrorSparseCheckout
//     -> the RUNTIME's own config says NOT sparse, and the runtime's
//     NEXT checkout re-materializes the full tree (does not stay
//     sparse).
//  3. gitdir.Seed, called again for a repo the RUNTIME has since left
//     sparse (core.sparseCheckout=true in the runtime's own config),
//     imports that state into the FRESHLY rebuilt agent config --
//     importSparseCheckoutConfig's own reverse-direction counterpart to
//     MirrorSparseCheckout, exercised here for real rather than assumed.
func TestSparseCheckoutMirror_BothDirectionsAndSeedImport(t *testing.T) {
	base := t.TempDir()
	workspaceDir := filepath.Join(base, "workspace")
	wt := filepath.Join(workspaceDir, "repo1")
	initRunTestRepo(t, wt)

	for _, dir := range []string{"apps/web", "apps/api", "contracts/api"} {
		if err := os.MkdirAll(filepath.Join(wt, dir), 0o755); err != nil {
			t.Fatalf("MkdirAll(%s): %v", dir, err)
		}
	}
	if err := os.WriteFile(filepath.Join(wt, "apps/web/index.js"), []byte("web\n"), 0o644); err != nil {
		t.Fatalf("write apps/web/index.js: %v", err)
	}
	if err := os.WriteFile(filepath.Join(wt, "apps/api/index.js"), []byte("api\n"), 0o644); err != nil {
		t.Fatalf("write apps/api/index.js: %v", err)
	}
	if err := os.WriteFile(filepath.Join(wt, "contracts/api/openapi.yaml"), []byte("spec\n"), 0o644); err != nil {
		t.Fatalf("write contracts/api/openapi.yaml: %v", err)
	}
	runGitForRunTest(t, wt, "add", ".")
	runGitForRunTest(t, wt, "commit", "-qm", "add apps/web, apps/api, contracts/api")

	// A second branch whose apps/api/index.js content DIFFERS from main's
	// -- created now, while the tree is still fully materialized, so no
	// sparse-checkout state is in play yet. Round-2 review (R7): step (1)
	// below used to prove "still sparse" only by switching between two
	// branches with IDENTICAL trees (tmp-branch/main) -- git's unpack-trees
	// never even looks at a path whose content is unchanged between the
	// two commits, so apps/api stayed absent whether or not the mirror had
	// actually set core.sparseCheckout on the runtime side. Verified
	// directly against real git: with a shared index/skip-worktree bits
	// (from the agent's own sparse-checkout set) but the RUNTIME's own
	// core.sparseCheckout left unset (the exact "mirror never ran" shape),
	// switching to a branch whose apps/api content genuinely differs DOES
	// re-materialize the file -- so this divergent branch, unlike the
	// same-tree round trip, is a real, config-dependent proof.
	runGitForRunTest(t, wt, "checkout", "-qb", "divergent-api")
	if err := os.WriteFile(filepath.Join(wt, "apps/api/index.js"), []byte("api v2 -- must never land on a sparse runtime\n"), 0o644); err != nil {
		t.Fatalf("write apps/api/index.js (divergent-api): %v", err)
	}
	runGitForRunTest(t, wt, "commit", "-aqm", "apps/api v2 on divergent-api")
	runGitForRunTest(t, wt, "checkout", "-q", "main")

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

	apiFile := filepath.Join(wt, "apps/api/index.js")
	webFile := filepath.Join(wt, "apps/web/index.js")
	mustExist := func(t *testing.T, path, context string) {
		t.Helper()
		if _, err := os.Stat(path); err != nil {
			t.Errorf("%s: %s does not exist (stat err = %v), want it present", context, path, err)
		}
	}
	mustNotExist := func(t *testing.T, path, context string) {
		t.Helper()
		if _, err := os.Stat(path); !os.IsNotExist(err) {
			t.Errorf("%s: %s exists (stat err = %v), want it absent", context, path, err)
		}
	}

	// -- (1) agent sets sparse; runtime's own next checkout stays sparse --
	runHardened(ctx, t, sup, repo, "sparse-checkout", "set", "--no-cone", "--", "/apps/web/*", "/contracts/api/*")
	if err := gitdir.MirrorSparseCheckout(ctx, sup, repo, nil, 10*time.Second, 5*time.Second); err != nil {
		t.Fatalf("MirrorSparseCheckout (set): %v", err)
	}

	mustExist(t, webFile, "after agent sparse-checkout set")
	mustNotExist(t, apiFile, "after agent sparse-checkout set")

	runtimeSparseConfig := strings.TrimSpace(runGitOutputForRunTest(t, wt, "config", "--get", "core.sparseCheckout"))
	if runtimeSparseConfig != "true" {
		t.Fatalf("runtime core.sparseCheckout after mirror(set) = %q, want \"true\"", runtimeSparseConfig)
	}

	// The runtime's own NEXT checkout -- an ordinary, unprivileged branch
	// switch -- must keep the tree sparse (not re-materialize apps/api).
	// This SAME-tree round trip (tmp-branch/main, identical content) is
	// kept as a sanity baseline, but it is NOT on its own proof of
	// anything sparse-config-dependent -- see the divergent-branch check
	// immediately below for why.
	runGitForRunTest(t, wt, "checkout", "-q", "-b", "tmp-branch")
	runGitForRunTest(t, wt, "checkout", "-q", "main")
	mustExist(t, webFile, "after runtime's own checkout, still sparse")
	mustNotExist(t, apiFile, "after runtime's own checkout, still sparse")

	// The REAL, discriminating proof (round-2 review, R7): switch onto
	// divergent-api, whose apps/api/index.js content genuinely DIFFERS
	// from main's. Unlike the same-tree round trip above, git's own
	// unpack-trees must actually decide, for THIS checkout, whether to
	// materialize the new content -- a decision that depends on the
	// RUNTIME's own core.sparseCheckout value (mirrored, or not) and the
	// (shared, via "info") sparse-checkout patterns, not merely on
	// whatever skip-worktree bits already happened to sit in the shared
	// index already. Verified directly against real git: with the
	// runtime's own core.sparseCheckout genuinely unmirrored (left unset),
	// this exact checkout re-materializes apps/api/index.js -- so a no-op
	// mirror is caught here, not just by the config-value read above.
	runGitForRunTest(t, wt, "checkout", "-q", "divergent-api")
	mustNotExist(t, apiFile, "after runtime's own checkout onto a tree that DIFFERS under apps/api, still sparse")
	runGitForRunTest(t, wt, "checkout", "-q", "main")

	// -- (2) agent disables sparse; runtime's own next checkout re-materializes everything --
	runHardened(ctx, t, sup, repo, "sparse-checkout", "disable")
	if err := gitdir.MirrorSparseCheckout(ctx, sup, repo, nil, 10*time.Second, 5*time.Second); err != nil {
		t.Fatalf("MirrorSparseCheckout (disable): %v", err)
	}

	mustExist(t, apiFile, "after agent sparse-checkout disable")

	runtimeSparseConfigAfterDisable := strings.TrimSpace(runGitOutputForRunTestAllowFailure(t, wt, "config", "--get", "core.sparseCheckout"))
	if runtimeSparseConfigAfterDisable == "true" {
		t.Fatalf("runtime core.sparseCheckout after mirror(disable) = %q, want \"false\" or unset", runtimeSparseConfigAfterDisable)
	}

	// (Correction, round-2 review, R7): this used to remove apps/api/
	// index.js by hand and let a PATHSPEC-limited `git checkout -- <path>`
	// restore it -- but that restores the file regardless of whether the
	// mirror actually disabled core.sparseCheckout on the runtime side:
	// the agent's own `sparse-checkout disable`, just above, already
	// clears every skip-worktree bit in the SHARED index unconditionally
	// (§30.5: index is one of the entries Seed symlinks), so a
	// pathspec-limited restore of an already-tracked, already-unmarked
	// path succeeds either way -- it is not sensitive to the runtime's
	// own config at all.
	//
	// The discriminating proof instead: a REAL, full ref switch onto
	// divergent-api, whose apps/api/index.js content DIFFERS from main's
	// -- exactly like step (1)'s own divergent-branch check, but now
	// proving the OPPOSITE direction (materializes, with the CORRECT new
	// content, rather than staying absent). Checking the CONTENT, not
	// just presence, additionally rules out a stale, pre-disable copy of
	// the file satisfying a presence-only check.
	runGitForRunTest(t, wt, "checkout", "-q", "divergent-api")
	mustExist(t, apiFile, "after runtime's own checkout onto a tree that DIFFERS under apps/api, post-disable")
	if data, err := os.ReadFile(apiFile); err != nil {
		t.Fatalf("read apiFile after runtime's own post-disable checkout: %v", err)
	} else if want := "api v2 -- must never land on a sparse runtime\n"; string(data) != want {
		t.Errorf("apiFile content after runtime's own post-disable checkout = %q, want %q (divergent-api's own real content, not a stale/sparse artifact)", data, want)
	}
	runGitForRunTest(t, wt, "checkout", "-q", "main")

	// -- (3) Seed imports a sparse runtime's state into a fresh agent config --
	// Re-enable sparse on the runtime side directly (an ordinary,
	// unprivileged write -- standing in for whatever earlier boot left it
	// this way), WITHOUT going through the agent at all this time, so
	// Seed's own import is what is actually being tested here, not the
	// mirror direction again. Written "--worktree"-scoped, matching how
	// git's own sparse-checkout machinery (and MirrorSparseCheckout
	// itself, since the fix for the linked-worktree finding) actually
	// represents this state once extensions.worktreeConfig is on for this
	// repo (step (2)'s own disable mirror already turned it on) -- a bare,
	// unscoped write here would be silently SHADOWED by that pre-existing
	// worktree-scoped entry at read time (see MirrorSparseCheckout's own
	// doc comment for the identical shadowing hazard, measured directly).
	runGitForRunTest(t, wt, "config", "--worktree", "core.sparseCheckout", "true")

	// A fresh Seed (gitdir.Seed is idempotent -- os.RemoveAll(repo.GitDir)
	// then rebuilt from scratch -- exactly what a warm boot's own
	// (re-)seed does) must import that value into the newly-built agent
	// config.
	if err := gitdir.Seed(ctx, sup, repo, "https://example.invalid/repo1.git", nil, 10*time.Second, 5*time.Second); err != nil {
		t.Fatalf("gitdir.Seed (re-seed, import check): %v", err)
	}

	agentConfigOut := exec.Command("git", "--git-dir", repo.GitDir, "config", "--get", "core.sparseCheckout")
	out, err := agentConfigOut.CombinedOutput()
	if err != nil {
		t.Fatalf("git --git-dir %s config --get core.sparseCheckout: %v\n%s", repo.GitDir, err, out)
	}
	if got := strings.TrimSpace(string(out)); got != "true" {
		t.Errorf("agent core.sparseCheckout after re-seed = %q, want \"true\" (Seed must import the runtime's own sparse state)", got)
	}
}

// runGitOutputForRunTest runs git in dir and returns stdout, failing the
// test on any error.
func runGitOutputForRunTest(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	out, err := cmd.Output()
	if err != nil {
		t.Fatalf("git %v (dir=%s): %v", args, dir, err)
	}
	return string(out)
}

// runGitOutputForRunTestAllowFailure runs git in dir and returns stdout,
// tolerating a non-zero exit (e.g. `config --get` on an unset key, exit
// 1) -- the empty string in that case, never failing the test.
func runGitOutputForRunTestAllowFailure(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	out, _ := cmd.Output()
	return string(out)
}
