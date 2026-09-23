package githarden_test

// Step 171 (§30.5): the split-repo shape, and the classes it closes --
// this file lives in package githarden_test (an EXTERNAL test package),
// not githarden's own internal test file (githarden_test.go, package
// githarden), specifically so it CAN import internal/sandboxagent/gitdir:
// gitdir imports githarden (production), so githarden's OWN internal test
// files cannot import gitdir back without Go reporting an import cycle --
// but an external test package is a separate compilation unit that is
// free to depend on both, exactly like internal/sandboxagent/gitdir's own
// TestSessionConfigEnvVar_MatchesBoot (run_test.go) does for boot.
// Everything here therefore uses ONLY githarden's own EXPORTED surface
// (Args, ArgsForClone, Spec, Env, Repo) -- never hardeningFlags or the
// other unexported helpers githarden_test.go's own transport-class tests
// still need direct access to.

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/narvidev/narvi/internal/sandboxagent/githarden"
	"github.com/narvidev/narvi/internal/sandboxagent/gitdir"
	"github.com/narvidev/narvi/internal/sandboxagent/supervisor"
)

// -- Step 171 (§30.5): the split-repo shape, and the classes it closes --
//
// newSplitRepo builds the REAL, measured production shape: a runtime
// worktree (a plain, non-bare git repo with one commit, exactly like a
// repository the agent runtime owns after §30.5's own chown) plus a REAL,
// seeded agent-owned git-dir (internal/sandboxagent/gitdir.Seed) -- never
// a stand-in for either half. Every test below arms an attack the SAME way
// the runtime legitimately could (an ordinary, unprivileged write into its
// own worktree's .git/config, or a committed .gitattributes any repository
// ships) and then runs sandbox-agent's OWN hardened git -- Args(repo,
// ...), through the agent-owned git-dir -- to prove the class is closed
// STRUCTURALLY, not by an enumerable flag.
func newSplitRepo(t *testing.T) (githarden.Repo, *supervisor.Supervisor) {
	t.Helper()
	base := t.TempDir()
	wt := filepath.Join(base, "workspace", "repo1")
	if err := os.MkdirAll(wt, 0o755); err != nil {
		t.Fatalf("MkdirAll: %v", err)
	}
	gitInRepo(t, wt, "init", "-q", "-b", "main")
	gitInRepo(t, wt, "config", "user.email", "t@example.invalid")
	gitInRepo(t, wt, "config", "user.name", "T")
	if err := os.WriteFile(filepath.Join(wt, "README.md"), []byte("hello\n"), 0o644); err != nil {
		t.Fatalf("write README.md: %v", err)
	}
	gitInRepo(t, wt, "add", ".")
	gitInRepo(t, wt, "commit", "-qm", "initial")

	gitDirRoot := filepath.Join(base, "gitdirs")
	if err := gitdir.EnsureRoot(gitDirRoot); err != nil {
		t.Fatalf("gitdir.EnsureRoot: %v", err)
	}
	layout := gitdir.Layout{Root: gitDirRoot, WorkspaceDir: filepath.Join(base, "workspace")}
	repo := layout.Repo("repo1")
	sup := supervisor.New()
	if err := gitdir.Seed(context.Background(), sup, repo, "https://example.invalid/repo1.git", nil, 10*time.Second, 5*time.Second); err != nil {
		t.Fatalf("gitdir.Seed: %v", err)
	}
	return repo, sup
}

// hardenedGit runs `git <Args(repo, args...)>` directly (a plain
// exec.Command, not through gitdir.Run) -- these closed-class proofs are
// about githarden.Args' own structural guarantee in isolation, independent
// of gitdir.Run's own separate HEAD-sync bracket (covered by this
// package's own sibling tests in internal/sandboxagent/gitdir and by the
// dedicated HEAD-sync-out test below, which DOES go through gitdir.Run on
// purpose).
func hardenedGit(t *testing.T, repo githarden.Repo, args ...string) (string, error) {
	t.Helper()
	cmd := exec.Command("git", githarden.Args(repo, args...)...)
	cmd.Env = githarden.Env(gitEnv())
	out, err := cmd.CombinedOutput()
	return string(out), err
}

// TestClosedClass_ContentFilterDoesNotExecute is the direct successor to
// the deleted TestOpenClass_ContentFilterExecutes: the SAME attack (an
// armed filter.evil.smudge plus a committed ".gitattributes" naming it for
// victim.txt), planted on the runtime's own worktree .git/config exactly
// as an ordinary, unprivileged write from the runtime would, but now
// checked out through the agent-owned git-dir. The driver must NOT run,
// and the restored content must be byte-for-byte correct (proving the
// checkout itself still worked -- the filter was skipped, not the whole
// checkout).
func TestClosedClass_ContentFilterDoesNotExecute(t *testing.T) {
	repo, _ := newSplitRepo(t)
	marker := filepath.Join(t.TempDir(), "EXECUTED")

	if err := os.WriteFile(filepath.Join(repo.WorkTree, ".gitattributes"), []byte("victim.txt filter=evil\n"), 0o644); err != nil {
		t.Fatalf("write .gitattributes: %v", err)
	}
	if err := os.WriteFile(filepath.Join(repo.WorkTree, "victim.txt"), []byte("content\n"), 0o644); err != nil {
		t.Fatalf("write victim: %v", err)
	}
	gitInRepo(t, repo.WorkTree, "add", "-A")
	gitInRepo(t, repo.WorkTree, "commit", "-qm", "seed victim + attributes")
	// The config half: an ordinary unprivileged write for the runtime,
	// against ITS OWN .git/config -- never the agent's.
	gitInRepo(t, repo.WorkTree, "config", "filter.evil.smudge", payload(marker, "cat"))

	if err := os.Remove(filepath.Join(repo.WorkTree, "victim.txt")); err != nil {
		t.Fatalf("remove victim: %v", err)
	}

	// The agent-side checkout -- the exact call shape SyncAll/CleanForImageBuild
	// make, via the agent-owned git-dir.
	if out, err := hardenedGit(t, repo, "checkout", "--", "victim.txt"); err != nil {
		t.Fatalf("git checkout -- victim.txt: %v\n%s", err, out)
	}

	if _, statErr := os.Stat(marker); statErr == nil {
		t.Fatal("the armed smudge filter EXECUTED even through the agent-owned git-dir -- if this now fails, the class is not actually closed")
	}
	data, err := os.ReadFile(filepath.Join(repo.WorkTree, "victim.txt"))
	if err != nil {
		t.Fatalf("read restored victim.txt: %v", err)
	}
	if string(data) != "content\n" {
		t.Errorf("victim.txt content = %q, want %q (checkout must still succeed byte-for-byte; only the filter is inert)", data, "content\n")
	}
}

// TestClosedClass_MergeDriverDoesNotExecute is the direct successor to the
// deleted TestOpenClass_MergeDriverExecutes: same attack, same planting
// discipline (runtime's own worktree .git/config), now checked via a real
// three-way merge run through the agent-owned git-dir.
func TestClosedClass_MergeDriverDoesNotExecute(t *testing.T) {
	repo, _ := newSplitRepo(t)
	marker := filepath.Join(t.TempDir(), "EXECUTED")

	if err := os.WriteFile(filepath.Join(repo.WorkTree, ".gitattributes"), []byte("f.txt merge=evil\n"), 0o644); err != nil {
		t.Fatalf("write .gitattributes: %v", err)
	}
	if err := os.WriteFile(filepath.Join(repo.WorkTree, "f.txt"), []byte("base\n"), 0o644); err != nil {
		t.Fatalf("write f: %v", err)
	}
	gitInRepo(t, repo.WorkTree, "add", "-A")
	gitInRepo(t, repo.WorkTree, "commit", "-qm", "seed")
	gitInRepo(t, repo.WorkTree, "config", "merge.evil.driver", payload(marker, "true"))

	gitInRepo(t, repo.WorkTree, "checkout", "-q", "-b", "theirs")
	if err := os.WriteFile(filepath.Join(repo.WorkTree, "f.txt"), []byte("theirs\n"), 0o644); err != nil {
		t.Fatalf("write theirs: %v", err)
	}
	gitInRepo(t, repo.WorkTree, "commit", "-qam", "theirs")
	gitInRepo(t, repo.WorkTree, "checkout", "-q", "-")
	if err := os.WriteFile(filepath.Join(repo.WorkTree, "f.txt"), []byte("ours\n"), 0o644); err != nil {
		t.Fatalf("write ours: %v", err)
	}
	gitInRepo(t, repo.WorkTree, "commit", "-qam", "ours")

	mergeOut, mergeErr := hardenedGit(t, repo, "merge", "theirs")

	// Precondition: git must actually have attempted a three-way merge of
	// the armed path.
	if !strings.Contains(mergeOut, "f.txt") {
		t.Fatalf("precondition failed: git never attempted a three-way merge of the armed path.\ngit merge said: %v\n%s", mergeErr, mergeOut)
	}

	if _, statErr := os.Stat(marker); statErr == nil {
		t.Fatalf("the armed merge driver EXECUTED even through the agent-owned git-dir\ngit merge said: %v\n%s", mergeErr, mergeOut)
	}
}

// TestClosedClass_StashPopIndexDoesNotExecuteEither proves the SAME merge-
// driver class closes for the OTHER call shape this codebase actually
// reaches it through: `git stash pop --index` (gitclone's own syncOne),
// whose own reapplication is itself a three-way merge.
func TestClosedClass_StashPopIndexDoesNotExecuteEither(t *testing.T) {
	repo, _ := newSplitRepo(t)
	marker := filepath.Join(t.TempDir(), "EXECUTED")

	if err := os.WriteFile(filepath.Join(repo.WorkTree, ".gitattributes"), []byte("f.txt merge=evil\n"), 0o644); err != nil {
		t.Fatalf("write .gitattributes: %v", err)
	}
	if err := os.WriteFile(filepath.Join(repo.WorkTree, "f.txt"), []byte("base\n"), 0o644); err != nil {
		t.Fatalf("write f: %v", err)
	}
	gitInRepo(t, repo.WorkTree, "add", "-A")
	gitInRepo(t, repo.WorkTree, "commit", "-qm", "seed")
	gitInRepo(t, repo.WorkTree, "config", "merge.evil.driver", payload(marker, "true"))

	// Dirty the tree, stash it (runtime side -- an ordinary user action),
	// then diverge f.txt on HEAD so the pop's own reapplication is a real
	// three-way merge, not a trivial fast-forward.
	if err := os.WriteFile(filepath.Join(repo.WorkTree, "f.txt"), []byte("stashed\n"), 0o644); err != nil {
		t.Fatalf("write stashed: %v", err)
	}
	gitInRepo(t, repo.WorkTree, "stash", "push", "-m", "wip")
	if err := os.WriteFile(filepath.Join(repo.WorkTree, "f.txt"), []byte("head moved on\n"), 0o644); err != nil {
		t.Fatalf("write head-moved: %v", err)
	}
	gitInRepo(t, repo.WorkTree, "commit", "-qam", "head moved on")

	popOut, popErr := hardenedGit(t, repo, "stash", "pop", "--index")
	if !strings.Contains(popOut, "f.txt") && popErr == nil {
		t.Fatalf("precondition unclear: stash pop did not appear to touch f.txt at all\n%s", popOut)
	}

	if _, statErr := os.Stat(marker); statErr == nil {
		t.Fatalf("the armed merge driver EXECUTED during `stash pop --index` through the agent-owned git-dir\n%s", popOut)
	}
}

// TestClosedClass_RuntimeHookDoesNotExecute proves a runtime-planted hook
// (§30.5's own original "formerly-quiet" concern: a runtime-planted
// .git/hooks/pre-commit handing a prompt-injected agent arbitrary
// execution) never runs: --git-dir=<agent-dir> means $GIT_DIR resolves to
// the agent's OWN git-dir, whose own hooks/ (gitdir.Seed's step 4) is
// empty and agent-owned -- the runtime's own hooks/ directory is never
// even consulted, independent of core.hooksPath=/dev/null's own
// defense-in-depth value.
func TestClosedClass_RuntimeHookDoesNotExecute(t *testing.T) {
	repo, _ := newSplitRepo(t)
	marker := filepath.Join(t.TempDir(), "EXECUTED")

	hookPath := filepath.Join(repo.WorkTree, ".git", "hooks", "pre-commit")
	hookBody := "#!/bin/sh\n" + payload(marker, "exit 0") + "\n"
	if err := os.WriteFile(hookPath, []byte(hookBody), 0o755); err != nil {
		t.Fatalf("write runtime pre-commit hook: %v", err)
	}

	out, err := hardenedGit(t, repo, "commit", "--allow-empty", "-m", "trigger pre-commit")
	if err != nil {
		t.Fatalf("git commit --allow-empty: %v\n%s", err, out)
	}

	if _, statErr := os.Stat(marker); statErr == nil {
		t.Fatal("the runtime-planted pre-commit hook EXECUTED through the agent-owned git-dir")
	}
}

// TestSharedObjects_VisibleBothWays pins the objects-symlink-not-alternates
// decision (docs/DECISIONS.md): a DIRECTORY symlink for objects/, not
// "objects/info/alternates" -- alternates was measured and rejected because
// it does not make agent-written objects visible to the runtime's own git
// (a real, both-ways requirement). This proves both directions with a real
// commit each way: the agent commits a new file, and the RUNTIME's own
// (unhardened, "-C wt") git can read it back; then the runtime commits a
// new file, and the AGENT's own git-dir can read THAT back.
func TestSharedObjects_VisibleBothWays(t *testing.T) {
	repo, _ := newSplitRepo(t)

	// Agent side: a real commit through the agent-owned git-dir.
	if err := os.WriteFile(filepath.Join(repo.WorkTree, "from-agent.txt"), []byte("agent wrote this\n"), 0o644); err != nil {
		t.Fatalf("write from-agent.txt: %v", err)
	}
	if out, err := hardenedGit(t, repo, "add", "from-agent.txt"); err != nil {
		t.Fatalf("git add (agent side): %v\n%s", err, out)
	}
	if out, err := hardenedGit(t, repo, "commit", "-m", "agent commit"); err != nil {
		t.Fatalf("git commit (agent side): %v\n%s", err, out)
	}
	agentSHA := strings.TrimSpace(mustHardenedGit(t, repo, "rev-parse", "HEAD"))

	// The RUNTIME's own, unhardened git (a plain "-C wt", no --git-dir
	// override at all) must be able to resolve that exact commit -- proof
	// the object is genuinely shared, not merely agent-local.
	cmd := exec.Command("git", "-C", repo.WorkTree, "cat-file", "-e", agentSHA)
	cmd.Env = gitEnv()
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("runtime git cannot see the agent's own commit %s: %v\n%s", agentSHA, err, out)
	}

	// Runtime side: a real commit through the runtime's own, unhardened
	// git.
	if err := os.WriteFile(filepath.Join(repo.WorkTree, "from-runtime.txt"), []byte("runtime wrote this\n"), 0o644); err != nil {
		t.Fatalf("write from-runtime.txt: %v", err)
	}
	gitInRepo(t, repo.WorkTree, "add", "from-runtime.txt")
	gitInRepo(t, repo.WorkTree, "commit", "-qm", "runtime commit")
	runtimeCmd := exec.Command("git", "-C", repo.WorkTree, "rev-parse", "HEAD")
	runtimeCmd.Env = gitEnv()
	runtimeSHAOut, err := runtimeCmd.Output()
	if err != nil {
		t.Fatalf("git rev-parse HEAD (runtime side): %v", err)
	}
	runtimeSHA := strings.TrimSpace(string(runtimeSHAOut))

	// The AGENT's own git-dir must be able to resolve THAT commit too.
	if out, err := hardenedGit(t, repo, "cat-file", "-e", runtimeSHA); err != nil {
		t.Fatalf("agent git-dir cannot see the runtime's own commit %s: %v\n%s", runtimeSHA, err, out)
	}
}

// mustHardenedGit is hardenedGit's own "fail the test on error" variant,
// for call sites that only want the stdout, not an (out, err) pair to
// inspect themselves.
func mustHardenedGit(t *testing.T, repo githarden.Repo, args ...string) string {
	t.Helper()
	out, err := hardenedGit(t, repo, args...)
	if err != nil {
		t.Fatalf("git %s: %v\n%s", strings.Join(args, " "), err, out)
	}
	return out
}

// TestCleanFDX_LeavesGitDirsIntact proves `git clean -fdx` (gitclone's own
// CleanForImageBuild, run through the agent-owned git-dir) never touches
// EITHER .git -- git's own clean(1) always excludes ".git" from an
// untracked-file sweep, but this is the executable proof for the split
// shape specifically: the runtime's own real ".git" directory and the
// agent's own separately-rooted git-dir must both survive.
func TestCleanFDX_LeavesGitDirsIntact(t *testing.T) {
	repo, _ := newSplitRepo(t)
	if err := os.WriteFile(filepath.Join(repo.WorkTree, "untracked-residue.txt"), []byte("residue\n"), 0o644); err != nil {
		t.Fatalf("write untracked residue: %v", err)
	}

	if out, err := hardenedGit(t, repo, "clean", "-fdx"); err != nil {
		t.Fatalf("git clean -fdx: %v\n%s", err, out)
	}

	if _, err := os.Stat(filepath.Join(repo.WorkTree, ".git")); err != nil {
		t.Errorf("runtime .git missing after clean -fdx: %v", err)
	}
	if _, err := os.Stat(repo.GitDir); err != nil {
		t.Errorf("agent git-dir missing after clean -fdx: %v", err)
	}
	if _, err := os.Stat(filepath.Join(repo.WorkTree, "untracked-residue.txt")); !os.IsNotExist(err) {
		t.Errorf("untracked-residue.txt survived clean -fdx (stat err = %v), want it gone", err)
	}
}

// TestHeadSyncedOut_AfterAgentCheckoutMinusB_RuntimeCommitsOntoRightBranch
// is the executable proof behind mutation-verify point (c): gitdir.Run's
// own SyncHeadOut bracket is what keeps the runtime's own worktree HEAD
// following an agent-side branch checkout/creation. Deliberately routed
// through gitdir.Run (not the bare hardenedGit helper above, which
// bypasses that bracket on purpose for the structural-closure proofs) --
// this is the one test in this file that specifically needs it.
func TestHeadSyncedOut_AfterAgentCheckoutMinusB_RuntimeCommitsOntoRightBranch(t *testing.T) {
	repo, sup := newSplitRepo(t)
	ctx := context.Background()

	spec := githarden.Spec(repo, "checkout", "-b", "feature-x")
	if _, err := gitdir.Run(ctx, sup, repo, nil, spec, 10*time.Second, 5*time.Second); err != nil {
		t.Fatalf("gitdir.Run(checkout -b feature-x): %v", err)
	}

	if got := currentBranch(t, repo.WorkTree); got != "feature-x" {
		t.Fatalf("runtime worktree branch after agent checkout -b = %q, want %q -- SyncHeadOut did not run", got, "feature-x")
	}

	// The runtime then commits, entirely on its own (no agent-owned
	// git-dir involved at all) -- this commit must land on feature-x.
	if err := os.WriteFile(filepath.Join(repo.WorkTree, "feature.txt"), []byte("feature\n"), 0o644); err != nil {
		t.Fatalf("write feature.txt: %v", err)
	}
	gitInRepo(t, repo.WorkTree, "add", "feature.txt")
	gitInRepo(t, repo.WorkTree, "commit", "-qm", "feature commit")

	if got := currentBranch(t, repo.WorkTree); got != "feature-x" {
		t.Fatalf("runtime worktree branch after its own commit = %q, want %q", got, "feature-x")
	}

	logCmd := exec.Command("git", "-C", repo.WorkTree, "log", "--oneline", "main..feature-x")
	logCmd.Env = gitEnv()
	out, err := logCmd.Output()
	if err != nil {
		t.Fatalf("git log main..feature-x: %v", err)
	}
	if !strings.Contains(string(out), "feature commit") {
		t.Errorf("feature-x does not contain the runtime's own commit (git log main..feature-x = %q) -- it landed on the wrong branch", out)
	}
}

func currentBranch(t *testing.T, dir string) string {
	t.Helper()
	cmd := exec.Command("git", "-C", dir, "rev-parse", "--abbrev-ref", "HEAD")
	cmd.Env = gitEnv()
	out, err := cmd.Output()
	if err != nil {
		t.Fatalf("git -C %s rev-parse --abbrev-ref HEAD: %v", dir, err)
	}
	return strings.TrimSpace(string(out))
}


// gitEnv/gitInRepo/payload duplicate githarden_test.go's own identically-
// named helpers (package githarden, an internal test file) -- a DIFFERENT
// Go package from this file even though it lives in the same directory,
// so nothing there is visible here. Mirrors this codebase's own existing
// precedent for the identical situation (internal/sandboxagent/boot's own
// mkdirAllInternal/initGitRepoInternal in depsladder_internal_test.go,
// duplicating boot_test's fingerprint_test.go helpers).

// gitEnv is os.Environ plus a fixed identity -- every git command this
// file spawns needs it.
func gitEnv() []string {
	return append(os.Environ(),
		"GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@example.invalid",
		"GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@example.invalid",
	)
}

// gitInRepo runs git in dir with a fixed identity, failing the test on error.
func gitInRepo(t *testing.T, dir string, args ...string) {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	cmd.Env = gitEnv()
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git %s: %v\n%s", strings.Join(args, " "), err, out)
	}
}

// payload returns a shell command that touches marker and then behaves.
func payload(marker, passthrough string) string {
	return "touch " + marker + "; " + passthrough
}

// TestTransportClass_AgentConfigArbitraryHelperBlockedByAllowProtocol is
// TestTransportClass_ArbitraryRemoteHelperBlockedByAllowProtocol's own
// Step 171 (§30.5) companion: the SAME "foo::" arbitrary-remote-helper
// attack, but planted directly in the AGENT's OWN seeded git-dir config
// (the one config source the split shape can no longer keep the runtime
// away from BY CONSTRUCTION -- gitdir.Seed itself only ever writes
// remote.origin.* plus the fixed core.*/gc.* keys named in its own doc
// comment, so this scenario models a hypothetical regression in Seed, or
// any other future code path, that let an arbitrary key reach the agent's
// own config) -- proving GIT_ALLOW_PROTOCOL remains the guarantee for this
// class regardless of WHICH config it was planted in, not merely a
// property of the runtime-facing side that the split now makes moot.
func TestTransportClass_AgentConfigArbitraryHelperBlockedByAllowProtocol(t *testing.T) {
	repo, _ := newSplitRepo(t)
	marker := filepath.Join(t.TempDir(), "EXECUTED")

	agentConfig := func(key, value string) {
		t.Helper()
		cmd := exec.Command("git", "--git-dir", repo.GitDir, "config", key, value)
		cmd.Env = gitEnv()
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git --git-dir %s config %s %s: %v\n%s", repo.GitDir, key, value, err, out)
		}
	}
	agentConfig("protocol.foo.allow", "always")
	agentConfig("alias.remote-foo", "!"+payload(marker, "true"))
	agentConfig("remote.origin.url", "foo::whatever")

	// FIXED: Args()+Env() must block it even planted here.
	fixed := exec.Command("git", githarden.Args(repo, "fetch", "origin")...)
	fixed.Env = githarden.Env(gitEnv())
	_ = fixed.Run()
	if _, err := os.Stat(marker); err == nil {
		t.Fatal("the foo:: alias ran through the agent's own git-dir even with Args()+Env() (GIT_ALLOW_PROTOCOL=https) in place")
	}

	// MUTATION: drop GIT_ALLOW_PROTOCOL -- must reopen.
	mutated := exec.Command("git", githarden.Args(repo, "fetch", "origin")...)
	mutated.Env = gitEnv()
	_ = mutated.Run()
	if _, err := os.Stat(marker); err != nil {
		t.Fatalf("mutation: removing GIT_ALLOW_PROTOCOL did not re-open the attack against the agent's own config (marker absent)")
	}
}
