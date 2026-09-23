//go:build integration

// New §9.3-class resilience scenario (§19.3/§19.9's own "fetch-fail
// boot" addition): §19.3's fetch-aware SyncAll degrade policy, exercised
// through a REAL, FULL boot sequence (runBootSequence -> gitclone.SyncAll ->
// boot.RunBoot -> hooks) -- not just gitclone's own unit tests (internal/
// sandboxagent/gitclone/sync_test.go's own TestSyncAll_FetchFails_* table),
// proving the degrade policy holds when every other real piece of a boot
// (hook running, AGENTS.md generation, fingerprint logging) is wired in too.
package main

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/narvidev/narvi/contracts/gen/go/sessionconfig"
	"github.com/narvidev/narvi/internal/domain/sandboxboot"
	"github.com/narvidev/narvi/internal/platform"
	"github.com/narvidev/narvi/internal/sandboxagent/boot"
	"github.com/narvidev/narvi/internal/sandboxagent/gitdir"
	"github.com/narvidev/narvi/internal/sandboxagent/services"
	"github.com/narvidev/narvi/internal/sandboxagent/supervisor"
)

// unreachableOriginURL is a certainly-closed local port -- a fast,
// deterministic connection-refused failure for the fetch step, never a
// hang, and never dependent on any real network/DNS resolution.
const unreachableOriginURL = "https://127.0.0.1:1/repo1.git"

// bakeExistingRepoImageWorkspace simulates a repo_image boot's own
// baked-at-build-time workspace: a real repo, cloned once (via the real
// git-http-backend test server, exactly like a build-time clone would),
// living at workspaceDir/repo1 with a real "origin" remote already
// configured -- exactly what gitclone.SyncAll expects to find on disk
// (it never runs `git clone` itself). The caller then typically points
// origin at something unreachable to simulate a boot-time fetch failure
// against an otherwise-real, already-populated workspace.
func bakeExistingRepoImageWorkspace(t *testing.T, workspaceDir string) {
	t.Helper()

	// The git-http-backend test server (startGitHTTPServer) uses a real
	// but self-signed TLS cert -- trusted here ONLY because this is a
	// throwaway test server, never anything resembling production
	// configuration (mirrors bootsequence_cleanbuild_integration_test.go's
	// own identical precedent). Set before the bake clone below AND before
	// runBootSequenceRepoImage's own later SyncAll fetch attempt -- both
	// need it.
	t.Setenv("GIT_SSL_NO_VERIFY", "true")

	reposParent := t.TempDir()
	bareRepoDir := filepath.Join(reposParent, "repo1.git")
	if err := os.MkdirAll(bareRepoDir, 0o755); err != nil {
		t.Fatalf("mkdir bare repo dir: %v", err)
	}
	mustRunGit(t, reposParent, "init", "--bare", "-b", "main", bareRepoDir)

	seedDir := t.TempDir()
	mustRunGit(t, reposParent, "clone", bareRepoDir, seedDir)
	if err := os.WriteFile(filepath.Join(seedDir, "README.md"), []byte("baked content\n"), 0o644); err != nil {
		t.Fatalf("write README.md: %v", err)
	}
	mustRunGit(t, seedDir, "add", "README.md")
	mustRunGit(t, seedDir, "commit", "-m", "seed commit")
	mustRunGit(t, seedDir, "push", "origin", "main")

	server := startGitHTTPServer(t, reposParent)

	if err := os.MkdirAll(workspaceDir, 0o755); err != nil {
		t.Fatalf("mkdir workspace dir: %v", err)
	}
	mustRunGit(t, workspaceDir, "clone", server.URL+"/repo1.git", "repo1")
}

// runBootSequenceRepoImage invokes runBootSequence directly (in-process),
// exactly as run() itself does, against workspaceDir's own ALREADY-EXISTING
// repo1 (baked via bakeExistingRepoImageWorkspace above) -- BootModeRepoImage,
// which drives gitclone.SyncAll, never CloneAll (SyncAll never clones into
// an existing directory).
func runBootSequenceRepoImage(t *testing.T, workspaceDir string, branch *string) error {
	t.Helper()

	// GIT_SSL_NO_VERIFY: harmless here (the origin is either the real
	// self-signed-TLS test server or a deliberately-unreachable URL,
	// neither ever completes a real TLS handshake that verification would
	// affect either way) -- kept for parity with this package's other
	// boot-sequence tests.
	t.Setenv("GIT_SSL_NO_VERIFY", "true")

	sup := supervisor.New()

	cfg := boot.Config{
		BootMode:           sandboxboot.BootModeRepoImage,
		WorkspaceDir:       workspaceDir,
		CredentialCacheDir: t.TempDir(),
		// §30.5: see bootsequence_cleanbuild_integration_test.go's
		// identical comment -- gitclone.SyncAll's own real lchown needs a
		// self-uid/gid Credential to run unprivileged.
		RuntimeUID: uint32(os.Getuid()),
		RuntimeGID: uint32(os.Getgid()),
		SessionConfig: &sessionconfig.SessionConfig{
			BootMode:          sessionconfig.SessionConfigBootModeRepoImage,
			ControlPlaneWsUrl: "wss://unused.invalid/ws",
			Gen:               1,
			Repos: []sessionconfig.SessionConfigReposElem{
				{Name: "repo1", Url: "https://example.invalid/repo1.git", Branch: branch},
			},
			SandboxId:    "5b1c1e2e-6b1a-4b1a-9b1a-6b1a4b1a9b1a",
			SandboxToken: "test-sandbox-token",
			SessionId:    "resilience-fetch-fail-session",
		},
	}

	ctx, cancel := context.WithTimeout(context.Background(), bootSequenceTestTimeout)
	defer cancel()

	noopProgress := func(services.BootProgressEvent) {}
	noopGitSync := func(string, string, string) {}
	noopFetchTiming := func(string, float64, bool) {}
	noopCheckoutTiming := func(string, float64, bool) {}
	noopHookTiming := func(string, string, string, bool, bool, float64) {}

	return runBootSequence(ctx, sup, cfg, gitdir.Layout{Root: t.TempDir(), WorkspaceDir: cfg.WorkspaceDir}, nil, platform.DefaultTimeouts(), nil, nil, noopProgress, noopGitSync, noopFetchTiming, noopCheckoutTiming, noopHookTiming)
}

// TestResilienceScenario_FetchFailBoot_InventedBranch_DegradesAndBootSucceeds
// proves the non-fatal half of §19.3's own degrade policy through a REAL,
// FULL boot sequence: the session names no explicit branch (repos[].branch
// == nil, so SyncAll invents "narvi/<sessionID>" -- "acceptable from HEAD"
// per §19.3), the workspace's own origin is unreachable, so the boot-time
// fetch fails outright -- but the WHOLE boot sequence still succeeds,
// proceeding on stale (baked) image state, exactly as §19.3 requires ("warm
// boot must never become network-dependent for liveness").
func TestResilienceScenario_FetchFailBoot_InventedBranch_DegradesAndBootSucceeds(t *testing.T) {
	workspaceDir := t.TempDir()
	bakeExistingRepoImageWorkspace(t, workspaceDir)

	repoDir := filepath.Join(workspaceDir, "repo1")
	mustRunGit(t, repoDir, "remote", "set-url", "origin", unreachableOriginURL)

	bootErr := runBootSequenceRepoImage(t, workspaceDir, nil)
	if bootErr != nil {
		t.Fatalf("runBootSequence() error = %v, want nil (an invented branch's own fetch failure must degrade, never fail the boot)", bootErr)
	}

	// The workspace must still be genuinely usable: the baked content is
	// there, and a real (invented) branch is checked out -- this is a full,
	// working boot on stale image state, not a half-broken one.
	if _, err := os.Stat(filepath.Join(repoDir, "README.md")); err != nil {
		t.Errorf("baked README.md missing after degraded boot: %v", err)
	}
	branch := gitOutput(t, repoDir, "rev-parse", "--abbrev-ref", "HEAD")
	if branch == "HEAD" || branch == "" {
		t.Errorf("checked-out branch after degraded boot = %q, want a real, named branch (not detached HEAD)", branch)
	}

	// AGENTS.md must still have been generated -- proof the rest of the
	// boot sequence (not just SyncAll) genuinely ran to completion.
	if _, err := os.Stat(filepath.Join(workspaceDir, "AGENTS.md")); err != nil {
		t.Errorf("AGENTS.md missing after degraded boot: %v", err)
	}
}

// TestResilienceScenario_FetchFailBoot_ExplicitBranchNeitherLocalNorFetchable_FatalBoot
// proves the non-negotiable OTHER half of §19.3's own degrade policy
// through the same real, full boot sequence: a session that EXPLICITLY
// names a branch neither already local nor fetchable (origin
// unreachable) must FAIL the boot outright for its primary repo, rather
// than let checkoutBranch's own HEAD fallback silently fork a same-named
// branch at a stale base -- §19.3's own words: "Silently forking a
// same-named branch at a stale base is the one outcome that must never
// happen; this rule is non-negotiable in review."
func TestResilienceScenario_FetchFailBoot_ExplicitBranchNeitherLocalNorFetchable_FatalBoot(t *testing.T) {
	workspaceDir := t.TempDir()
	bakeExistingRepoImageWorkspace(t, workspaceDir)

	repoDir := filepath.Join(workspaceDir, "repo1")
	mustRunGit(t, repoDir, "remote", "set-url", "origin", unreachableOriginURL)

	explicitBranch := "feature-branch-nowhere"
	bootErr := runBootSequenceRepoImage(t, workspaceDir, &explicitBranch)
	if bootErr == nil {
		t.Fatal("runBootSequence() error = nil, want a fatal error (explicit branch is neither local nor fetchable -- must never silently fork at a stale base)")
	}

	// The dangerous outcome this rule exists to prevent: a NEW local
	// branch, coincidentally named exactly like the requested one, forked
	// silently at the stale baked HEAD.
	branches := gitOutput(t, repoDir, "branch", "--list", explicitBranch)
	if branches != "" {
		t.Errorf("branch %q was created locally despite a fatal fetch failure -- this is EXACTLY the silent-fork-at-stale-base bug §19.3 exists to prevent (branch --list output: %q)", explicitBranch, branches)
	}
}
