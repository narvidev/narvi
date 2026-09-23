//go:build integration

// Adversarial-review test, added to independently verify §3.4's
// ("gitstate in-sandbox", §3.4) own claim that runBootSequence's
// BootModeBuild-only clean-tree-before-snapshot step (main.go, guarded by
// "if cfg.BootMode == sandboxboot.BootModeBuild") actually runs as part of
// a REAL boot sequence -- not just gitclone.CleanForImageBuild called in
// isolation (already covered directly by internal/sandboxagent/gitclone/
// sync_test.go's own TestCleanForImageBuild_* tests) -- and that
// BootModeFresh, which shares the exact same CloneAll/hook-running code
// path, does NOT run it. Reuses this package's own existing git-http-
// backend TLS test-server harness (startGitHTTPServer/mustRunGit,
// push_integration_test.go, same package) rather than duplicating it.
//
// This calls runBootSequence directly, in-process (no subprocess) --
// unlike push_integration_test.go, this path never re-execs the sandbox-
// agent binary itself (no credential-helper round trip is exercised: the
// git-http-backend server here allows anonymous clone), so os.Executable()
// resolving to the `go test` binary rather than a real compiled one is
// harmless (CredHelperGitArg's value is computed but never actually
// invoked by git for an unauthenticated clone).
package main

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/narvidev/narvi/contracts/gen/go/sessionconfig"
	"github.com/narvidev/narvi/internal/domain/sandboxboot"
	"github.com/narvidev/narvi/internal/platform"
	"github.com/narvidev/narvi/internal/sandboxagent/boot"
	"github.com/narvidev/narvi/internal/sandboxagent/credentials"
	"github.com/narvidev/narvi/internal/sandboxagent/gitdir"
	"github.com/narvidev/narvi/internal/sandboxagent/services"
	"github.com/narvidev/narvi/internal/sandboxagent/supervisor"
)

const bootSequenceTestTimeout = 45 * time.Second

// setUpResidueRepoAndServer creates a real bare repo, seeded with one
// commit containing an executable setup.sh that (a) writes a marker file
// OUTSIDE the git repo entirely (proof setup.sh really ran) and (b) leaves
// REAL dirty residue INSIDE the repo: an untracked file and an uncommitted
// modification to a tracked file -- exactly the "setup.sh residue" §3.4's
// clean-tree step exists to discard. Served over the same real git-http-
// backend TLS harness push_integration_test.go already establishes.
func setUpResidueRepoAndServer(t *testing.T, markerDir string) (gitServerURL string) {
	t.Helper()

	reposParent := t.TempDir()
	bareRepoDir := filepath.Join(reposParent, "repo.git")
	if err := os.MkdirAll(bareRepoDir, 0o755); err != nil {
		t.Fatalf("mkdir bare repo dir: %v", err)
	}
	mustRunGit(t, reposParent, "init", "--bare", "-b", "main", bareRepoDir)

	seedDir := t.TempDir()
	mustRunGit(t, reposParent, "clone", bareRepoDir, seedDir)

	if err := os.WriteFile(filepath.Join(seedDir, "README.md"), []byte("committed content\n"), 0o644); err != nil {
		t.Fatalf("write README.md: %v", err)
	}

	setupScript := "#!/bin/sh\n" +
		"set -e\n" +
		"touch \"" + markerDir + "/setup-ran\"\n" +
		"echo residue > untracked-residue.txt\n" +
		"echo dirty-modification > README.md\n"
	setupPath := filepath.Join(seedDir, "setup.sh")
	if err := os.WriteFile(setupPath, []byte(setupScript), 0o755); err != nil {
		t.Fatalf("write setup.sh: %v", err)
	}

	mustRunGit(t, seedDir, "add", "README.md", "setup.sh")
	mustRunGit(t, seedDir, "commit", "-m", "seed commit with setup.sh")
	mustRunGit(t, seedDir, "push", "origin", "main")

	gitServer := startGitHTTPServer(t, reposParent)
	return gitServer.URL + "/repo.git"
}

// runBootSequenceForMode invokes runBootSequence directly (in-process),
// exactly as run() itself does, for one BootMode, cloning repoURL into a
// fresh workspace directory via the REAL, unmodified CloneAll path.
func runBootSequenceForMode(t *testing.T, mode sandboxboot.BootMode, repoURL string) (workspaceDir string, bootErr error) {
	t.Helper()
	workspaceDir, bootErr = runBootSequenceForModeWithCredentialCacheDir(t, mode, repoURL, t.TempDir())
	return workspaceDir, bootErr
}

// runBootSequenceForModeWithCredentialCacheDir is runBootSequenceForMode's
// general-purpose form: credentialCacheDir is the caller's own choice
// (rather than a fresh t.TempDir() runBootSequenceForMode picks internally)
// so a test can pre-seed it with a credential BEFORE calling runBootSequence,
// then assert on its state afterward -- §30.4(3)'s own "a boot-time cache
// purge in all modes" needs exactly that shape to prove end to end.
func runBootSequenceForModeWithCredentialCacheDir(t *testing.T, mode sandboxboot.BootMode, repoURL, credentialCacheDir string) (workspaceDir string, bootErr error) {
	t.Helper()

	// The git-http-backend test server (startGitHTTPServer) uses a real
	// but self-signed TLS cert -- trusted here ONLY because this is a
	// throwaway test server, never anything resembling production
	// configuration. Inherited by the git clone subprocess via
	// supervisor.Spec's own nil-Env "inherit this process's environment"
	// convention (cloneOne, gitclone/clone.go), mirroring
	// push_integration_test.go's own runSandboxAgent identically.
	t.Setenv("GIT_SSL_NO_VERIFY", "true")

	workspaceDir = t.TempDir()
	sup := supervisor.New()

	cfg := boot.Config{
		BootMode:           mode,
		WorkspaceDir:       workspaceDir,
		CredentialCacheDir: credentialCacheDir,
		// §30.5: CloneAll now re-owns each repo's own worktree
		// for the runtime (boot.ChownWorkspaceForRuntime) immediately after
		// seeding its agent git-dir, a real lchown that requires
		// CAP_CHOWN/root unless the target uid/gid is THIS test process's
		// own -- exactly the same "self" escape hatch push_integration_test.
		// go's own runSandboxAgent already documents for the identical
		// reason.
		RuntimeUID: uint32(os.Getuid()),
		RuntimeGID: uint32(os.Getgid()),
		SessionConfig: &sessionconfig.SessionConfig{
			BootMode:          sessionconfig.SessionConfigBootMode(mode),
			ControlPlaneWsUrl: "wss://unused.invalid/ws",
			Gen:               1,
			Repos: []sessionconfig.SessionConfigReposElem{
				{Name: "repo1", Url: repoURL},
			},
			SandboxId:    "5b1c1e2e-6b1a-4b1a-9b1a-6b1a4b1a9b1a",
			SandboxToken: "test-sandbox-token",
			SessionId:    "boot-sequence-test-session",
		},
	}

	ctx, cancel := context.WithTimeout(context.Background(), bootSequenceTestTimeout)
	defer cancel()

	noopProgress := func(services.BootProgressEvent) {}
	noopGitSync := func(string, string, string) {}
	noopFetchTiming := func(string, float64, bool) {}
	noopCheckoutTiming := func(string, float64, bool) {}
	noopHookTiming := func(string, string, string, bool, bool, float64) {}

	bootErr = runBootSequence(ctx, sup, cfg, gitdir.Layout{Root: t.TempDir(), WorkspaceDir: cfg.WorkspaceDir}, nil, platform.DefaultTimeouts(), nil, nil, noopProgress, noopGitSync, noopFetchTiming, noopCheckoutTiming, noopHookTiming)
	return workspaceDir, bootErr
}

// TestRunBootSequence_BootModeBuild_CleanTreeStepDiscardsSetupResidue
// proves finding (3) end to end: a REAL BootModeBuild boot, through
// runBootSequence itself (not gitclone.CleanForImageBuild called in
// isolation), clones a repo whose setup.sh leaves real dirty residue, and
// the workspace is fully clean (git status --porcelain empty, residue file
// gone, tracked-file modification reverted) once runBootSequence returns
// -- §3.4: "Image builds must snapshot a clean tree".
func TestRunBootSequence_BootModeBuild_CleanTreeStepDiscardsSetupResidue(t *testing.T) {
	markerDir := t.TempDir()
	repoURL := setUpResidueRepoAndServer(t, markerDir)

	workspaceDir, bootErr := runBootSequenceForMode(t, sandboxboot.BootModeBuild, repoURL)
	if bootErr != nil {
		t.Fatalf("runBootSequence(BootModeBuild) error = %v, want nil", bootErr)
	}

	// setup.sh genuinely ran (not skipped) -- the marker file exists
	// outside the repo entirely.
	if _, err := os.Stat(filepath.Join(markerDir, "setup-ran")); err != nil {
		t.Fatalf("setup.sh marker file missing, setup.sh did not run: %v", err)
	}

	repoDir := filepath.Join(workspaceDir, "repo1")

	// The clean-tree step must have discarded ALL of setup.sh's residue:
	// tree is fully clean.
	status := gitOutput(t, repoDir, "status", "--porcelain")
	if status != "" {
		t.Errorf("git status --porcelain after BootModeBuild boot = %q, want empty (clean-tree step should have run)", status)
	}

	// The untracked residue file must be genuinely gone from disk (`git
	// clean -fdx`), not merely unreported by status.
	if _, err := os.Stat(filepath.Join(repoDir, "untracked-residue.txt")); !os.IsNotExist(err) {
		t.Errorf("untracked-residue.txt stat = %v, want IsNotExist (must be discarded by the clean-tree step)", err)
	}

	// The tracked file's dirty modification must be reverted to its
	// committed content (`git checkout -- .`).
	data, err := os.ReadFile(filepath.Join(repoDir, "README.md"))
	if err != nil {
		t.Fatalf("read README.md: %v", err)
	}
	if string(data) != "committed content\n" {
		t.Errorf("README.md = %q, want the committed content restored", data)
	}
}

// TestRunBootSequence_BootModeFresh_DoesNotRunCleanTreeStep proves finding
// (4)'s BootModeFresh half: the EXACT SAME setup.sh-residue-producing repo,
// booted via BootModeFresh instead (which shares the identical
// CloneAll-then-RunHooks code path as BootModeBuild in runBootSequence's
// own "default" switch case), is left DIRTY -- the clean-tree step must be
// gated strictly on BootModeBuild, never running for BootModeFresh.
func TestRunBootSequence_BootModeFresh_DoesNotRunCleanTreeStep(t *testing.T) {
	markerDir := t.TempDir()
	repoURL := setUpResidueRepoAndServer(t, markerDir)

	workspaceDir, bootErr := runBootSequenceForMode(t, sandboxboot.BootModeFresh, repoURL)
	if bootErr != nil {
		t.Fatalf("runBootSequence(BootModeFresh) error = %v, want nil", bootErr)
	}

	if _, err := os.Stat(filepath.Join(markerDir, "setup-ran")); err != nil {
		t.Fatalf("setup.sh marker file missing, setup.sh did not run: %v", err)
	}

	repoDir := filepath.Join(workspaceDir, "repo1")

	// setup.sh's residue must SURVIVE for BootModeFresh: the clean-tree
	// step is BootModeBuild-only (§3.4 Part E), so a fresh boot's
	// own dirty tree is left exactly as setup.sh produced it.
	status := gitOutput(t, repoDir, "status", "--porcelain")
	if status == "" {
		t.Error("git status --porcelain after BootModeFresh boot = empty, want dirty residue to survive (clean-tree step must not run outside BootModeBuild)")
	}
	if _, err := os.Stat(filepath.Join(repoDir, "untracked-residue.txt")); err != nil {
		t.Errorf("untracked-residue.txt stat = %v, want it to still exist (BootModeFresh must not discard setup.sh residue)", err)
	}
}

// TestRunBootSequence_PurgesCredentialCacheAtBootStart is §30.4(3)'s own
// "a boot-time cache purge in all modes" test, proven through
// runBootSequence itself: a credential is seeded into cfg.
// CredentialCacheDir BEFORE boot, as if left over from a prior boot cycle
// of a reused sandbox/warm-resumed provider instance, and must be gone
// once runBootSequence returns -- for BootModeFresh, which shares no code
// path with CleanForImageBuild's own end-of-BootModeBuild purge, so this
// specifically proves the UNCONDITIONAL, start-of-boot purge runs on its
// own. Explicitly NOT load-bearing on its own per §30.4(3)'s own wording
// (the snapshot-mint-time purge is) -- this test proves the mechanism
// exists and runs, not that it alone closes the snapshot-restore gap.
func TestRunBootSequence_PurgesCredentialCacheAtBootStart(t *testing.T) {
	markerDir := t.TempDir()
	repoURL := setUpResidueRepoAndServer(t, markerDir)

	credentialCacheDir := t.TempDir()
	leftoverCache := &credentials.Cache{Dir: credentialCacheDir}
	if err := leftoverCache.Store("github.com", credentials.Credential{
		Username: "x-access-token", Password: "leftover-token-from-a-prior-boot-cycle", ExpiresAt: time.Now().Add(time.Hour),
	}); err != nil {
		t.Fatalf("seed leftover credential cache: %v", err)
	}

	_, bootErr := runBootSequenceForModeWithCredentialCacheDir(t, sandboxboot.BootModeFresh, repoURL, credentialCacheDir)
	if bootErr != nil {
		t.Fatalf("runBootSequence(BootModeFresh) error = %v, want nil", bootErr)
	}

	if _, found, err := leftoverCache.Load("github.com"); err != nil || found {
		t.Errorf("leftoverCache.Load(\"github.com\") after boot = (found=%v, err=%v), want (false, nil) -- the boot-start purge must remove it", found, err)
	}
}

// gitOutput runs git with args in dir and returns its trimmed stdout,
// failing the test on any error -- mirrors internal/sandboxagent/gitclone/
// sync_test.go's own identically-named helper (a different package, so not
// importable directly; trivial enough not to warrant a shared test-support
// package).
func gitOutput(t *testing.T, dir string, args ...string) string {
	t.Helper()

	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	out, err := cmd.Output()
	if err != nil {
		t.Fatalf("git %v (dir=%s) failed: %v", args, dir, err)
	}
	return strings.TrimSpace(string(out))
}
