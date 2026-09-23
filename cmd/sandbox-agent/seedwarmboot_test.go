// This file proves seedWarmBootRepos' own criticality policy directly, at
// unit-test granularity, without booting run() itself (which blocks on OS
// signals / a live WS bridge / a real opencode spawn -- see main_test.go's
// own doc comment for the identical reasoning). seedWarmBootRepos was
// factored out of run()'s own pre-fingerprint gitdir.Seed loop specifically
// so this policy is testable in isolation (see its own doc comment,
// main.go).
package main

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/narvidev/narvi/contracts/gen/go/sessionconfig"
	"github.com/narvidev/narvi/internal/platform"
	"github.com/narvidev/narvi/internal/sandboxagent/boot"
	"github.com/narvidev/narvi/internal/sandboxagent/gitdir"
	"github.com/narvidev/narvi/internal/sandboxagent/supervisor"
)

// seedWarmBootTestGoodRepo creates a real, on-disk git checkout at
// workspaceDir/name that gitdir.Seed accepts cleanly.
func seedWarmBootTestGoodRepo(t *testing.T, workspaceDir, name string) {
	t.Helper()
	initRealGitRepoForPushTest(t, filepath.Join(workspaceDir, name))
}

// seedWarmBootTestBadRepo creates a real, on-disk git checkout at
// workspaceDir/name, then plants a .git/shallow marker -- exactly the
// on-disk layout the adversarial review reproduced with real git
// (`git fetch --depth=1`) and that gitdir.Seed refuses outright
// (unsupportedGitDirEntries), so gitdir.Seed(...) is guaranteed to fail
// for this repo.
func seedWarmBootTestBadRepo(t *testing.T, workspaceDir, name string) {
	t.Helper()
	dir := filepath.Join(workspaceDir, name)
	initRealGitRepoForPushTest(t, dir)
	if err := os.WriteFile(filepath.Join(dir, ".git", "shallow"), []byte("deadbeef\n"), 0o644); err != nil {
		t.Fatalf("write .git/shallow: %v", err)
	}
}

// TestSeedWarmBootRepos_PrimaryFailureIsFatal proves the loop still refuses
// to boot when the PRIMARY (position 0) repo's Seed call fails -- matching
// gitclone.SyncAll's own identical policy (sync.go: "position 0 =
// primary" -- a primary failure is fatal).
func TestSeedWarmBootRepos_PrimaryFailureIsFatal(t *testing.T) {
	t.Parallel()

	workspaceDir := t.TempDir()
	seedWarmBootTestBadRepo(t, workspaceDir, "bad-primary")

	cfg := boot.Config{
		WorkspaceDir: workspaceDir,
		SessionConfig: &sessionconfig.SessionConfig{
			Repos: []sessionconfig.SessionConfigReposElem{
				{Name: "bad-primary", Url: "https://example.invalid/bad-primary.git"},
			},
		},
	}
	layout := gitdir.Layout{Root: t.TempDir(), WorkspaceDir: workspaceDir}
	if err := gitdir.EnsureRoot(layout.Root); err != nil {
		t.Fatalf("gitdir.EnsureRoot() error = %v", err)
	}

	seeded, _, err := seedWarmBootRepos(context.Background(), supervisor.New(), cfg, layout, nil,
		platform.DefaultTimeouts().GitSyncStepTimeout, platform.DefaultTimeouts().ProcessStopGracePeriod)
	if err == nil {
		t.Fatal("seedWarmBootRepos() error = nil, want a fatal error for the failed primary repo's Seed call")
	}
	if len(seeded) != 0 {
		t.Errorf("seedWarmBootRepos() seeded = %v, want empty (nothing was seeded before the primary's own Seed call failed)", seeded)
	}
}

// TestSeedWarmBootRepos_SecondaryFailureContinues proves the fix for the
// finding directly: before it, ANY repo's Seed failure -- including a
// SECONDARY one -- returned fatally from run(), bypassing the exact
// primary/secondary policy gitclone.SyncAll's own later per-repo Seed call
// implements. A secondary repo's Seed failure must now be a logged
// warning, and the loop must still reach and seed every repo after it, so
// run() can go on to log the §5.3 boot fingerprint and bring the bridge
// up.
func TestSeedWarmBootRepos_SecondaryFailureContinues(t *testing.T) {
	t.Parallel()

	workspaceDir := t.TempDir()
	seedWarmBootTestGoodRepo(t, workspaceDir, "primary")
	seedWarmBootTestBadRepo(t, workspaceDir, "bad-secondary")
	seedWarmBootTestGoodRepo(t, workspaceDir, "later")

	cfg := boot.Config{
		WorkspaceDir: workspaceDir,
		SessionConfig: &sessionconfig.SessionConfig{
			Repos: []sessionconfig.SessionConfigReposElem{
				{Name: "primary", Url: "https://example.invalid/primary.git"},
				{Name: "bad-secondary", Url: "https://example.invalid/bad-secondary.git"},
				{Name: "later", Url: "https://example.invalid/later.git"},
			},
		},
	}
	layout := gitdir.Layout{Root: t.TempDir(), WorkspaceDir: workspaceDir}
	if err := gitdir.EnsureRoot(layout.Root); err != nil {
		t.Fatalf("gitdir.EnsureRoot() error = %v", err)
	}

	seeded, warnings, err := seedWarmBootRepos(context.Background(), supervisor.New(), cfg, layout, nil,
		platform.DefaultTimeouts().GitSyncStepTimeout, platform.DefaultTimeouts().ProcessStopGracePeriod)
	if err != nil {
		t.Fatalf("seedWarmBootRepos() error = %v, want nil (a secondary repo's Seed failure is a warning, not fatal)", err)
	}
	if len(warnings) != 1 || warnings[0].repo != "bad-secondary" {
		t.Errorf("seedWarmBootRepos() warnings = %+v, want exactly one warning naming bad-secondary", warnings)
	}
	if !seeded["primary"] || !seeded["later"] || seeded["bad-secondary"] {
		t.Errorf("seedWarmBootRepos() seeded = %v, want {primary, later} but NOT bad-secondary", seeded)
	}

	// "later" (position 2, after the failed secondary) must still have
	// been reached and seeded -- proving the loop did not stop at the
	// secondary failure the way it would have before this fix.
	laterGitDir := layout.Repo("later").GitDir
	if _, statErr := os.Stat(filepath.Join(laterGitDir, "config")); statErr != nil {
		t.Errorf("later repo's agent git-dir config missing (stat error = %v) -- "+
			"the loop must continue seeding repos after a secondary failure", statErr)
	}
}
